package ingest

import (
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
	for name, want := range map[string]bool{"999_Backup(Ver.1.0)": true, "PricingTable_廢棄": true, "功能項目備份": true, "002_功能說明": false} {
		if got := skipSheet(pats, name); got != want {
			t.Errorf("skipSheet(%q) = %v", name, got)
		}
	}
}
