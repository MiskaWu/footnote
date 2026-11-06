package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
)

func TestMatches(t *testing.T) {
	refs := []string{"a/b:x.xlsx#流程"}
	if !matches([]string{"a/b:x.xlsx"}, refs) || !matches([]string{"a/b:x.xlsx#流程"}, refs) {
		t.Error("should match")
	}
	if matches([]string{"a/b:x.xlsx#別的"}, refs) || matches([]string{"a/b:y.xlsx"}, refs) {
		t.Error("should not match")
	}
	// 只到檔案的出處，對不上寫到分頁的預期出處
	if matches([]string{"a/b:x.xlsx#流程"}, []string{"a/b:x.xlsx"}) {
		t.Error("file-level ref should not satisfy sheet-level expectation")
	}
}

// ---- Run 用假實作：FAQ 卡引用了預期出處就算命中 ----

type fakeStore struct {
	cands  []store.Candidate
	faqs   map[string]*store.FAQ // path → 卡
	lookup int
}

func (f *fakeStore) HybridSearch(ctx context.Context, vec []float32, fts string, k, limit int, prefixes []string) ([]store.Candidate, error) {
	return f.cands, nil
}
func (f *fakeStore) GetFAQ(ctx context.Context, project, path string) (*store.FAQ, error) {
	f.lookup++
	if c, ok := f.faqs[path]; ok {
		return c, nil
	}
	return nil, store.ErrNotFound
}

type fakeEmbed struct{}

func (fakeEmbed) Embed(ctx context.Context, texts []string, kind tei.EmbedKind) ([][]float32, error) {
	return [][]float32{{1, 0}}, nil
}

type fakeRerank map[string]float32 // content 結尾 → score

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

func TestRunCountsFAQCitingExpectedSource(t *testing.T) {
	st := &fakeStore{
		cands: []store.Candidate{
			{ID: 1, ProjectPath: "d", Path: "x.xlsx", Heading: "流程", Content: "doc"},
			{ID: 2, ProjectPath: "kb/faq", Path: "x/card.md", Heading: "問法", Kind: "faq", Content: "card"},
			{ID: 3, ProjectPath: "kb/faq", Path: "x/loose.md", Heading: "問法", Kind: "faq", Content: "loose"},
		},
		faqs: map[string]*store.FAQ{
			"x/card.md":  {Sources: []store.FAQSourceRow{{Ref: "d:y.xlsx#規則"}, {Ref: "d:x.xlsx#流程"}}},
			"x/loose.md": {Sources: []store.FAQSourceRow{{Ref: "d:z.xlsx"}}}, // 只引用到檔案
		},
	}
	e := &qa.Engine{Store: st, Embedder: fakeEmbed{}, Reranker: fakeRerank{"card": 0.9, "loose": 0.8, "doc": 0.5}}
	items := []Item{
		{Q: "卡片第二個來源", Expect: "answer", Sources: []string{"d:x.xlsx#流程"}},
		{Q: "卡片本身", Expect: "answer", Sources: []string{"kb/faq:x/card.md"}},
		{Q: "卡片只到檔案", Expect: "answer", Sources: []string{"d:z.xlsx#某分頁"}},
		{Q: "沒人引用", Expect: "answer", Sources: []string{"d:w.xlsx"}},
	}
	rs, err := Run(context.Background(), e, items, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 1, 0, 0} // 第一題：卡片排第一且引用了 d:x.xlsx#流程，不必等到第三名的文件片段
	for i, r := range rs {
		if r.Rank != want[i] {
			t.Errorf("%s: rank=%d, want %d", r.Q, r.Rank, want[i])
		}
	}
	if st.lookup != 2 {
		t.Errorf("GetFAQ called %d times, want 2 (cached per card)", st.lookup)
	}
}

func TestSummarizeAndPick(t *testing.T) {
	rs := []ItemResult{
		{Item: Item{Expect: "answer"}, Top: 0.9, Rank: 1},
		{Item: Item{Expect: "answer"}, Top: 0.4, Rank: 4},
		{Item: Item{Expect: "answer"}, Top: 0.2, Rank: 0},
		{Item: Item{Expect: "no_data"}, Top: 0.3},
		{Item: Item{Expect: "general"}, Top: 0.1},
	}
	s := Summarize(rs, 0.35, 0, 3)
	if s.AnswerN != 3 || s.NegativeN != 2 || s.FalseRefusalN != 1 || s.MisleadingN != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if s.RecallAtK != 1.0/3 || s.MRR != (1+0.25)/3 {
		t.Errorf("recall=%v mrr=%v", s.RecallAtK, s.MRR)
	}
	// τ 0.35 與 0.40 都是零誤導、誤拒 1 題；0.30 以下會誤導、0.45 以上誤拒 2 題
	best := Pick(Sweep(rs, 0, 3), 0)
	if best == nil || best.Tau < 0.349 || best.Tau > 0.351 {
		t.Errorf("pick = %+v", best)
	}
}

func TestPickTakesMiddleOfPlateau(t *testing.T) {
	rs := []ItemResult{
		{Item: Item{Expect: "answer"}, Top: 0.66},
		{Item: Item{Expect: "no_data"}, Top: 0.16},
	}
	best := Pick(Sweep(rs, 0, 3), 0)
	if best == nil || best.Tau < 0.399 || best.Tau > 0.401 {
		t.Errorf("pick = %+v, want 0.40 (middle of 0.20..0.65)", best)
	}
}

func TestFollowUpGateInSummaryAndSweep(t *testing.T) {
	f := func(v float32) *float32 { return &v }
	rs := []ItemResult{
		{Item: Item{Expect: "answer"}, Top: 0.9},                                          // 一般題，不受 τ_follow 影響
		{Item: Item{Expect: "answer", History: []string{"前"}}, Top: 0.9, Follow: f(0.4)},  // 追問、本題也找得到
		{Item: Item{Expect: "no_data", History: []string{"前"}}, Top: 0.9, Follow: f(0.1)}, // 追問、高分全來自前一題
	}
	// 只看 τ_answer 的話這題拒答題會被放行；加上 τ_follow 0.2 就擋下
	if s := Summarize(rs, 0.45, 0, 3); s.MisleadingN != 1 {
		t.Fatalf("without τ_follow: %+v", s)
	}
	if s := Summarize(rs, 0.45, 0.2, 3); s.MisleadingN != 0 || s.FalseRefusalN != 0 {
		t.Fatalf("with τ_follow 0.2: %+v", s)
	}
	rows := SweepFollow(rs, 0.45, 3)
	if len(rows) != 16 || rows[0].AnswerN+rows[0].NegativeN != 2 {
		t.Fatalf("sweep follow rows = %d, first = %+v", len(rows), rows[0])
	}
	// 0.15–0.40 都零錯誤，取中間
	if best := Pick(rows, 0); best == nil || best.TauFollow < 0.249 || best.TauFollow > 0.301 {
		t.Errorf("pick = %+v", best)
	}
	if SweepFollow(rs[:1], 0.45, 3) != nil {
		t.Error("no follow-up items should give nil")
	}
}

// 翻譯補查：第一次沒過、補查過了就算通過，名次用補查的；補查只在第一次沒過時才算（跟線上一樣）。
func TestSummarizeWithRescue(t *testing.T) {
	rs := []ItemResult{
		{Item: Item{Expect: "answer"}, Top: 0.1, Rank: 1, Rescue: &Rescue{Top: 0.8, Rank: 2}},          // 補查救回
		{Item: Item{Expect: "answer"}, Top: 0.9, Rank: 1, Rescue: &Rescue{Top: 0.95, Rank: 3}},         // 第一次就過：名次用第一次的
		{Item: Item{Expect: "answer"}, Top: 0.1, Rank: 1, Rescue: &Rescue{Skipped: "error", Top: 0.9}}, // 補查沒查成：不算
		{Item: Item{Expect: "no_data"}, Top: 0.1, Rescue: &Rescue{Top: 0.6}},                           // 陷阱題被救成答案：算誤導
	}
	s := Summarize(rs, 0.45, 0.2, 3)
	if s.FalseRefusalN != 1 || s.MisleadingN != 1 || s.MRR != (0.5+1+1)/3 {
		t.Fatalf("summary = %+v", s)
	}
	if !rs[0].Rescued(0.45, 0.2) || rs[1].Rescued(0.45, 0.2) || rs[2].Rescued(0.45, 0.2) || !rs[3].Rescued(0.45, 0.2) {
		t.Error("Rescued flags wrong")
	}
	// 追問題：補查的本題單獨分數也要過 τ_follow
	f := func(v float32) *float32 { return &v }
	r := ItemResult{Item: Item{Expect: "answer"}, Top: 0.9, Follow: f(0.05), Rescue: &Rescue{Top: 0.9, Follow: f(0.1)}}
	if r.Pass(0.45, 0.2) {
		t.Error("rescue follow score below τ_follow should not pass")
	}
}
