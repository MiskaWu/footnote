package qa

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/llm"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
)

var cfg = config.QA{Candidates: 30, KReply: 3, TauAnswer: 0.35, TauKeep: 0.15}

func hit(id int64, kind string, score float32) Hit {
	return Hit{Candidate: store.Candidate{ID: id, Kind: kind, ProjectPath: "p", Path: "f.md"}, Score: score}
}

func TestDecideGateABlocks(t *testing.T) {
	r := Decide([]Hit{hit(1, "doc", 0.34), hit(2, "doc", 0.2)}, nil, cfg)
	if r.Status != StatusNoData || r.Reason != "gate_a" || len(r.Hits) != 0 {
		t.Fatalf("got %+v", r)
	}
	if len(r.NearMisses) != 2 {
		t.Errorf("near misses = %d", len(r.NearMisses))
	}
}

func TestDecideEmpty(t *testing.T) {
	if r := Decide(nil, nil, cfg); r.Status != StatusNoData {
		t.Fatalf("got %+v", r)
	}
}

func TestDecideDocsRespectsTauKeepAndKReply(t *testing.T) {
	r := Decide([]Hit{hit(1, "doc", 0.9), hit(2, "doc", 0.5), hit(3, "doc", 0.14), hit(4, "doc", 0.13)}, nil, cfg)
	if r.Status != StatusDocs || len(r.Hits) != 2 {
		t.Fatalf("got status=%s hits=%d", r.Status, len(r.Hits))
	}
	r = Decide([]Hit{hit(1, "doc", 0.9), hit(2, "doc", 0.8), hit(3, "doc", 0.7), hit(4, "doc", 0.6)}, nil, cfg)
	if len(r.Hits) != 3 {
		t.Fatalf("k_reply not applied: %d", len(r.Hits))
	}
}

func TestDecideFAQOnTop(t *testing.T) {
	r := Decide([]Hit{hit(1, "faq", 0.8), hit(2, "doc", 0.7)}, nil, cfg)
	if r.Status != StatusFAQ || len(r.Hits) != 1 || r.Hits[0].ID != 1 {
		t.Fatalf("got %+v", r)
	}
}

func TestDecideFAQNotMixedIntoDocs(t *testing.T) {
	r := Decide([]Hit{hit(1, "doc", 0.8), hit(2, "faq", 0.7), hit(3, "doc", 0.6)}, nil, cfg)
	if r.Status != StatusDocs || len(r.Hits) != 2 || r.Hits[1].ID != 3 {
		t.Fatalf("got %+v", r.Hits)
	}
}

func TestDecideNewerVersionFirst(t *testing.T) {
	ver := func(id int64, p, newer string, score float32) Hit {
		h := hit(id, "doc", score)
		h.Path, h.SupersededBy = p, newer
		return h
	}
	// 分數順序：舊版、更舊版、新版 ⇒ 新版移到最前面，兩份舊版維持原本順序
	r := Decide([]Hit{ver(1, "09_權限系統.xlsx", "09_權限系統_調整版.xlsx", 0.9), ver(2, "00_權限.xlsx", "09_權限系統_調整版.xlsx", 0.8), ver(3, "09_權限系統_調整版.xlsx", "", 0.7)}, nil, cfg)
	if len(r.Hits) != 3 || r.Hits[0].ID != 3 || r.Hits[1].ID != 1 || r.Hits[2].ID != 2 {
		t.Fatalf("got %+v, want ids 3, 1, 2", r.Hits)
	}
	// 新版沒進回覆（分數低於 τ_keep）就不補，舊版照常列出
	r = Decide([]Hit{ver(1, "09_權限系統.xlsx", "09_權限系統_調整版.xlsx", 0.9), ver(3, "09_權限系統_調整版.xlsx", "", 0.1)}, nil, cfg)
	if len(r.Hits) != 1 || r.Hits[0].ID != 1 {
		t.Fatalf("got %+v", r.Hits)
	}
}

// ---- Engine 用假實作 ----

type fakeStore struct {
	cands []store.Candidate
	faq   *store.FAQ
	fts   []string // 收到的全文檢索字串
}

func (f *fakeStore) HybridSearch(ctx context.Context, vec []float32, fts string, k, limit int, prefixes []string) ([]store.Candidate, error) {
	f.fts = append(f.fts, fts)
	return f.cands, nil
}
func (f *fakeStore) GetFAQ(ctx context.Context, project, path string) (*store.FAQ, error) {
	if f.faq == nil {
		return nil, store.ErrNotFound
	}
	return f.faq, nil
}

type fakeEmbed struct{ got []string }

func (f *fakeEmbed) Embed(ctx context.Context, texts []string, kind tei.EmbedKind) ([][]float32, error) {
	f.got = append(f.got, texts...)
	return [][]float32{{1, 0}}, nil
}

type fakeRerank map[string]float32 // content → score

func (f fakeRerank) Rerank(ctx context.Context, query string, docs []string) ([]float32, error) {
	out := make([]float32, len(docs))
	for i, d := range docs {
		for k, v := range f {
			if strings.HasSuffix(d, k) {
				out[i] = v
			}
		}
	}
	return out, nil
}

func TestEngineRerankOrdersAndFetchesFAQ(t *testing.T) {
	st := &fakeStore{
		cands: []store.Candidate{
			{ID: 1, Kind: "doc", Content: "低分"},
			{ID: 2, Kind: "faq", Content: "高分卡片", ProjectPath: "kb/faq", Path: "a.md"},
		},
		faq: &store.FAQ{Path: "a.md"},
	}
	emb := &fakeEmbed{}
	e := &Engine{Store: st, Embedder: emb, Reranker: fakeRerank{"低分": 0.4, "高分卡片": 0.9}, Cfg: cfg}
	r, err := e.Run(context.Background(), "怎麼回滾", []string{"部署流程是什麼"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusFAQ || r.Hits[0].ID != 2 || r.FAQ == nil {
		t.Fatalf("got %+v", r)
	}
	if emb.got[0] != "部署流程是什麼\n怎麼回滾" {
		t.Errorf("follow-up query = %q", emb.got[0])
	}
}

func TestGate(t *testing.T) {
	f := func(v float32) *float32 { return &v }
	for _, tc := range []struct {
		top    float32
		follow *float32
		pass   bool
		reason string
	}{
		{0.9, nil, true, ""},
		{0.3, nil, false, "gate_a"},
		{0.9, f(0.5), true, ""},
		{0.9, f(0.1), false, "gate_follow"}, // 混合字串高分，但本題單獨找不到東西
		{0.3, f(0.9), false, "gate_a"},
	} {
		pass, reason := Gate(tc.top, tc.follow, 0.45, 0.2)
		if pass != tc.pass || reason != tc.reason {
			t.Errorf("Gate(%v, %v) = %v %q, want %v %q", tc.top, tc.follow, pass, reason, tc.pass, tc.reason)
		}
	}
}

// queryRerank 依「查詢含哪個字、文件含哪個字」給分，用來分辨重排是拿混合字串還是本題單獨去算。
type queryRerank struct{ got []string }

func (q *queryRerank) Rerank(ctx context.Context, query string, docs []string) ([]float32, error) {
	q.got = append(q.got, query)
	out := make([]float32, len(docs))
	for i, d := range docs {
		if strings.Contains(query, "回滾") && strings.Contains(d, "回滾") {
			out[i] = 0.95
		}
		if strings.Contains(query, "同事") && strings.Contains(d, "同事") {
			out[i] = 0.6
		}
	}
	return out, nil
}

func TestRunFollowUpNeedsQuestionAloneToPass(t *testing.T) {
	st := &fakeStore{
		cands: []store.Candidate{{ID: 1, Kind: "faq", Content: "回滾次數", ProjectPath: "kb/faq", Path: "rollback/entry.md"}},
		faq:   &store.FAQ{Path: "rollback/entry.md"},
	}
	c := cfg
	c.TauFollow = 0.2
	history := []string{"回滾一天可以做幾次？"}

	rr := &queryRerank{}
	e := &Engine{Store: st, Embedder: &fakeEmbed{}, Reranker: rr, Cfg: c}
	r, err := e.Run(context.Background(), "我隨便追問一下當測試", history, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 混合字串含「回滾」所以最高分 0.95，但本題單獨對卡片是 0：不能重貼前一題的答案
	if r.Status != StatusNoData || r.Reason != "gate_follow" || r.Top < 0.9 || r.Follow == nil || *r.Follow != 0 {
		t.Fatalf("got status=%s reason=%s top=%v follow=%v", r.Status, r.Reason, r.Top, r.Follow)
	}
	if len(rr.got) != 2 || rr.got[1] != "我隨便追問一下當測試" {
		t.Errorf("rerank queries = %q, want the combined query then the question alone", rr.got)
	}

	// 本題自己也找得到東西（文件有「同事」）就照常回答
	st.cands[0].Content = "回滾可以找同事一起做"
	r, err = e.Run(context.Background(), "那可以找同事一起做嗎？", history, nil)
	if err != nil || r.Status != StatusFAQ || r.Follow == nil || *r.Follow < 0.5 {
		t.Fatalf("got %+v, err=%v", r, err)
	}

	// 不是追問就不算本題分數
	r, _ = e.Run(context.Background(), "回滾一天可以做幾次？", nil, nil)
	if r.Follow != nil || r.Status != StatusFAQ {
		t.Fatalf("non-follow-up got %+v", r)
	}
}

type fakeAliases [][]string

func (f fakeAliases) Glossary(ctx context.Context) ([][]string, error) { return f, nil }

// 別名：問題說 rollback、文件寫回滾。送進模型的每一份文字（嵌入、全文檢索、重排、追問分數、挑摘錄、整理回答）
// 都要補上「回滾」，漏掉任何一處，那一步就會跟改之前一樣判成不相關。
func TestRunExpandsAliasesEverywhere(t *testing.T) {
	st := &fakeStore{cands: []store.Candidate{{ID: 1, Kind: "doc", ProjectPath: "d", Path: "21_回滾.xlsx", Heading: "C5.回滾權限", Content: "回滾權限\n值班工程師可以執行回滾"}}}
	emb, rr := &fakeEmbed{}, &queryRerank{}
	g := &fakeGen{ans: &llm.Answer{Status: llm.StatusAnswered, Text: "x [1]"}}
	c := cfg
	c.TauFollow, c.ExcerptChars = 0.2, 200
	e := &Engine{Store: st, Embedder: emb, Reranker: rr, Cfg: c, Gen: g, Aliases: fakeAliases{{"回滾", "rollback"}}}

	r, err := e.Run(context.Background(), "那 rollback 的權限呢？", []string{"rollback 一天可以做幾次？"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 前面的問題與本題各自展開，本題自己也帶著別名
	want := "rollback（回滾） 一天可以做幾次？\n那 rollback（回滾） 的權限呢？"
	if r.Query != want || emb.got[0] != want {
		t.Errorf("query = %q, embedded = %q, want %q", r.Query, emb.got[0], want)
	}
	if !strings.Contains(st.fts[0], "回滾") {
		t.Errorf("full-text query = %q, want it to contain 回滾", st.fts[0])
	}
	// 重排兩次：混合字串、本題單獨（追問分數）；本題單獨的那次要自己補上，不能靠前一題
	if len(rr.got) != 2 || rr.got[1] != "那 rollback（回滾） 的權限呢？" {
		t.Fatalf("rerank calls = %q", rr.got)
	}
	if r.Status != StatusAnswer || r.Follow == nil || *r.Follow < 0.9 {
		t.Fatalf("status = %s follow = %v, want an answer passing the follow-up gate", r.Status, r.Follow)
	}
	if !strings.Contains(g.question, "rollback（回滾） 的權限") {
		t.Errorf("question to the model = %q", g.question)
	}

	// 沒有整理回答時要挑摘錄：挑摘錄的重排也要補上
	e.Gen, rr.got = nil, nil
	r, err = e.Run(context.Background(), "rollback 的權限", nil, nil)
	if err != nil || r.Status != StatusDocs || len(rr.got) != 2 {
		t.Fatalf("status = %s rerank calls = %q err = %v", r.Status, rr.got, err)
	}
	for _, q := range rr.got {
		if !strings.Contains(q, "回滾") {
			t.Errorf("rerank query %q lacks the alias", q)
		}
	}

	// eval 與 search 走 Retrieve／FollowScore，也要展開
	rr.got = nil
	ranked, _, err := e.Retrieve(context.Background(), "rollback 的權限", nil)
	if err != nil || len(ranked) != 1 || ranked[0].Score < 0.9 {
		t.Fatalf("Retrieve: %+v, %v", ranked, err)
	}
	if s, err := e.FollowScore(context.Background(), "rollback 的權限", ranked); err != nil || s < 0.9 {
		t.Fatalf("FollowScore = %v, %v", s, err)
	}

	// 沒有詞彙表：照原字串，rollback 對不上回滾
	e.Aliases = nil
	r, _ = e.Run(context.Background(), "rollback 的權限", nil, nil)
	if r.Query != "rollback 的權限" || r.Status != StatusNoData {
		t.Fatalf("without glossary: query = %q status = %s", r.Query, r.Status)
	}
}

func TestWindows(t *testing.T) {
	content := "a1\na2\na3\na4\na5\na6\na7"
	ws := Windows(content, 100, 4)
	if len(ws) != 3 || ws[0].Text != "a1\na2\na3\na4" || ws[1].Text != "a3\na4\na5\na6" || ws[2].Text != "a5\na6\na7" {
		t.Fatalf("windows = %+v", ws)
	}
	if !ws[0].First || ws[0].Last || !ws[2].Last {
		t.Errorf("first/last flags wrong: %+v", ws)
	}
	if got := ws[1].Display(); got != "…\na3\na4\na5\na6\n…" {
		t.Errorf("display = %q", got)
	}
	// 字數上限：第一行就超過時截斷該行
	long := Windows(strings.Repeat("長", 50)+"\n短", 10, 4)
	if long[0].Text != strings.Repeat("長", 10)+"…" {
		t.Errorf("long line = %q", long[0].Text)
	}
}

// 重排只看窗口有沒有「8H」，確認挑的是含答案的那組行，而不是片段開頭。
type answerRerank struct{}

func (answerRerank) Rerank(ctx context.Context, q string, docs []string) ([]float32, error) {
	out := make([]float32, len(docs))
	for i, d := range docs {
		out[i] = 0.9 // 片段本身
		if strings.Contains(d, "8H") {
			out[i] = 0.99
		} else if !strings.Contains(d, "備份") {
			out[i] = 0.1
		}
	}
	return out, nil
}

func TestRunPicksExcerptWithAnswer(t *testing.T) {
	content := "備份保留的時間點\n節點1-1\n節點1-2\n節點1-3\n節點1-4\n節點1-5\n備份保留累計時間上限\n基礎 | 8H\n進階方案 | 基礎再加上2H"
	st := &fakeStore{cands: []store.Candidate{{ID: 1, Kind: "doc", Content: content}}}
	c := cfg
	c.ExcerptChars = 200
	e := &Engine{Store: st, Embedder: &fakeEmbed{}, Reranker: answerRerank{}, Cfg: c}
	r, err := e.Run(context.Background(), "備份保留最多累積幾小時", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusDocs || !strings.Contains(r.Hits[0].Excerpt, "基礎 | 8H") || strings.Contains(r.Hits[0].Excerpt, "節點1-1") {
		t.Fatalf("excerpt = %q", r.Hits[0].Excerpt)
	}
}

// ---- 模型整理回答----

type fakeGen struct {
	ans      *llm.Answer
	err      error
	question string
	docs     []llm.Doc
	calls    int
}

func (f *fakeGen) Answer(ctx context.Context, q string, docs []llm.Doc) (*llm.Answer, error) {
	f.calls++
	f.question, f.docs = q, docs
	if f.err != nil {
		return nil, f.err
	}
	a := *f.ans
	return &a, nil
}

type fakeBudget bool

func (b fakeBudget) Allow(ctx context.Context) (bool, error) { return bool(b), nil }

func genEngine(g *fakeGen, budget Budget, cands ...store.Candidate) *Engine {
	c := cfg
	c.KMax = 2
	scores := fakeRerank{}
	for i, cd := range cands {
		scores[cd.Content] = 0.9 - float32(i)*0.1
	}
	return &Engine{Store: &fakeStore{cands: cands, faq: &store.FAQ{}}, Embedder: &fakeEmbed{}, Reranker: scores, Cfg: c, Gen: g, Budget: budget}
}

var genCands = []store.Candidate{
	{ID: 1, Kind: "doc", ProjectPath: "d", Path: "02_部署.xlsx", Heading: "3.部署介面", Content: "x2 規格由帳號等級決定"},
	{ID: 2, Kind: "doc", ProjectPath: "d", Path: "00_憑證.xlsx", Heading: "介面", Draft: true, Content: "憑證效期 30 天"},
	{ID: 3, Kind: "doc", ProjectPath: "d", Path: "x.xlsx", Heading: "x", Content: "第三段不送"},
}

func TestGenerateAnswer(t *testing.T) {
	g := &fakeGen{ans: &llm.Answer{Status: llm.StatusAnswered, Text: "x2 靠帳號等級 [1]", Model: "m", InputTokens: 10}}
	r, err := genEngine(g, fakeBudget(true), genCands...).Run(context.Background(), "兩倍規格怎麼開通", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusAnswer || len(r.Hits) != 2 || r.Answer.InputTokens != 10 {
		t.Fatalf("got status=%s hits=%d answer=%+v", r.Status, len(r.Hits), r.Answer)
	}
	// 送給模型的段落：k_max=2，編號從 1 起，未確認設計帶 note
	if len(g.docs) != 2 || g.docs[0].Index != 1 || g.docs[1].Note != "未確認設計" || g.question != "兩倍規格怎麼開通" {
		t.Errorf("docs = %+v question = %q", g.docs, g.question)
	}
}

func TestGenerateNoDataAndFallbacks(t *testing.T) {
	// 閘門 B：模型說文件沒有 ⇒ no_data
	g := &fakeGen{ans: &llm.Answer{Status: llm.StatusNoData, Model: "m"}}
	r, _ := genEngine(g, nil, genCands...).Run(context.Background(), "q", nil, nil)
	if r.Status != StatusNoData || r.Reason != "gate_b" || r.Answer == nil {
		t.Fatalf("gate b: %+v", r)
	}
	// 閘門 C：引用對不上（llm.Validate 標 uncited）⇒ no_data、reason=gate_c
	g = &fakeGen{ans: &llm.Answer{Status: llm.StatusNoData, Reason: "uncited", Model: "m"}}
	r, _ = genEngine(g, nil, genCands...).Run(context.Background(), "q", nil, nil)
	if r.Status != StatusNoData || r.Reason != "gate_c" {
		t.Fatalf("gate c: %+v", r)
	}
	// API 錯誤、拒答：退回原文摘錄（仍是 docs，不是缺口）
	for _, g := range []*fakeGen{
		{err: errors.New("503")},
		{ans: &llm.Answer{Status: llm.StatusNoData, Reason: "refusal", Model: "m", InputTokens: 5}},
	} {
		r, _ = genEngine(g, nil, genCands...).Run(context.Background(), "q", nil, nil)
		if r.Status != StatusDocs || r.GenSkipped == "" || len(r.Hits) == 0 {
			t.Fatalf("fallback: status=%s skipped=%q", r.Status, r.GenSkipped)
		}
	}
	// 預算用完：不呼叫模型，退回原文摘錄
	g = &fakeGen{ans: &llm.Answer{Status: llm.StatusAnswered, Text: "x [1]"}}
	r, _ = genEngine(g, fakeBudget(false), genCands...).Run(context.Background(), "q", nil, nil)
	if g.calls != 0 || r.Status != StatusDocs || r.GenSkipped != "budget" {
		t.Fatalf("budget: calls=%d status=%s skipped=%q", g.calls, r.Status, r.GenSkipped)
	}
}

func TestGenerateSkipsFAQAndPassesHistory(t *testing.T) {
	g := &fakeGen{ans: &llm.Answer{Status: llm.StatusAnswered, Text: "x [1]"}}
	faq := store.Candidate{ID: 9, Kind: "faq", ProjectPath: "kb/faq", Path: "a.md", Content: "卡片"}
	r, _ := genEngine(g, nil, append([]store.Candidate{faq}, genCands...)...).Run(context.Background(), "q", nil, nil)
	if r.Status != StatusFAQ || g.calls != 0 {
		t.Fatalf("faq on top should not call the model: status=%s calls=%d", r.Status, g.calls)
	}
	r, _ = genEngine(g, nil, genCands...).Run(context.Background(), "那效期呢？", []string{"兩倍規格怎麼開通"}, nil)
	if !strings.Contains(g.question, "兩倍規格怎麼開通") || !strings.HasSuffix(g.question, "那效期呢？") {
		t.Errorf("follow-up question to the model = %q", g.question)
	}
}

// ---- 翻譯補查----

type fakeRewriter struct {
	repl   map[string]string // 原句 → 改寫；沒列的原樣
	err    error
	calls  [][]string
	titles []string
}

func (f *fakeRewriter) Rewrite(ctx context.Context, lines, titles []string) (*llm.Rewrite, error) {
	f.calls, f.titles = append(f.calls, lines), titles
	rw := &llm.Rewrite{Model: "m", InputTokens: 300, OutputTokens: 40}
	if f.err != nil {
		return rw, f.err
	}
	for _, l := range lines {
		if r, ok := f.repl[l]; ok {
			l = r
		}
		rw.Lines = append(rw.Lines, l)
	}
	return rw, nil
}

// termRerank：查詢與文件都含 term 才給高分，模擬「問題寫英文、文件寫中文」時重排判不相關。
type termRerank string

func (t termRerank) Rerank(ctx context.Context, query string, docs []string) ([]float32, error) {
	out := make([]float32, len(docs))
	for i, d := range docs {
		out[i] = 0.05
		if strings.Contains(query, string(t)) && strings.Contains(d, string(t)) {
			out[i] = 0.9
		}
	}
	return out, nil
}

func rescueEngine(rw *fakeRewriter, g *fakeGen, budget Budget) (*Engine, *fakeStore) {
	st := &fakeStore{cands: []store.Candidate{{ID: 1, Kind: "doc", ProjectPath: "d", Path: "06_值班系統.xlsx", Heading: "操作說明", Content: "值班一個月 30 格"}}}
	e := &Engine{Store: st, Embedder: &fakeEmbed{}, Reranker: termRerank("值班"), Cfg: cfg, Rewriter: rw, Budget: budget}
	if g != nil {
		e.Gen = g
	}
	return e, st
}

func TestRunRescue(t *testing.T) {
	ctx := context.Background()
	q := "on-call 有幾格？"
	fixed := "on-call（值班）有幾格？"

	// 救回來：第一次沒過，改寫後通過；交給模型整理的是改寫後的問題，Query 是補查實際拿去查的字串
	rw := &fakeRewriter{repl: map[string]string{q: fixed}}
	g := &fakeGen{ans: &llm.Answer{Status: llm.StatusAnswered, Text: "30 格 [1]"}}
	e, st := rescueEngine(rw, g, fakeBudget(true))
	r, err := e.Run(ctx, q, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusAnswer || r.Rescue == nil || !r.Rescue.Rescued || r.Query != fixed || r.Rescue.Query != fixed {
		t.Fatalf("rescued: status=%s query=%q rescue=%+v", r.Status, r.Query, r.Rescue)
	}
	if g.question != fixed || r.Rescue.Usage == nil || r.Rescue.Usage.InputTokens != 300 || len(st.fts) != 2 {
		t.Fatalf("rescued: model question=%q usage=%+v searches=%d", g.question, r.Rescue.Usage, len(st.fts))
	}

	// 補了還是找不到：維持第一次的結果（缺口用第一次的字串去重），但記下試過的字串
	rw = &fakeRewriter{repl: map[string]string{q: "on-call（輪班）有幾格？"}}
	e, _ = rescueEngine(rw, nil, nil)
	r, _ = e.Run(ctx, q, nil, nil)
	if r.Status != StatusNoData || r.Reason != "gate_a" || r.Query != q || r.Rescue.Rescued || r.Rescue.Query != "on-call（輪班）有幾格？" {
		t.Fatalf("not rescued: status=%s query=%q rescue=%+v", r.Status, r.Query, r.Rescue)
	}

	// 第一次就過：不呼叫改寫
	rw = &fakeRewriter{}
	e, _ = rescueEngine(rw, nil, nil)
	if r, _ = e.Run(ctx, "值班有幾格？", nil, nil); r.Status != StatusDocs || r.Rescue != nil || len(rw.calls) != 0 {
		t.Fatalf("passed first time: status=%s rescue=%+v calls=%d", r.Status, r.Rescue, len(rw.calls))
	}

	// 預算用完：不呼叫改寫，照第一次回 no_data
	rw = &fakeRewriter{repl: map[string]string{q: fixed}}
	e, _ = rescueEngine(rw, nil, fakeBudget(false))
	if r, _ = e.Run(ctx, q, nil, nil); r.Status != StatusNoData || r.Rescue.Skipped != "budget" || len(rw.calls) != 0 {
		t.Fatalf("budget: status=%s rescue=%+v calls=%d", r.Status, r.Rescue, len(rw.calls))
	}

	// 改寫失敗：不是錯誤，照第一次回 no_data；用量照記（預算要算）
	rw = &fakeRewriter{err: errors.New("503")}
	e, _ = rescueEngine(rw, nil, nil)
	r, err = e.Run(ctx, q, nil, nil)
	if err != nil || r.Status != StatusNoData || r.Rescue.Skipped != "error" || r.Rescue.Usage == nil {
		t.Fatalf("rewrite error: err=%v status=%s rescue=%+v", err, r.Status, r.Rescue)
	}

	// 模型沒補任何字：不再查一次
	rw = &fakeRewriter{}
	e, st = rescueEngine(rw, nil, nil)
	if r, _ = e.Run(ctx, q, nil, nil); r.Rescue.Skipped != "unchanged" || len(st.fts) != 1 {
		t.Fatalf("unchanged: rescue=%+v searches=%d", r.Rescue, len(st.fts))
	}

	// 討論串追問：前面的問題與本題一起改寫（一行對一行），追問分數用改寫後的本題
	rw = &fakeRewriter{repl: map[string]string{"那 on-call 排滿呢？": "那 on-call（值班）排滿呢？"}}
	c := cfg
	c.TauFollow = 0.2
	e, _ = rescueEngine(rw, nil, nil)
	e.Cfg = c
	r, _ = e.Run(ctx, "那 on-call 排滿呢？", []string{"憑證多久到期？"}, nil)
	if len(rw.calls) != 1 || len(rw.calls[0]) != 2 || rw.calls[0][1] != "那 on-call 排滿呢？" {
		t.Fatalf("follow-up rewrite input = %q", rw.calls)
	}
	if r.Status != StatusDocs || !r.Rescue.Rescued || r.Follow == nil || *r.Follow < 0.5 || r.Question() != "那 on-call（值班）排滿呢？" {
		t.Fatalf("follow-up rescue: status=%s follow=%v question=%q", r.Status, r.Follow, r.Question())
	}
}

type fakeTitles []string

func (f fakeTitles) DocPaths(ctx context.Context) ([]string, error) { return f, nil }

func TestDocTitlesGoToRewriter(t *testing.T) {
	got := DocTitles([]string{"specs/02_部署流程.xlsx", "specs/21_回滾-權限配置.xlsx", "docs/deploy.md", "specs/02_部署流程.xlsx"})
	if !slices.Equal(got, []string{"部署流程", "回滾-權限配置", "deploy"}) {
		t.Fatalf("titles = %q", got)
	}
	rw := &fakeRewriter{repl: map[string]string{"on-call 有幾格？": "值班系統有幾格？"}}
	e, _ := rescueEngine(rw, nil, nil)
	e.Titles = fakeTitles{"specs/06_值班系統.xlsx"}
	if r, _ := e.Run(context.Background(), "on-call 有幾格？", nil, nil); !r.Rescue.Rescued || !slices.Equal(rw.titles, []string{"值班系統"}) {
		t.Fatalf("rescue = %+v titles = %q", r.Rescue, rw.titles)
	}
}
