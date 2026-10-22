package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestStripMention(t *testing.T) {
	for in, want := range map[string]string{
		"<@123> 怎麼回滾？":   "怎麼回滾？",
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
