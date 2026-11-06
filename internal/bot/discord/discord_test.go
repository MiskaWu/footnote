package discord

import (
	"github.com/miskawu/footnote/internal/kb"
	"github.com/miskawu/footnote/internal/render"
	"github.com/miskawu/footnote/internal/store"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestStripMention(t *testing.T) {
	for in, want := range map[string]string{
		"<@123> 回滾怎麼做？":  "回滾怎麼做？",
		"回滾 <@!123> 怎麼做": "回滾  怎麼做",
		"<@123>":         "",
		"<@999> 別人":      "<@999> 別人",
	} {
		if got := stripMention(in, "123"); got != want {
			t.Errorf("stripMention(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFeedbackIDRoundTrip(t *testing.T) {
	row := feedbackButtons(42)[0].(discordgo.ActionsRow)
	for i, wantVote := range []int{1, -1} {
		id := row.Components[i].(discordgo.Button).CustomID
		askID, vote, ok := parseFeedbackID(id)
		if !ok || askID != 42 || vote != wantVote {
			t.Errorf("parse(%q) = %d %d %v", id, askID, vote, ok)
		}
	}
	for _, bad := range []string{"fn:fb:x:1", "fn:fb:1:2", "fn:fb:1"} {
		if _, _, ok := parseFeedbackID(bad); ok {
			t.Errorf("parse(%q) should fail", bad)
		}
	}
}

func TestThreadName(t *testing.T) {
	if got := threadName(strings.Repeat("問", 60)); len([]rune(got)) != 51 {
		t.Errorf("thread name len = %d", len([]rune(got)))
	}
}

func TestCardEmbed(t *testing.T) {
	long := "[1] [handbook · 21_回滾.xlsx › S.基本原則（第 2–48 列）](https://gitlab.example.com/acme/handbook/-/blob/main/specs/" + strings.Repeat("%E6%B7%B1", 40) + ".xlsx)"
	c := &render.Card{Color: "partial", Body: "結論 [1]。\n• 一點 [1]", Footer: "AI 整理", Sources: []string{long, long, long, long, long, long}}
	e := cardEmbed(c)
	if e.Color != 0xF0B232 || e.Description != c.Body || e.Footer == nil || e.Footer.Text != "AI 整理" || e.Title != "" {
		t.Fatalf("embed = %+v", e)
	}
	// 出處超過單欄上限就分欄，每欄都在上限內，一行都不少
	if len(e.Fields) < 2 || e.Fields[0].Name != "出處" {
		t.Fatalf("fields = %d, first = %q", len(e.Fields), e.Fields[0].Name)
	}
	lines := 0
	for _, f := range e.Fields {
		if f.Name != "出處" && f.Name != "​" {
			t.Errorf("unexpected field %q", f.Name)
		}
		if n := len([]rune(f.Value)); n > embedFieldMax {
			t.Errorf("field too long: %d", n)
		}
		lines += strings.Count(f.Value, "\n") + 1
	}
	if lines != 6 {
		t.Errorf("source lines = %d, want 6", lines)
	}
	// FAQ 卡：有標題，沒有連結（卡片存在資料庫）
	f := cardEmbed(&render.Card{Color: "faq", Title: "回滾一天可以做幾次？", Body: "b"})
	if f.Title != "回滾一天可以做幾次？" || f.URL != "" || f.Color != 0x5865F2 || len(f.Fields) != 0 || f.Footer != nil {
		t.Errorf("faq embed = %+v", f)
	}
}

func TestCardEmbedNotesBeforeSources(t *testing.T) {
	e := cardEmbed(&render.Card{Color: "partial", Body: "**結論**", Notes: []string{"文件沒寫天數", "兩份說法不同"}, Sources: []string{"⁽¹⁾ [a](u)"}})
	if len(e.Fields) != 2 || e.Fields[0].Name != "⚠ 注意" || e.Fields[0].Value != "- 文件沒寫天數\n- 兩份說法不同" || e.Fields[1].Name != "出處" {
		t.Fatalf("fields = %+v %+v", e.Fields[0], e.Fields[1])
	}
}

func TestParseReviewID(t *testing.T) {
	for id, want := range map[string]struct {
		n      int64
		action string
		ok     bool
	}{
		"fn:rv:12:approve": {12, "approve", true},
		"fn:rv:12:reject":  {12, "reject", true},
		"fn:rv:12:reason":  {12, "reason", true},
		"fn:rv:12:delete":  {0, "", false}, // 只有這三種動作
		"fn:rv:x:approve":  {0, "", false},
		"fn:rv:12":         {0, "", false},
	} {
		n, action, ok := parseReviewID(id)
		if n != want.n || action != want.action || ok != want.ok {
			t.Errorf("%s = %d, %q, %v", id, n, action, ok)
		}
	}
}

func TestReviewEmbed(t *testing.T) {
	c := store.KBChange{ID: 7, ProposedBy: "agent", Note: "回答 G7"}
	e := reviewEmbed(c, kb.Preview{Title: "新增 canary/promote.md", Gap: "G7「金絲雀什麼時候可以全量？」", Body: "本文", Lines: []string{"問法：a／b", "[1] x:y#z"}})
	if e.Title != "提案 #7：新增 canary/promote.md" || e.Description != "本文" || len(e.Fields) != 3 {
		t.Fatalf("embed = %+v", e)
	}
	// 差異放在 code block：內容再長、再含反引號，框都要關得起來，也不能超過上限
	long := strings.Repeat("- 舊的一行 ``` 內容\n", 400)
	e = reviewEmbed(c, kb.Preview{Title: "修改 a/b.md", Diff: long})
	body := strings.TrimSuffix(strings.TrimPrefix(e.Description, "```diff\n"), "\n```")
	if len([]rune(e.Description)) > embedDescMax || !strings.HasSuffix(e.Description, "\n```") || strings.Contains(body, "```") {
		t.Fatalf("description 長 %d：%q", len([]rune(e.Description)), e.Description[:40])
	}
}
