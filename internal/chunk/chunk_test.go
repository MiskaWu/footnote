package chunk

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMarkdownHeadingsAndLines(t *testing.T) {
	src := strings.Join([]string{
		"# 部署", // 1
		"",     // 2
		strings.Repeat("部署流程說明。", 15), // 3
		"",      // 4
		"## 回滾", // 5
		strings.Repeat("回滾要先確認版本。", 12), // 6
		"### 細節",                    // 7
		strings.Repeat("細節內容。", 20), // 8
		"# 監控",                      // 9
		strings.Repeat("監控面板。", 20), // 10
	}, "\n")
	got := Markdown(src)
	want := []struct {
		heading    string
		start, end int
	}{
		{"部署", 1, 4},
		{"部署 › 回滾", 5, 6},
		{"部署 › 回滾 › 細節", 7, 8},
		{"監控", 9, 10},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].Heading != w.heading || got[i].LineStart != w.start || got[i].LineEnd != w.end {
			t.Errorf("chunk %d = %q %d-%d, want %q %d-%d", i, got[i].Heading, got[i].LineStart, got[i].LineEnd, w.heading, w.start, w.end)
		}
	}
}

func TestMarkdownCodeFenceNotSplit(t *testing.T) {
	code := "```go\n" + strings.Repeat("fmt.Println(\"x\")\n", 100) + "```"
	src := "# 範例\n\n" + strings.Repeat("說明文字。", 20) + "\n\n" + code + "\n\n# 標題在區塊外\n" + strings.Repeat("後續內容。", 20)
	var fence *Chunk
	for _, c := range Markdown(src) {
		c := c
		if strings.Contains(c.Content, "```go") {
			fence = &c
		}
	}
	if fence == nil || !strings.HasSuffix(fence.Content, "```") || strings.Count(fence.Content, "fmt.Println") != 100 {
		t.Fatalf("code block was split or lost: %+v", fence)
	}
}

func TestMarkdownHashInsideFenceIsNotHeading(t *testing.T) {
	src := "# 真標題\n\n" + strings.Repeat("內容。", 30) + "\n\n```sh\n# 這是註解\necho hi\n```\n"
	for _, c := range Markdown(src) {
		if strings.Contains(c.Heading, "註解") {
			t.Fatalf("fence comment treated as heading: %+v", c)
		}
	}
}

func TestMarkdownShortSectionMergesIntoNext(t *testing.T) {
	src := "# 短\n一句話\n# 長\n" + strings.Repeat("長內容。", 30)
	got := Markdown(src)
	if len(got) != 1 {
		t.Fatalf("got %d chunks: %+v", len(got), got)
	}
	if got[0].Heading != "短" || !strings.Contains(got[0].Content, "一句話") || !strings.Contains(got[0].Content, "長內容") {
		t.Errorf("merged chunk = %+v", got[0])
	}
}

func TestMarkdownLongSectionSplitsUnderLimit(t *testing.T) {
	var paras []string
	for i := 0; i < 10; i++ {
		paras = append(paras, strings.Repeat("段落文字。", 40))
	}
	for _, c := range Markdown("# 長節\n\n" + strings.Join(paras, "\n\n")) {
		if n := utf8.RuneCountInString(c.Content); n > MaxChars {
			t.Errorf("chunk has %d chars > %d", n, MaxChars)
		}
	}
}

func TestLinesPacksRowsAndSplitsLongCell(t *testing.T) {
	lines := []Line{
		{N: 2, Text: "標題"},
		{N: 5, Text: strings.Repeat("說", 700)},
		{N: 6, Text: strings.Repeat("明", 700)},
		{N: 9, Text: strings.Repeat("長句。", 500)},
	}
	got := Lines("回滾", lines)
	if got[0].LineStart != 2 || got[0].LineEnd != 5 {
		t.Errorf("first chunk rows = %d-%d, want 2-5", got[0].LineStart, got[0].LineEnd)
	}
	for _, c := range got {
		if c.Heading != "回滾" {
			t.Errorf("heading = %q", c.Heading)
		}
		if n := utf8.RuneCountInString(c.Content); n > MaxChars {
			t.Errorf("chunk rows %d-%d has %d chars", c.LineStart, c.LineEnd, n)
		}
	}
	last := got[len(got)-1]
	if last.LineStart != 9 || !strings.HasSuffix(last.Content, "。") {
		t.Errorf("long cell not split at sentence end: %+v", last.LineStart)
	}
}
