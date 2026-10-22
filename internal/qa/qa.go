// Package qa 回答一個問題：混合檢索 → 重排序 → 閘門 A → 決定回覆內容。不呼叫任何 LLM（plan §4）。
package qa

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/ingest"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
	"github.com/miskawu/footnote/internal/textseg"
)

const (
	StatusFAQ    = "faq"
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
	Reason     string // no_data 時：gate_a
	Query      string
	QueryVec   []float32
	Top        float32
	Hits       []Hit      // 回覆要列的：FAQ 一張卡，或最多 k_reply 個文件段落
	NearMisses []Hit      // 前幾名，不論分數（no_data 回覆與缺口用）
	FAQ        *store.FAQ // Status=faq 時
	Ranked     []Hit      // 全部重排結果（search／eval 用）
	Latency    time.Duration
}

type Engine struct {
	Store    Searcher
	Embedder Embedder
	Reranker Reranker
	Cfg      config.QA
	Log      *slog.Logger // 可為 nil
}

// BuildQuery 把同一討論串的前幾個問題串在本問題前面（plan §7.3 第 1 步）。
func BuildQuery(history []string, question string) string {
	parts := append(append([]string{}, history...), question)
	return strings.Join(parts, "\n")
}

// Run 跑完整線上流程。
func (e *Engine) Run(ctx context.Context, question string, history, prefixes []string) (*Result, error) {
	start := time.Now()
	query := BuildQuery(history, question)
	ranked, vec, err := e.Retrieve(ctx, query, prefixes)
	if err != nil {
		return nil, err
	}
	r := Decide(ranked, e.Cfg)
	r.Query, r.QueryVec, r.Ranked = query, vec, ranked
	if r.Status == StatusDocs && e.Cfg.ExcerptChars > 0 {
		// 挑摘錄失敗不影響回答，回覆時退回取片段開頭
		if err := e.pickExcerpts(ctx, query, r.Hits); err != nil && e.Log != nil {
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

// Retrieve 做混合檢索與重排序，回傳依重排分數排序的結果與查詢向量。
func (e *Engine) Retrieve(ctx context.Context, query string, prefixes []string) ([]Hit, []float32, error) {
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

// Decide 是閘門 A 與回覆組裝的決策（純函式，方便測試）。ranked 須已依分數由高到低排序。
func Decide(ranked []Hit, cfg config.QA) *Result {
	r := &Result{}
	r.NearMisses = ranked[:min(nearMissCount, len(ranked))]
	if len(ranked) == 0 {
		r.Status, r.Reason = StatusNoData, "gate_a"
		return r
	}
	r.Top = ranked[0].Score
	if float64(r.Top) < cfg.TauAnswer {
		r.Status, r.Reason = StatusNoData, "gate_a"
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
	return r
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
