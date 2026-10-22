package render

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/store"
)

var opts = Options{GitLabURL: "https://gitlab.example.com", ExcerptChars: 200, Refs: map[string]string{"acme/handbook": "main"}}

func docHit(heading string, draft bool, content string) qa.Hit {
	return qa.Hit{Candidate: store.Candidate{
		ProjectPath: "acme/handbook", Path: "specs/計費規則.xlsx", Ref: "main",
		Heading: heading, LineStart: 3, LineEnd: 20, Content: content, Draft: draft, Kind: "doc",
	}, Score: 0.8}
}

func TestReplyDocs(t *testing.T) {
	r := &qa.Result{Status: qa.StatusDocs, Hits: []qa.Hit{docHit("結帳流程", false, "先建立訂單。\n選擇付款方式"), docHit("草稿", true, "x")}}
	got := Reply(r, nil, opts)
	for _, want := range []string{
		"1. [handbook · 計費規則.xlsx › 結帳流程（第 3–20 列）](<https://gitlab.example.com/acme/handbook/-/blob/main/specs/%E8%A8%88%E8%B2%BB%E8%A6%8F%E5%89%87.xlsx>)",
		"> 先建立訂單。\n> 選擇付款方式",
		"（草稿）",
		"按 👎",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestReplyNoDataWithGap(t *testing.T) {
	r := &qa.Result{Status: qa.StatusNoData, NearMisses: []qa.Hit{docHit("退款", false, "")}}
	got := Reply(r, &GapInfo{ID: 42, Hits: 3, Status: "open"}, opts)
	for _, want := range []string{"目前內部文件沒有相關資料", "知識缺口 G42", "被問過 3 次", "最接近的文件是", "回到這個討論串通知你"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if got := Reply(r, &GapInfo{ID: 42, Status: "dismissed"}, opts); got != GeneralText {
		t.Errorf("dismissed reply = %q", got)
	}
}

func TestReplyFAQ(t *testing.T) {
	stale := time.Now()
	r := &qa.Result{
		Status: qa.StatusFAQ,
		Hits:   []qa.Hit{{Candidate: store.Candidate{ProjectPath: "acme/faq", Path: "billing/refund.md", Ref: "main", Kind: "faq", Content: "在後台的訂單頁按退款 [1]。"}}},
		FAQ: &store.FAQ{
			Verified:   time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
			StaleSince: &stale,
			Sources:    []store.FAQSourceRow{{Ord: 1, Ref: "acme/handbook:specs/計費規則.xlsx#退款規則", SrcProject: "acme/handbook", SrcPath: "specs/計費規則.xlsx"}},
		},
	}
	got := Reply(r, nil, opts)
	for _, want := range []string{"在後台的訂單頁按退款 [1]。\n──\n[1] [handbook · 計費規則.xlsx › 退款規則]", "FAQ · [billing/refund.md]", "2026-10-08 確認", "⚠"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestReplyFitsMaxLen(t *testing.T) {
	long := strings.Repeat("很長的內容。", 100)
	r := &qa.Result{Status: qa.StatusDocs, Hits: []qa.Hit{docHit("a", false, long), docHit("b", false, long), docHit("c", false, long)}}
	o := opts
	o.MaxLen = 600
	got := Reply(r, nil, o)
	if n := utf8.RuneCountInString(got); n > 600 {
		t.Fatalf("len %d > 600", n)
	}
	if !strings.Contains(got, "1. ") {
		t.Errorf("lost first hit:\n%s", got)
	}
}

func TestLinkMarkdownLine(t *testing.T) {
	got := Link("https://g", "a/b", "main", "docs/x y.md", 12)
	if got != "https://g/a/b/-/blob/main/docs/x%20y.md?plain=1#L12" {
		t.Errorf("got %s", got)
	}
}

func TestExcerptCutsAtSentence(t *testing.T) {
	if got := Excerpt("第一句很長很長的句子。第二句", 13); got != "第一句很長很長的句子。…" {
		t.Errorf("got %q", got)
	}
	if got := Excerpt("短。很長很長很長很長很長很長", 10); got != "短。很長很長很長很長…" {
		t.Errorf("sentence end too early should not cut there: %q", got)
	}
}
