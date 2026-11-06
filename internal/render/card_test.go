package render

import (
	"strings"
	"testing"
	"time"

	"github.com/miskawu/footnote/internal/llm"
	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/store"
)

func TestBuildCardAnswer(t *testing.T) {
	a := &llm.Answer{Status: "partial", Summary: "從主控台執行 [3]。", Points: []string{"- 每天 2 次 [1][2]", "  "}, Caveats: []string{"⚠ 名稱不一致 [3]"},
		Missing: "文件沒寫緊急權限怎麼取得"}
	a.Compose()
	// 第 1、2 段是同一分頁（DT.對照表）的兩段：合併成同一個出處
	h1, h2, h3 := docHit("DT.對照表", false, "x"), docHit("DT.對照表", false, "y"), docHit("S.基本原則", true, "z")
	h2.LineStart, h2.LineEnd = 50, 90
	r := &qa.Result{Status: qa.StatusAnswer, Answer: a, Hits: []qa.Hit{h1, h2, h3}}
	c := BuildCard(r, opts)
	if c == nil || c.Color != "partial" || c.Title != "" || c.Footer != CardNote {
		t.Fatalf("card = %+v", c)
	}
	// 結論粗體、空一行、真正的條列；[1][2] 同分頁合成一個上標；[3] 變成 ⁽²⁾
	if want := "**從主控台執行⁽²⁾。**\n\n- 每天 2 次⁽¹⁾"; c.Body != want {
		t.Errorf("body = %q, want %q", c.Body, want)
	}
	if len(c.Notes) != 2 || c.Notes[0] != "名稱不一致⁽²⁾" || c.Notes[1] != "文件沒寫緊急權限怎麼取得" {
		t.Errorf("notes = %q", c.Notes)
	}
	if len(c.Sources) != 2 || !strings.HasPrefix(c.Sources[0], "⁽¹⁾ [21_回滾 › DT.對照表](") ||
		!strings.HasPrefix(c.Sources[1], "⁽²⁾ [21_回滾 › S.基本原則](") || !strings.HasSuffix(c.Sources[1], "（未確認設計）") ||
		strings.Contains(strings.Join(c.Sources, ""), "(<") || strings.Contains(strings.Join(c.Sources, ""), "列）") {
		t.Errorf("sources = %q", c.Sources)
	}
	// 純文字版用同一套分組：[1][2] 合成 [1]，出處兩筆、不帶列號
	got := Reply(r, nil, opts)
	if !strings.Contains(got, "• 每天 2 次 [1]\n") || !strings.Contains(got, "[2] [handbook · 21_回滾.xlsx › S.基本原則](<") || strings.Contains(got, "[3]") {
		t.Errorf("text reply:\n%s", got)
	}
	a2 := &llm.Answer{Status: "answered", Summary: "結論 [1]"}
	a2.Compose()
	if c := BuildCard(&qa.Result{Status: qa.StatusAnswer, Answer: a2, Hits: r.Hits}, opts); c.Color != "answered" || len(c.Notes) != 0 || c.Body != "**結論⁽¹⁾**" {
		t.Errorf("answered card = %+v", c)
	}
}

func TestBuildCardFAQAndOthers(t *testing.T) {
	stale := time.Now()
	r := &qa.Result{
		Status: qa.StatusFAQ,
		Hits:   []qa.Hit{{Candidate: store.Candidate{ProjectPath: "kb/faq", Path: "rollback/entry.md", Ref: "main", Kind: "faq", Heading: "回滾一天可以做幾次？", Content: "從主控台執行 [1]。"}}},
		FAQ: &store.FAQ{
			Verified: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), StaleSince: &stale,
			Sources: []store.FAQSourceRow{{Ord: 1, Ref: "acme/handbook:specs/21_回滾.xlsx#A.回滾步驟", SrcProject: "acme/handbook", SrcPath: "specs/21_回滾.xlsx"}},
		},
	}
	c := BuildCard(r, opts)
	if c == nil || c.Color != "stale" || c.Title != "回滾一天可以做幾次？" ||
		c.Body != "從主控台執行⁽¹⁾。" || len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "內容待重新確認") ||
		c.Footer != "FAQ 卡 · 2026-10-08 確認" || len(c.Sources) != 1 || !strings.HasPrefix(c.Sources[0], "⁽¹⁾ [21_回滾 › A.回滾步驟](") {
		t.Fatalf("faq card = %+v", c)
	}
	for _, st := range []string{qa.StatusDocs, qa.StatusNoData} {
		if BuildCard(&qa.Result{Status: st}, opts) != nil {
			t.Errorf("%s should stay plain text", st)
		}
	}
}

func TestSup(t *testing.T) {
	if sup(1) != "⁽¹⁾" || sup(12) != "⁽¹²⁾" {
		t.Errorf("sup = %q %q", sup(1), sup(12))
	}
	// 不在對照表的編號拿掉；連續重複的合成一個；不連續的保留
	if got := supMarks("甲 [1][2] 乙 [9]，丙 [1]", map[int]int{1: 1, 2: 1}); got != "甲⁽¹⁾ 乙，丙⁽¹⁾" {
		t.Errorf("supMarks = %q", got)
	}
}
