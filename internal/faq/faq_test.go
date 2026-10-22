package faq

import (
	"strings"
	"testing"
)

const good = `---
questions:
  - 正式環境要怎麼回滾？
  - 部署壞掉怎麼退回上一版
sources:
  - acme/handbook:deploy/rollback.md#回滾步驟
  - acme/handbook:deploy/release-checklist.md#上線後
gap: 3
verified: 2026-10-08
---
執行 acmectl rollback 退回上一個 release [1]。回滾後要在發布頻道公告 [2]。
`

func TestParseGood(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Questions) != 2 || len(c.Sources) != 2 || c.Gap == nil || *c.Gap != 3 {
		t.Fatalf("card = %+v", c)
	}
	if c.Sources[0].Project != "acme/handbook" || c.Sources[0].Path != "deploy/rollback.md" || c.Sources[0].Anchor != "回滾步驟" {
		t.Errorf("source0 = %+v", c.Sources[0])
	}
	if c.BodyLine != 11 {
		t.Errorf("BodyLine = %d, want 11", c.BodyLine)
	}
	if probs := Lint(c, nil); len(probs) != 0 {
		t.Errorf("Lint = %v", probs)
	}
}

func TestLintProblems(t *testing.T) {
	src := `---
questions: [只有一種]
sources:
  - 沒有冒號
  - a/b:c.md
verified: 昨天
gap: 0
---
內容 [3]。
`
	c, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	probs := strings.Join(Lint(c, nil), "\n")
	for _, want := range []string{"至少要兩種問法", "格式應為", "不是 YYYY-MM-DD", "gap 要是正整數", "超出 sources 範圍"} {
		if !strings.Contains(probs, want) {
			t.Errorf("missing %q in:\n%s", want, probs)
		}
	}
}

func TestLintUncitedSourceAndLength(t *testing.T) {
	src := "---\nquestions: [a, b]\nsources: [\"x/y:a.md\", \"x/y:b.md\"]\nverified: 2026-10-08\n---\n" + strings.Repeat("長", 1300) + " [1]"
	c, _ := Parse([]byte(src))
	probs := strings.Join(Lint(c, nil), "\n")
	if !strings.Contains(probs, "沒有被引用") || !strings.Contains(probs, "超過上限") {
		t.Errorf("probs = %s", probs)
	}
}

func TestLintCheckSources(t *testing.T) {
	c, _ := Parse([]byte(good))
	resolve := func(project, path string) (string, bool, bool) {
		if project != "acme/handbook" {
			return "", false, false
		}
		return "sha", strings.HasSuffix(path, "rollback.md"), true
	}
	probs := strings.Join(Lint(c, resolve), "\n")
	if !strings.Contains(probs, "找不到 deploy/release-checklist.md") {
		t.Errorf("probs = %s", probs)
	}
	c.Sources[0].Project = "secret/private"
	if probs := strings.Join(Lint(c, resolve), "\n"); !strings.Contains(probs, "不在同步範圍內") {
		t.Errorf("probs = %s", probs)
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{"沒有 frontmatter", "---\nquestions: [a]\n"} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("Parse(%q) want error", src)
		}
	}
}
