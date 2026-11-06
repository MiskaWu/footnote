package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSupersededBy(t *testing.T) {
	s := Source{Versions: [][]string{{"新.xlsx", "舊.xlsx", "更舊.xlsx"}}}
	for p, want := range map[string]string{"新.xlsx": "", "舊.xlsx": "新.xlsx", "更舊.xlsx": "新.xlsx", "別的.xlsx": ""} {
		if got := s.SupersededBy(p); got != want {
			t.Errorf("SupersededBy(%s) = %q, want %q", p, got, want)
		}
	}
}

func TestCheckVersions(t *testing.T) {
	ok := Source{Name: "d", Versions: [][]string{{"a", "b"}, {"c", "d"}}}
	if err := ok.checkVersions(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][][]string{
		"一組只有一份":   {{"a"}},
		"同一份出現在兩組": {{"a", "b"}, {"b", "c"}},
		"同一組重複":    {{"a", "a"}},
	} {
		s := Source{Name: "d", Versions: bad}
		if s.checkVersions() == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func load(t *testing.T, yml string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "footnote.yaml")
	if err := os.WriteFile(p, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const teiYAML = `
tei:
  embed_url: http://127.0.0.1:1
  rerank_url: http://127.0.0.1:2
`

// 沒寫 name 時：文件來源用 project_path，FAQ 來源用 "faq"（footnote ingest --source faq 靠這個）。
func TestLoadDefaultNames(t *testing.T) {
	c := load(t, teiYAML+`
sources:
  - project_path: team/docs
    dir: docs
faq:
  project_path: kb/faq
`)
	if got := c.Sources[0].Name; got != "team/docs" {
		t.Errorf("source name = %q, want team/docs", got)
	}
	if got := c.FAQ.Name; got != "faq" {
		t.Errorf("faq name = %q, want faq", got)
	}
}

func TestLoadExplicitNames(t *testing.T) {
	c := load(t, teiYAML+`
sources:
  - name: design
    project_path: team/docs
    dir: docs
faq:
  name: cards
  project_path: kb/faq
`)
	if c.Sources[0].Name != "design" || c.FAQ.Name != "cards" {
		t.Errorf("names = %q, %q, want design, cards", c.Sources[0].Name, c.FAQ.Name)
	}
}

func TestBotScope(t *testing.T) {
	strict := Bot{Channels: map[string][]string{"111": {"acme/"}, "222": {}, ScopeWeb: nil}}
	open := Bot{Channels: map[string][]string{"111": {"acme/"}, ScopeAny: {}}}
	for _, c := range []struct {
		name     string
		b        Bot
		key      string
		ok       bool
		prefixes []string
	}{
		{"列出的頻道帶專案前綴", strict, "111", true, []string{"acme/"}},
		{"空清單代表全部專案", strict, "222", true, nil},
		{"值是 null 也算有列", strict, ScopeWeb, true, nil},
		{"沒列的頻道不回答", strict, "333", false, nil},
		{"沒列 dm 時私訊不回答", strict, ScopeDM, false, nil},
		{"空表哪裡都不回答", Bot{}, "111", false, nil},
		{"* 接住沒列的頻道", open, "333", true, nil},
		{"* 也接住私訊", open, ScopeDM, true, nil},
		{"有列的頻道不被 * 蓋掉", open, "111", true, []string{"acme/"}},
	} {
		p, ok := c.b.Scope(c.key)
		if ok != c.ok || len(p) != len(c.prefixes) || (len(p) > 0 && p[0] != c.prefixes[0]) {
			t.Errorf("%s: Scope(%q) = %v, %v; want %v, %v", c.name, c.key, p, ok, c.prefixes, c.ok)
		}
	}
}

// Discord 的頻道 id 是 19 位數字，設定檔裡沒加引號時也要原樣當成字串，不能被轉成數字而變樣。
func TestLoadChannelIDUnquoted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.yaml")
	yml := "tei: {embed_url: http://e, rerank_url: http://r}\nbot:\n  channels:\n    1100000000000000001: [acme/]\n    \"dm\": []\n"
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := c.Bot.Scope("1100000000000000001"); !ok || len(p) != 1 || p[0] != "acme/" {
		t.Fatalf("unquoted id: got %v, %v (channels=%v)", p, ok, c.Bot.Channels)
	}
	if _, ok := c.Bot.Scope("web"); ok {
		t.Fatal("web 沒列，不該開放")
	}
}

// FAQ 來源固定存在資料庫。舊設定留著 dir 時要報錯，不能默默當成沒有卡片。
func TestFAQIsDB(t *testing.T) {
	c := load(t, teiYAML+`
faq:
  project_path: kb/faq
`)
	if c.FAQ.Type != TypeDB {
		t.Errorf("faq type = %q", c.FAQ.Type)
	}
	p := filepath.Join(t.TempDir(), "footnote.yaml")
	if err := os.WriteFile(p, []byte(teiYAML+"faq:\n  project_path: kb/faq\n  dir: ~/faq-cards\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("faq.dir 要報錯")
	}
}

// 文件來源不能宣告成 db：db 只給 FAQ，文件來源一定要有 checkout。
func TestDocSourceMustBeLocal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "footnote.yaml")
	if err := os.WriteFile(p, []byte(teiYAML+"sources:\n  - project_path: team/docs\n    dir: docs\n    type: gitlab\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("want error")
	}
}

// Discord 的 id 是 19 位數字，設定檔沒加引號時也要原樣當字串。
func TestReviewIDsUnquoted(t *testing.T) {
	c := load(t, teiYAML+`
bot:
  review_channel: 1100000000000000001
  reviewers: [1100000000000000002]
`)
	if c.Bot.ReviewChannel != "1100000000000000001" || len(c.Bot.Reviewers) != 1 || c.Bot.Reviewers[0] != "1100000000000000002" {
		t.Fatalf("bot = %+v", c.Bot)
	}
}
