package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miskawu/footnote/internal/config"
)

func TestFingerprintChangesWithExtractionSettings(t *testing.T) {
	base := config.Source{ProjectPath: "a/b", SkipSheets: []string{"備份"}, Draft: []string{"00_*"}, MaxTextKB: 512}
	fp := Fingerprint(base)
	if fp != Fingerprint(base) {
		t.Fatal("fingerprint not stable")
	}
	changed := base
	changed.SkipSheets = []string{"備份", "廢棄"}
	if Fingerprint(changed) == fp {
		t.Error("skip_sheets change should change fingerprint")
	}
	changed = base
	changed.Draft = nil
	if Fingerprint(changed) == fp {
		t.Error("draft change should change fingerprint")
	}
	changed = base
	changed.Name = "renamed" // 不影響產出的設定
	if Fingerprint(changed) != fp {
		t.Error("name change should not change fingerprint")
	}
}

func TestSkipSheet(t *testing.T) {
	pats := []string{"備份", "Backup", "廢棄"}
	for name, want := range map[string]bool{"999_Backup(v1)": true, "PricingTable_廢棄": true, "項目備份": true, "002_使用說明": false} {
		if got := skipSheet(pats, name); got != want {
			t.Errorf("skipSheet(%q) = %v", name, got)
		}
	}
}

func TestExtract(t *testing.T) {
	src := config.Source{MaxTextKB: 1}
	secs, reason := Extract(src, "docs/a.md", []byte("# 標題\n\n內文"))
	if reason != "" || len(secs) != 1 || len(secs[0].Lines) != 3 || secs[0].Lines[2].N != 3 || secs[0].Lines[2].Text != "內文" {
		t.Fatalf("md = %+v, %q", secs, reason)
	}
	if _, reason := Extract(src, "a.png", nil); reason != "unsupported_type" {
		t.Errorf("png reason = %q", reason)
	}
	if _, reason := Extract(src, "big.md", []byte(strings.Repeat("字", 1000))); !strings.HasPrefix(reason, "too_large") {
		t.Errorf("big reason = %q", reason)
	}
	if _, reason := Extract(src, "s.md", []byte("DB_TOKEN = abcdefgh12345678")); !strings.HasPrefix(reason, "secret") {
		t.Errorf("secret reason = %q", reason)
	}
}

// 走 config.Load 而不是手刻 Source：--source faq 之前失效，就是因為 FAQ 的預設 name 只在 Load 裡出錯，
// 手刻設定的整合測試看不到。
func loadSources(t *testing.T) []config.Source {
	t.Helper()
	p := filepath.Join(t.TempDir(), "footnote.yaml")
	yml := `
tei: {embed_url: "http://127.0.0.1:1", rerank_url: "http://127.0.0.1:2"}
sources:
  - name: design
    project_path: team/design
    dir: design
  - project_path: team/wiki
    dir: wiki
faq:
  project_path: kb/faq
`
	if err := os.WriteFile(p, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c.AllSources()
}

func TestSelectSources(t *testing.T) {
	all := loadSources(t)
	for only, want := range map[string][]string{
		"":            {"team/design", "team/wiki", "kb/faq"},
		"design":      {"team/design"},
		"team/design": {"team/design"},
		"team/wiki":   {"team/wiki"},
		"faq":         {"kb/faq"},
		"kb/faq":      {"kb/faq"},
	} {
		got, err := selectSources(all, only)
		if err != nil {
			t.Errorf("selectSources(%q): %v", only, err)
			continue
		}
		var paths []string
		for _, s := range got {
			paths = append(paths, s.ProjectPath)
		}
		if strings.Join(paths, ",") != strings.Join(want, ",") {
			t.Errorf("selectSources(%q) = %v, want %v", only, paths, want)
		}
	}
}

func TestSelectSourcesUnknown(t *testing.T) {
	_, err := selectSources(loadSources(t), "fqa")
	if err == nil {
		t.Fatal("unknown source: want error")
	}
	for _, s := range []string{`"fqa"`, "design（team/design）", "team/wiki", "faq（kb/faq）"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q should mention %s", err, s)
		}
	}
	if strings.Contains(err.Error(), "team/wiki（") {
		t.Errorf("name 等於 project_path 時不必重複列：%q", err)
	}
}

// 打錯 --source 要在拿鎖、fetch 之前就報錯：Store 是 nil，碰到就會 panic。
func TestRunUnknownSourceFailsBeforeLock(t *testing.T) {
	in := &Ingester{Cfg: &config.Config{Sources: loadSources(t)}}
	res, err := in.Run(context.Background(), "fqa", false)
	if err == nil || len(res) != 0 {
		t.Fatalf("Run(fqa) = %v, %v; want error and no results", res, err)
	}
}
