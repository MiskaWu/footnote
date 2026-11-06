package bot

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/render"
)

func TestLimiter(t *testing.T) {
	l := NewLimiter(2, 3)
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	if !l.Allow("u", now) || !l.Allow("u", now) {
		t.Fatal("first two should pass")
	}
	if l.Allow("u", now.Add(10*time.Second)) {
		t.Fatal("third within a minute should be blocked")
	}
	if !l.Allow("other", now) {
		t.Fatal("other user should pass")
	}
	if !l.Allow("u", now.Add(61*time.Second)) {
		t.Fatal("after a minute should pass")
	}
	if l.Allow("u", now.Add(3*time.Minute)) {
		t.Fatal("per-day limit 3 should block")
	}
	if !l.Allow("u", now.Add(25*time.Hour)) {
		t.Fatal("next day should pass")
	}
}

func TestOnQuestionClosedScope(t *testing.T) {
	cfg := &config.Config{}
	cfg.Bot.Channels = map[string][]string{"111": {}}
	// Store、QA 都是 nil：沒開放的地方必須在碰到資料庫與檢索之前就擋下
	h := NewHandler(nil, nil, nil, nil, cfg, render.Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, in := range []Incoming{
		{Platform: "discord", ChannelRef: "222", Text: "q"},
		{Platform: "discord", ChannelRef: "111", DM: true, Text: "q"}, // 私訊不看 ChannelRef，看 "dm"
		{Platform: "web", ChannelRef: "web", Text: "q"},
	} {
		rep, err := h.OnQuestion(context.Background(), in, 0)
		if err != nil || rep.Status != "closed" || rep.Text != render.ClosedText {
			t.Errorf("%+v: got %+v, %v; want closed", in, rep, err)
		}
	}
}

func TestScopeKey(t *testing.T) {
	if k := (Incoming{ChannelRef: "111", DM: true}).ScopeKey(); k != config.ScopeDM {
		t.Errorf("DM key = %q", k)
	}
	if k := (Incoming{ChannelRef: "111"}).ScopeKey(); k != "111" {
		t.Errorf("channel key = %q", k)
	}
}
