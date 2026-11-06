package eval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/llm"
	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
)

// 這組假實作模擬真實的情況：問題裡補上文件叫法才查得到。文件只有兩個分頁，分數由 scores 依問題文字決定。
type aliasStore struct{}

func (aliasStore) HybridSearch(context.Context, []float32, string, int, int, []string) ([]store.Candidate, error) {
	return []store.Candidate{
		{ProjectPath: "p", Path: "doc.xlsx", Heading: "憑證", Content: "憑證規則"},
		{ProjectPath: "p", Path: "doc.xlsx", Heading: "備份", Content: "備份規則"},
	}, nil
}

func (aliasStore) GetFAQ(context.Context, string, string) (*store.FAQ, error) {
	return nil, store.ErrNotFound
}

type aliasEmbed struct{}

func (aliasEmbed) Embed(_ context.Context, texts []string, _ tei.EmbedKind) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}

// aliasRerank：問題含某個字串時第一份文件拿 first、第二份拿 second；都沒含時全部 0.1。
type aliasRule struct {
	has           string
	first, second float32
}

type aliasRerank struct {
	rules []aliasRule
	calls *int
}

func (r aliasRerank) Rerank(_ context.Context, q string, docs []string) ([]float32, error) {
	if r.calls != nil {
		*r.calls++
	}
	out := make([]float32, len(docs))
	for i := range out {
		out[i] = 0.1
	}
	for _, x := range r.rules {
		if strings.Contains(q, x.has) {
			out[0], out[1] = x.first, x.second
			break
		}
	}
	return out, nil
}

// aliasRewriter：行內含 from 就換成 to；err 非 nil 時模擬呼叫失敗。
type aliasRewriter struct {
	from, to string
	err      error
}

func (r aliasRewriter) Rewrite(_ context.Context, lines, _ []string) (*llm.Rewrite, error) {
	if r.err != nil {
		return nil, r.err
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l
		if r.from != "" {
			out[i] = strings.ReplaceAll(l, r.from, r.to)
		}
	}
	return &llm.Rewrite{Lines: out}, nil
}

func aliasEngine(rerank aliasRerank, rw qa.Rewriter) *qa.Engine {
	return &qa.Engine{Store: aliasStore{}, Embedder: aliasEmbed{}, Reranker: rerank, Rewriter: rw,
		Cfg: config.QA{Candidates: 10, KReply: 3, TauAnswer: 0.45, TauFollow: 0.2, TauKeep: 0.15}}
}

func aliasInput() AliasInput {
	return AliasInput{
		Before:  [][]string{{"回滾", "rollback"}},
		After:   [][]string{{"回滾", "rollback"}, {"憑證", "cert"}},
		Aliases: []string{"cert"},
		Asks:    []AliasAsk{{Q: "cert 上限", Source: "p:doc.xlsx#憑證"}},
		Traps:   []string{"cert 能吃嗎"},
		K:       3,
	}
}

func hasProblem(r *AliasReport, s string) bool {
	return strings.Contains(strings.Join(r.Problems, "\n"), s)
}

// 補上「憑證」才查得到；陷阱題含「能吃」，文件沒有這件事。
func goodRerank() aliasRerank {
	return aliasRerank{rules: []aliasRule{{"能吃", 0.1, 0.1}, {"憑證", 0.8, 0.2}}}
}

func TestCheckAliasPasses(t *testing.T) {
	r, err := CheckAlias(context.Background(), aliasEngine(goodRerank(), aliasRewriter{}), aliasInput())
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() {
		t.Fatalf("該通過：%v", r.Problems)
	}
	a := r.Asks[0]
	if a.Query != "cert（憑證） 上限" {
		t.Fatalf("要用實際展開的字串量分數：%q", a.Query)
	}
	if a.Before != 0.1 || a.Top != 0.8 {
		t.Fatalf("線上與提案的分數：%+v", a)
	}
	ev := r.Evidence()
	for _, want := range []string{"cert 上限", "0.100 ⇒ 0.800", "陷阱「cert 能吃嗎」", "0 題受影響"} {
		if !strings.Contains(ev, want) {
			t.Errorf("紀錄缺「%s」：%s", want, ev)
		}
	}
}

// 題目不齊時不能算通過，也不花檢索與模型的錢。
func TestCheckAliasNeedsQuestions(t *testing.T) {
	calls := 0
	rr := goodRerank()
	rr.calls = &calls
	in := aliasInput()
	in.Traps = []string{"跟這個叫法無關的問題"}
	r, err := CheckAlias(context.Background(), aliasEngine(rr, aliasRewriter{}), in)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() || !hasProblem(r, "--trap") {
		t.Fatalf("陷阱題沒提到叫法不能過：%v", r.Problems)
	}
	in = aliasInput()
	in.Asks = nil
	if r, _ = CheckAlias(context.Background(), aliasEngine(rr, aliasRewriter{}), in); r.OK() || !hasProblem(r, "--ask") {
		t.Fatalf("沒有 --ask 不能過：%v", r.Problems)
	}
	in = aliasInput()
	in.Asks[0].Source = "p:doc.xlsx" // 只寫到檔案的話，同檔案任何分頁都算數
	if r, _ = CheckAlias(context.Background(), aliasEngine(rr, aliasRewriter{}), in); r.OK() || !hasProblem(r, "分頁") {
		t.Fatalf("出處要寫到分頁：%v", r.Problems)
	}
	if calls != 0 {
		t.Fatalf("題目不齊時不該打重排，打了 %d 次", calls)
	}
}

func TestCheckAliasNoEffect(t *testing.T) {
	cases := []struct {
		name   string
		rerank aliasRerank
		want   string
	}{
		{"補上還是沒過門檻", aliasRerank{}, "還是沒過閘門 A"},
		{"第一名不是答案分頁", aliasRerank{rules: []aliasRule{{"能吃", 0.1, 0.1}, {"憑證", 0.2, 0.8}}}, "第一名不是答案分頁"},
		{"沒補也查得到", aliasRerank{rules: []aliasRule{{"能吃", 0.1, 0.1}, {"上限", 0.8, 0.2}}}, "證明不了"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := CheckAlias(context.Background(), aliasEngine(c.rerank, aliasRewriter{}), aliasInput())
			if err != nil {
				t.Fatal(err)
			}
			if r.OK() || !hasProblem(r, c.want) {
				t.Fatalf("要有「%s」：%v", c.want, r.Problems)
			}
		})
	}
}

// 補上別名之後陷阱題也過門檻，把沒資料變成有答案。
func TestCheckAliasTrapAnswered(t *testing.T) {
	rr := aliasRerank{rules: []aliasRule{{"憑證", 0.8, 0.2}}} // 補上之後陷阱題也過門檻
	r, err := CheckAlias(context.Background(), aliasEngine(rr, aliasRewriter{}), aliasInput())
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() || !hasProblem(r, "陷阱題") {
		t.Fatalf("陷阱題被答出來要擋：%v", r.Problems)
	}
}

// 第一次沒過、被翻譯補查救成答案，線上就會回答，一樣要擋。
func TestCheckAliasTrapRescued(t *testing.T) {
	rr := aliasRerank{rules: []aliasRule{{"規則", 0.8, 0.2}, {"能吃", 0.1, 0.1}, {"憑證", 0.8, 0.2}}}
	r, err := CheckAlias(context.Background(), aliasEngine(rr, aliasRewriter{from: "能吃嗎", to: "規則"}), aliasInput())
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() || !hasProblem(r, "陷阱題") {
		t.Fatalf("補查救成答案要擋：%v", r.Problems)
	}
}

// 補查沒跑起來就是沒驗到，不能算過。
func TestCheckAliasTrapUnverified(t *testing.T) {
	r, err := CheckAlias(context.Background(), aliasEngine(goodRerank(), aliasRewriter{err: errors.New("api down")}), aliasInput())
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() || !hasProblem(r, "沒驗到") {
		t.Fatalf("補查沒跑成不能算過：%v", r.Problems)
	}
}

func TestCheckAliasNeedsRewriter(t *testing.T) {
	if _, err := CheckAlias(context.Background(), aliasEngine(goodRerank(), nil), aliasInput()); err == nil {
		t.Fatal("沒有改寫模型時要報錯，不能少驗一半就算過")
	}
}

func TestCheckAliasEvalWorse(t *testing.T) {
	in := aliasInput()
	in.Items = []Item{
		{Q: "cert 可以給外包用嗎", Expect: "no_data"}, // 含「憑證」之後會過門檻：退步
		{Q: "別的題目", Expect: "no_data"},         // 不受影響，不重跑
	}
	rr := aliasRerank{rules: []aliasRule{{"能吃", 0.1, 0.1}, {"憑證", 0.8, 0.2}}}
	r, err := CheckAlias(context.Background(), aliasEngine(rr, aliasRewriter{}), in)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() || !hasProblem(r, "評估集退步") {
		t.Fatalf("評估集退步要擋：%v", r.Problems)
	}
	if r.Total != 2 || r.Affected != 1 || len(r.Worse) != 1 {
		t.Fatalf("受影響的題數：%+v", r)
	}
}

func TestWorse(t *testing.T) {
	yes, no := float32(0.8), float32(0.1)
	ans := Item{Expect: "answer"}
	neg := Item{Expect: "no_data"}
	cases := []struct {
		name     string
		was, now ItemResult
		bad      bool
	}{
		{"答案題維持", ItemResult{Item: ans, Top: yes, Rank: 1}, ItemResult{Item: ans, Top: yes, Rank: 2}, false},
		{"答案題被擋下", ItemResult{Item: ans, Top: yes, Rank: 1}, ItemResult{Item: ans, Top: no, Rank: 1}, true},
		{"答案掉出前三", ItemResult{Item: ans, Top: yes, Rank: 1}, ItemResult{Item: ans, Top: yes, Rank: 9}, true},
		{"本來就答不出來不算退步", ItemResult{Item: ans, Top: no, Rank: 9}, ItemResult{Item: ans, Top: no, Rank: 9}, false},
		{"陷阱題變成會答", ItemResult{Item: neg, Top: no}, ItemResult{Item: neg, Top: yes}, true},
		{"陷阱題本來就會答不算退步", ItemResult{Item: neg, Top: yes}, ItemResult{Item: neg, Top: yes}, false},
	}
	for _, c := range cases {
		if got := worse(c.was, c.now, 0.45, 0.2, 3) != ""; got != c.bad {
			t.Errorf("%s：得到 %v", c.name, got)
		}
	}
}
