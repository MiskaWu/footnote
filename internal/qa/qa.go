// Package qa 回答一個問題：展開別名 → 混合檢索 → 重排序 → 閘門 A（沒過時翻譯補查）→ 決定回覆內容；文件段落交給模型整理。
package qa

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/glossary"
	"github.com/miskawu/footnote/internal/ingest"
	"github.com/miskawu/footnote/internal/llm"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
	"github.com/miskawu/footnote/internal/textseg"
)

const (
	StatusFAQ    = "faq"
	StatusAnswer = "answer" // 模型依段落整理的回答
	StatusDocs   = "docs"
	StatusNoData = "no_data"

	nearMissCount = 3
	ftsK          = 40
)

type Searcher interface {
	HybridSearch(ctx context.Context, vec []float32, fts string, k, limit int, prefixes []string) ([]store.Candidate, error)
	GetFAQ(ctx context.Context, project, path string) (*store.FAQ, error)
}

type Embedder interface {
	Embed(ctx context.Context, texts []string, kind tei.EmbedKind) ([][]float32, error)
}

type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]float32, error)
}

type Hit struct {
	store.Candidate
	Score   float32
	Excerpt string // 片段裡跟問題最相關的幾行（原文）；空字串表示沒挑，回覆時取開頭
}

type Result struct {
	Status     string
	Reason     string // no_data 時：gate_a、gate_follow、gate_b（模型判斷沒有）、gate_c（引用對不上）
	Query      string
	QueryVec   []float32
	Top        float32
	Follow     *float32    // 討論串追問時本題單獨的分數（FollowScore）；不是追問為 nil
	Answer     *llm.Answer // 有呼叫模型時的回答與用量（降級時也留著，用量要記）
	GenSkipped string      // 該整理卻沒整理的原因：budget、error、refusal、max_tokens
	Hits       []Hit       // 回覆要列的：FAQ 一張卡、最多 k_reply 個文件段落，或送給模型的段落（answer）
	NearMisses []Hit       // 前幾名，不論分數（no_data 回覆與缺口用）
	FAQ        *store.FAQ  // Status=faq 時
	Ranked     []Hit       // 全部重排結果（search／eval 用）
	Rescue     *Rescue     // 閘門 A 沒過、試過翻譯補查時的紀錄；沒試為 nil
	Latency    time.Duration

	history  []string // 產生這份結果的問題（已展開別名；補查成功時是改寫後的），整理回答用
	question string
}

// History 與 Question 是產生這份結果的問題（已展開別名；補查成功時是改寫後的）。
func (r *Result) History() []string { return r.history }
func (r *Result) Question() string  { return r.question }

// Rescue 是翻譯補查的紀錄：閘門 A 沒過時請模型把外文、口語換成文件說法，再查一次。
// 補查通過就回補查的結果（Rescued），沒通過就保留第一次的結果（缺口的去重與最接近的段落照舊）。
type Rescue struct {
	Query   string       // 改寫後拿去檢索的字串；沒查成（Skipped）時是空字串
	Top     float32      // 補查的最高分
	Rescued bool         // 補查通過閘門，回覆用的是補查的結果
	Skipped string       // 沒查成的原因：budget（本月預算用完）、error（呼叫失敗或結果不能用）、unchanged（模型沒有補任何字）
	Usage   *llm.Rewrite // 呼叫用量；呼叫失敗時可能是 nil
}

// Aliases 提供別名詞彙表的叫法組（實作是 store.Glossary）。
type Aliases interface {
	Glossary(ctx context.Context) ([][]string, error)
}

// Rewriter 是翻譯補查的改寫（實作是 llm.Rewriter）：輸入討論串前面的問題加本題，一行對一行改寫；
// titles 是知識庫的文件名稱，讓模型照文件裡的系統名稱改寫。
type Rewriter interface {
	Rewrite(ctx context.Context, lines, titles []string) (*llm.Rewrite, error)
}

// Titles 提供索引裡的文件路徑（實作是 store.DocPaths），補查時整理成文件名稱交給模型當用詞參考。
type Titles interface {
	DocPaths(ctx context.Context) ([]string, error)
}

var titlePrefix = regexp.MustCompile(`^[0-9]+_`)

// DocTitles 把文件路徑整理成名稱：檔名去掉副檔名與開頭的編號（specs/02_部署流程.xlsx ⇒ 部署流程），去重。
func DocTitles(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		t := path.Base(p)
		t = strings.TrimSpace(titlePrefix.ReplaceAllString(strings.TrimSuffix(t, path.Ext(t)), ""))
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

type Engine struct {
	Store    Searcher
	Embedder Embedder
	Reranker Reranker
	Cfg      config.QA
	Log      *slog.Logger // 可為 nil
	Gen      Generator    // nil 表示不整理回答，照舊回原文摘錄
	Budget   Budget       // nil 表示不限
	Aliases  Aliases      // nil 表示不展開別名
	Rewriter Rewriter     // nil 表示不做翻譯補查
	Titles   Titles       // nil 表示補查時不給文件名稱
}

// BuildQuery 把同一討論串的前幾個問題串在本問題前面。
func BuildQuery(history []string, question string) string {
	parts := append(append([]string{}, history...), question)
	return strings.Join(parts, "\n")
}

// Glossary 讀目前的詞彙表。每題讀一次資料庫（一列、不到 1ms），同步改了詞彙表下一題就生效，
// serve 與 CLI 不必各自快取、也不會不同步。沒設 Aliases 時回傳 nil，Expand 原樣回傳。
func (e *Engine) Glossary(ctx context.Context) (*glossary.Glossary, error) {
	if e.Aliases == nil {
		return nil, nil
	}
	groups, err := e.Aliases.Glossary(ctx)
	if err != nil {
		return nil, err
	}
	return glossary.New(groups), nil
}

// Run 跑完整線上流程。送進模型的文字（檢索、重排、追問、挑摘錄、整理回答）都先展開別名；
// 閘門 A 沒過時做一次翻譯補查。Result.Query 是產生這份結果、實際拿去檢索的字串。
func (e *Engine) Run(ctx context.Context, question string, history, prefixes []string) (*Result, error) {
	start := time.Now()
	g, err := e.Glossary(ctx)
	if err != nil {
		return nil, err
	}
	r, err := e.Search(ctx, g, history, question, prefixes)
	if err != nil {
		return nil, err
	}
	if r.Status == StatusDocs && e.Gen != nil {
		e.generate(ctx, r, GenQuestion(r.history, r.question))
	}
	if r.Status == StatusDocs && e.Cfg.ExcerptChars > 0 {
		// 挑摘錄失敗不影響回答，回覆時退回取片段開頭
		if err := e.pickExcerpts(ctx, r.Query, r.Hits); err != nil && e.Log != nil {
			e.Log.Warn("pick excerpts failed; falling back to chunk start", "err", err)
		}
	}
	if r.Status == StatusFAQ {
		f, err := e.Store.GetFAQ(ctx, r.Hits[0].ProjectPath, r.Hits[0].Path)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		r.FAQ = f
	}
	r.Latency = time.Since(start)
	return r, nil
}

// Search 是 Attempt 加上沒過閘門 A 時的翻譯補查：線上決定回覆內容之前走的那一段。
// g 由呼叫端給：審核詞彙表提案時要用還沒上線的版本跑，量到的才是核准之後的行為（eval.CheckAlias）。
func (e *Engine) Search(ctx context.Context, g *glossary.Glossary, history []string, question string, prefixes []string) (*Result, error) {
	r, err := e.Attempt(ctx, g, history, question, prefixes)
	if err != nil {
		return nil, err
	}
	if r.Status == StatusNoData && e.Rewriter != nil { // 這時的 no_data 只可能來自閘門 A（含追問）
		return e.rescue(ctx, r, g, prefixes)
	}
	return r, nil
}

// Attempt 是一次「展開別名、檢索、重排、追問分數、閘門 A」。線上（Run 與翻譯補查）和評估走同一條，
// 評估量到的就是機器人實際的行為。history 與 question 各自展開：本題自己也要帶上別名
// （追問分數與整理回答只看本題）；展開重複做結果不變，傳進來的已經展開過也沒關係。
func (e *Engine) Attempt(ctx context.Context, g *glossary.Glossary, history []string, question string, prefixes []string) (*Result, error) {
	hist := make([]string, len(history))
	for i, h := range history {
		hist[i] = g.Expand(h)
	}
	history, question = hist, g.Expand(question)
	query := BuildQuery(history, question)
	ranked, vec, err := e.retrieve(ctx, query, prefixes)
	if err != nil {
		return nil, err
	}
	var follow *float32
	if len(history) > 0 {
		s, err := e.followScore(ctx, question, ranked)
		if err != nil {
			return nil, err
		}
		follow = &s
	}
	r := Decide(ranked, follow, e.Cfg)
	r.Query, r.QueryVec, r.Ranked = query, vec, ranked
	r.history, r.question = history, question
	return r, nil
}

// rescue 是翻譯補查：改寫後再跑一次 attempt。通過就回補查的結果，否則保留第一次的。
// 改寫失敗、預算用完都不算錯誤：照第一次的結果回 no_data（這題本來就沒過閘門 A）。
// 補查通過後照常交給模型整理回答，所以還要再過閘門 B、C。
func (e *Engine) rescue(ctx context.Context, first *Result, g *glossary.Glossary, prefixes []string) (*Result, error) {
	rs := &Rescue{}
	first.Rescue = rs
	if e.Budget != nil {
		if ok, err := e.Budget.Allow(ctx); err != nil || !ok {
			rs.Skipped = "budget"
			e.warn("rescue skipped", "reason", "budget", "err", err)
			return first, nil
		}
	}
	hist, q, usage, err := e.RewriteQuestion(ctx, first.history, first.question)
	rs.Usage = usage
	switch {
	case err != nil:
		rs.Skipped = "error"
		e.warn("rescue rewrite failed", "err", err)
		return first, nil
	case q == first.question && slices.Equal(hist, first.history):
		rs.Skipped = "unchanged"
		return first, nil
	}
	r, err := e.Attempt(ctx, g, hist, q, prefixes)
	if err != nil {
		return nil, err
	}
	rs.Query, rs.Top = r.Query, r.Top
	if r.Status == StatusNoData {
		return first, nil
	}
	rs.Rescued = true
	r.Rescue = rs
	return r, nil
}

// RewriteQuestion 請模型改寫（討論串前面的問題加本題，一行對一行），回傳改寫後的前面問題與本題。
// 線上的補查與評估共用這一條；改寫結果交給 Attempt，模型補的說法剛好在詞彙表裡時也會再展開。
func (e *Engine) RewriteQuestion(ctx context.Context, history []string, question string) ([]string, string, *llm.Rewrite, error) {
	var titles []string
	if e.Titles != nil {
		paths, err := e.Titles.DocPaths(ctx)
		if err != nil {
			return nil, "", nil, err
		}
		titles = DocTitles(paths)
	}
	rw, err := e.Rewriter.Rewrite(ctx, append(append([]string{}, history...), question), titles)
	if err != nil {
		return nil, "", rw, err
	}
	return rw.Lines[:len(rw.Lines)-1], rw.Lines[len(rw.Lines)-1], rw, nil
}

// Retrieve 展開別名後做混合檢索與重排序，回傳依重排分數排序的結果與查詢向量（eval、search 用；線上走 Run）。
func (e *Engine) Retrieve(ctx context.Context, query string, prefixes []string) ([]Hit, []float32, error) {
	g, err := e.Glossary(ctx)
	if err != nil {
		return nil, nil, err
	}
	return e.retrieve(ctx, g.Expand(query), prefixes)
}

// retrieve 的 query 已展開別名。
func (e *Engine) retrieve(ctx context.Context, query string, prefixes []string) ([]Hit, []float32, error) {
	vecs, err := e.Embedder.Embed(ctx, []string{query}, tei.Query)
	if err != nil {
		return nil, nil, err
	}
	cands, err := e.Store.HybridSearch(ctx, vecs[0], textseg.Query(query), ftsK, e.Cfg.Candidates, prefixes)
	if err != nil {
		return nil, nil, err
	}
	docs := make([]string, len(cands))
	for i, c := range cands {
		docs[i] = RerankText(c)
	}
	scores, err := e.Reranker.Rerank(ctx, query, docs)
	if err != nil {
		return nil, nil, err
	}
	hits := make([]Hit, len(cands))
	for i, c := range cands {
		hits[i] = Hit{Candidate: c, Score: scores[i]}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits, vecs[0], nil
}

// RerankText 是送去重排序的文字，與嵌入用的前綴一致。
func RerankText(c store.Candidate) string {
	return ingest.EmbedText(c.ProjectPath, c.Path, c.Heading, c.Content)
}

// followCheck：追問時拿本題單獨重排的名次數（取檢索結果的前幾名）。
func (e *Engine) followCheck() int { return max(e.Cfg.KReply, 1) }

// FollowScore 用本題單獨（不串前一題）對檢索結果前幾名重排，回傳最高分。
// 追問的檢索字串混了前一題，最高分可能全來自前一題；這個分數才看得出本題自己有沒有找到東西。
func (e *Engine) FollowScore(ctx context.Context, question string, ranked []Hit) (float32, error) {
	g, err := e.Glossary(ctx)
	if err != nil {
		return 0, err
	}
	return e.followScore(ctx, g.Expand(question), ranked)
}

// followScore 的 question 已展開別名。
func (e *Engine) followScore(ctx context.Context, question string, ranked []Hit) (float32, error) {
	n := min(e.followCheck(), len(ranked))
	if n == 0 {
		return 0, nil
	}
	docs := make([]string, n)
	for i := range docs {
		docs[i] = RerankText(ranked[i].Candidate)
	}
	scores, err := e.Reranker.Rerank(ctx, question, docs)
	if err != nil {
		return 0, err
	}
	var best float32
	for _, s := range scores {
		best = max(best, s)
	}
	return best, nil
}

// Gate 是閘門 A 的判斷，線上（Decide）與評估（eval）共用同一份。top 是最高重排分數；
// follow 是討論串追問時本題單獨的分數（FollowScore），不是追問時為 nil。
func Gate(top float32, follow *float32, tauAnswer, tauFollow float64) (pass bool, reason string) {
	if float64(top) < tauAnswer {
		return false, "gate_a"
	}
	if follow != nil && float64(*follow) < tauFollow {
		return false, "gate_follow"
	}
	return true, ""
}

// Decide 是閘門 A 與回覆組裝的決策（純函式，方便測試）。ranked 須已依分數由高到低排序。
func Decide(ranked []Hit, follow *float32, cfg config.QA) *Result {
	r := &Result{Follow: follow}
	r.NearMisses = ranked[:min(nearMissCount, len(ranked))]
	if len(ranked) == 0 {
		r.Status, r.Reason = StatusNoData, "gate_a"
		return r
	}
	r.Top = ranked[0].Score
	if ok, reason := Gate(r.Top, follow, cfg.TauAnswer, cfg.TauFollow); !ok {
		r.Status, r.Reason = StatusNoData, reason
		return r
	}
	if ranked[0].Kind == "faq" {
		r.Status, r.Hits = StatusFAQ, ranked[:1]
		return r
	}
	r.Status = StatusDocs
	for _, h := range ranked {
		if len(r.Hits) >= cfg.KReply || float64(h.Score) < cfg.TauKeep {
			break
		}
		if h.Kind == "faq" { // 文件段落回覆裡不混 FAQ 卡（FAQ 只在排第一時整張回）
			continue
		}
		r.Hits = append(r.Hits, h)
	}
	r.Hits = newerFirst(r.Hits)
	return r
}

// newerFirst：回覆裡同時有某份舊版與它的新版時，把新版移到舊版前面；其餘照分數排。
// 新版沒進回覆時不另外補進來——它分數不夠，代表這題的內容可能只寫在舊版。
func newerFirst(hits []Hit) []Hit {
	out := make([]Hit, 0, len(hits))
	used := make([]bool, len(hits))
	for i, h := range hits {
		if used[i] {
			continue
		}
		if h.SupersededBy != "" {
			for j := i + 1; j < len(hits); j++ {
				if !used[j] && hits[j].ProjectPath == h.ProjectPath && hits[j].Path == h.SupersededBy {
					out, used[j] = append(out, hits[j]), true
				}
			}
		}
		out, used[i] = append(out, h), true
	}
	return out
}

const excerptRows = 4

// pickExcerpts 用重排序模型在每個要列出的片段裡挑跟問題最相關的幾行。
// xlsx 的片段一塊裝了幾十列，答案常在中間，取開頭的話使用者看不出來。
func (e *Engine) pickExcerpts(ctx context.Context, query string, hits []Hit) error {
	var texts []string
	var owner []int
	var spans []Window
	for i, h := range hits {
		for _, w := range Windows(h.Content, e.Cfg.ExcerptChars, excerptRows) {
			texts = append(texts, w.Text)
			owner = append(owner, i)
			spans = append(spans, w)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	scores, err := e.Reranker.Rerank(ctx, query, texts)
	if err != nil {
		return err
	}
	best := make([]int, len(hits))
	for i := range best {
		best[i] = -1
	}
	for j, sc := range scores {
		if b := best[owner[j]]; b < 0 || sc > scores[b] {
			best[owner[j]] = j
		}
	}
	for i, j := range best {
		if j >= 0 {
			hits[i].Excerpt = spans[j].Display()
		}
	}
	return nil
}

// Window 是片段裡連續的幾行。
type Window struct {
	Text        string
	First, Last bool // 是否從第一行開始、是否到最後一行
}

// Display 是回覆裡顯示的摘錄：前後還有內容時補上「…」。
func (w Window) Display() string {
	s := w.Text
	if !w.First {
		s = "…\n" + s
	}
	if !w.Last {
		s += "\n…"
	}
	return s
}

// Windows 把片段切成最多 rows 行、不超過 maxChars 字的窗口，每隔 rows/2 行起一個。
// 單行超過 maxChars 時截斷該行。
func Windows(content string, maxChars, rows int) []Window {
	var lines []string
	for _, l := range strings.Split(content, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	step := max(1, rows/2)
	var out []Window
	for start := 0; start < len(lines); start += step {
		var parts []string
		size, end := 0, start
		for end < len(lines) && len(parts) < rows {
			n := len([]rune(lines[end]))
			if len(parts) > 0 && size+n > maxChars {
				break
			}
			line := lines[end]
			if n > maxChars {
				line = string([]rune(line)[:maxChars]) + "…"
				n = maxChars
			}
			parts = append(parts, line)
			size += n
			end++
		}
		out = append(out, Window{Text: strings.Join(parts, "\n"), First: start == 0, Last: end == len(lines)})
		if end == len(lines) {
			break
		}
	}
	return out
}
