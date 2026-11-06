package render

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/miskawu/footnote/internal/llm"
	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/store"
)

var opts = Options{GitLabURL: "https://gitlab.example.com", ExcerptChars: 200, Refs: map[string]string{"acme/handbook": "main"}}

func docHit(heading string, draft bool, content string) qa.Hit {
	return qa.Hit{Candidate: store.Candidate{
		ProjectPath: "acme/handbook", Path: "specs/21_回滾.xlsx", Ref: "main",
		Heading: heading, LineStart: 3, LineEnd: 20, Content: content, Draft: draft, Kind: "doc",
	}, Score: 0.8}
}

func TestReplyDocs(t *testing.T) {
	r := &qa.Result{Status: qa.StatusDocs, Hits: []qa.Hit{docHit("A.回滾步驟", false, "從主控台執行。\n選擇版本"), docHit("草稿", true, "x")}}
	got := Reply(r, nil, opts)
	for _, want := range []string{
		"1. [handbook · 21_回滾.xlsx › A.回滾步驟（第 3–20 列）](<https://gitlab.example.com/acme/handbook/-/blob/main/specs/21_%E5%9B%9E%E6%BB%BE.xlsx>)",
		"> 從主控台執行。\n> 選擇版本",
		"（未確認設計）",
		"按 👎",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestReplyMarksOldVersion(t *testing.T) {
	old := docHit("Sheet2", false, "權限只剩一人時進入收回倒數")
	old.Path, old.SupersededBy = "specs/09_權限系統.xlsx", "specs/09_權限系統_調整版.xlsx"
	got := Reply(&qa.Result{Status: qa.StatusDocs, Hits: []qa.Hit{old}}, nil, opts)
	if !strings.Contains(got, "（舊版，新版見 09_權限系統_調整版）") {
		t.Errorf("missing old-version tag in:\n%s", got)
	}
}

func TestReplyNoDataWithGap(t *testing.T) {
	r := &qa.Result{Status: qa.StatusNoData, NearMisses: []qa.Hit{docHit("總覽", false, "")}}
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
		Hits:   []qa.Hit{{Candidate: store.Candidate{ProjectPath: "kb/faq", Path: "rollback/entry.md", Ref: "main", Kind: "faq", Content: "從主控台執行 [1]。"}}},
		FAQ: &store.FAQ{
			Verified:   time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
			StaleSince: &stale,
			Sources:    []store.FAQSourceRow{{Ord: 1, Ref: "acme/handbook:specs/21_回滾.xlsx#A.回滾步驟", SrcProject: "acme/handbook", SrcPath: "specs/21_回滾.xlsx"}},
		},
	}
	got := Reply(r, nil, opts)
	for _, want := range []string{"從主控台執行 [1]。\n──\n[1] [handbook · 21_回滾.xlsx › A.回滾步驟]", "FAQ · rollback/entry.md", "2026-10-08 確認", "⚠"} {
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

func TestReplyAnswer(t *testing.T) {
	h1, h2, h3 := docHit("A.回滾步驟", false, "x"), docHit("C.其他", false, "z"), docHit("S.基本原則", true, "y")
	a := &llm.Answer{
		Status: "partial", Summary: "從主控台執行 [3]。", Points: []string{"- 次數用完改用緊急權限 [1][3]", " "},
		Caveats: []string{"緊急權限名稱兩份文件不同 [1][7]"}, Missing: "文件沒寫緊急權限怎麼取得。",
	}
	a.Compose()
	r := &qa.Result{Status: qa.StatusAnswer, Hits: []qa.Hit{h1, h2, h3}, Answer: a}
	got := Reply(r, nil, opts)
	for _, want := range []string{
		"從主控台執行 [2]。\n• 次數用完改用緊急權限 [1][2]\n⚠ 緊急權限名稱兩份文件不同 [1]", // 引用 1、3 重編成 1、2；超出範圍的 [7] 拿掉
		"\n⚠ 文件沒寫緊急權限怎麼取得。\n",                                  // 已經以「文件」開頭就原樣用，不重複、也不切字
		"[1] [handbook · 21_回滾.xlsx › A.回滾步驟", "[2] [handbook · 21_回滾.xlsx › S.基本原則", "（未確認設計）", AnswerNote,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// 只列引用到的段落，編號連續
	if strings.Contains(got, "C.其他") || strings.Contains(got, "[3]") || strings.Contains(got, "[7]") {
		t.Errorf("uncited source or gap in numbering:\n%s", got)
	}
	// 超過單則上限時縮短本文，出處與提示保留
	r.Answer.Text = strings.Repeat("很長的回答。", 400) + " [3]"
	o := opts
	o.MaxLen = 2000
	got = Reply(r, nil, o)
	if utf8.RuneCountInString(got) > 2000 || !strings.Contains(got, AnswerNote) || !strings.Contains(got, "S.基本原則") {
		t.Errorf("fit failed: len=%d", utf8.RuneCountInString(got))
	}
}

func TestReplyGeneralScope(t *testing.T) {
	r := &qa.Result{Status: qa.StatusNoData, Answer: &llm.Answer{Status: "no_data", Scope: "general"}}
	if got := Reply(r, nil, opts); got != GeneralText {
		t.Errorf("got %q", got)
	}
}

func TestMissingLine(t *testing.T) {
	for in, want := range map[string]string{
		"文件沒寫明開通所需的權限": "⚠ 文件沒寫明開通所需的權限", // 不能切成「明開通……」
		"憑證效期的實際天數":    "⚠ 文件沒寫：憑證效期的實際天數",
		"  ":           "",
	} {
		if got := missingLine(in); got != want {
			t.Errorf("missingLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// FAQ 卡存在資料庫，沒有網址：文件片段照常連到 GitLab，卡片只留文字，不能組出一個 404 的連結。
func TestFAQHitHasNoLink(t *testing.T) {
	doc := qa.Hit{Candidate: store.Candidate{Kind: "doc", ProjectPath: "acme/handbook", Ref: "main", Path: "a.xlsx"}}
	if got := textLink("標籤", doc, opts); !strings.Contains(got, "https://gitlab.example.com/") {
		t.Errorf("doc = %q", got)
	}
	faq := qa.Hit{Candidate: store.Candidate{Kind: "faq", ProjectPath: "kb/faq", Ref: "main", Path: "rollback/entry.md"}}
	if got := textLink("標籤", faq, opts); got != "標籤" {
		t.Errorf("faq = %q", got)
	}
}
