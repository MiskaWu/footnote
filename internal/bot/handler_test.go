package bot

import (
	"testing"
	"time"
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
