// Package faq 解析與檢查 FAQ 卡。
package faq

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/miskawu/footnote/internal/secret"
)

const MaxBodyChars = 1200

type Card struct {
	Questions []string
	Sources   []Ref
	Gap       *int64
	Verified  time.Time
	Body      string
	BodyLine  int // 本文第一行的行號

	rawVerified string
	rawSources  []string
}

// Ref 是一筆來源：project_path:path#anchor。
type Ref struct {
	Raw     string
	Project string
	Path    string
	Anchor  string
}

func ParseRef(s string) (Ref, error) {
	r := Ref{Raw: s}
	main, anchor, _ := strings.Cut(s, "#")
	project, path, ok := strings.Cut(main, ":")
	if !ok || project == "" || path == "" {
		return r, fmt.Errorf("來源 %q 格式應為 project_path:path[#anchor]", s)
	}
	r.Project, r.Path, r.Anchor = project, path, anchor
	return r, nil
}

// Parse 解析卡片。格式錯誤回傳 error；內容規則由 Lint 檢查。
func Parse(content []byte) (*Card, error) {
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(content, []byte("---\n")) {
		return nil, fmt.Errorf("缺少 frontmatter（第一行要是 ---）")
	}
	rest := content[4:]
	end := bytes.Index(rest, []byte("\n---\n"))
	var fm, body []byte
	switch {
	case end >= 0:
		fm, body = rest[:end], rest[end+5:]
	case bytes.HasSuffix(rest, []byte("\n---")):
		fm = rest[:len(rest)-4]
	default:
		return nil, fmt.Errorf("frontmatter 沒有結尾的 ---")
	}
	var raw struct {
		Questions []string `yaml:"questions"`
		Sources   []string `yaml:"sources"`
		Gap       *int64   `yaml:"gap"`
		Verified  string   `yaml:"verified"`
	}
	if err := yaml.Unmarshal(fm, &raw); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	c := &Card{
		Questions:   raw.Questions,
		Gap:         raw.Gap,
		Body:        strings.TrimSpace(string(body)),
		BodyLine:    bytes.Count(content[:4+len(fm)+5], []byte("\n")) + 1,
		rawVerified: raw.Verified,
		rawSources:  raw.Sources,
	}
	c.Verified, _ = time.Parse("2006-01-02", raw.Verified)
	for _, s := range raw.Sources {
		r, err := ParseRef(s)
		if err != nil {
			r = Ref{Raw: s}
		}
		c.Sources = append(c.Sources, r)
	}
	return c, nil
}

var citeRe = regexp.MustCompile(`\[(\d+)\]`)

// Resolver 回答「某專案的某檔案現在存在嗎、sha 是什麼」；專案不在同步範圍內時 known=false。
type Resolver func(project, path string) (sha string, exists, known bool)

// Lint 回傳所有問題；resolver 為 nil 時不檢查來源是否存在（--check-sources 才檢查）。
func Lint(c *Card, resolve Resolver) []string {
	var probs []string
	if len(c.Questions) < 2 {
		probs = append(probs, "questions 至少要兩種問法")
	}
	for i, q := range c.Questions {
		if strings.TrimSpace(q) == "" {
			probs = append(probs, fmt.Sprintf("questions[%d] 是空的", i))
		}
	}
	if len(c.Sources) == 0 {
		probs = append(probs, "sources 至少要一筆")
	}
	for i, r := range c.Sources {
		if r.Project == "" {
			if _, err := ParseRef(r.Raw); err != nil {
				probs = append(probs, fmt.Sprintf("sources[%d]：%v", i, err))
			}
		}
	}
	if c.Verified.IsZero() {
		probs = append(probs, fmt.Sprintf("verified %q 不是 YYYY-MM-DD 日期", c.rawVerified))
	}
	if c.Gap != nil && *c.Gap <= 0 {
		probs = append(probs, "gap 要是正整數（缺口編號）")
	}
	if c.Body == "" {
		probs = append(probs, "本文是空的")
	}
	if n := utf8.RuneCountInString(c.Body); n > MaxBodyChars {
		probs = append(probs, fmt.Sprintf("本文 %d 字元，超過上限 %d", n, MaxBodyChars))
	}

	cited := map[int]bool{}
	for _, m := range citeRe.FindAllStringSubmatch(c.Body, -1) {
		n, _ := strconv.Atoi(m[1])
		if n < 1 || n > len(c.Sources) {
			probs = append(probs, fmt.Sprintf("引用 [%d] 超出 sources 範圍 1..%d", n, len(c.Sources)))
			continue
		}
		cited[n] = true
	}
	if len(c.Sources) > 0 && len(cited) == 0 && c.Body != "" {
		probs = append(probs, "本文沒有任何 [n] 引用")
	}
	for i := range c.Sources {
		if !cited[i+1] && len(cited) > 0 {
			probs = append(probs, fmt.Sprintf("sources[%d]（[%d]）沒有被引用", i, i+1))
		}
	}
	for _, f := range secret.Scan(c.Body + "\n" + strings.Join(c.Questions, "\n")) {
		probs = append(probs, fmt.Sprintf("疑似密鑰（規則 %s）", f.Rule))
	}

	if resolve != nil {
		for i, r := range c.Sources {
			if r.Project == "" {
				continue
			}
			_, exists, known := resolve(r.Project, r.Path)
			switch {
			case !known:
				probs = append(probs, fmt.Sprintf("sources[%d]：專案 %s 不在同步範圍內（只能引用全員可讀、已同步的專案）", i, r.Project))
			case !exists:
				probs = append(probs, fmt.Sprintf("sources[%d]：%s 找不到 %s", i, r.Project, r.Path))
			}
		}
	}
	sort.Strings(probs)
	return probs
}

// EmbedText 是卡片的嵌入／全文檢索用文字：所有問法加上本文。
func (c *Card) EmbedText(project, path string) string {
	return project + " › " + path + "\n" + strings.Join(c.Questions, "\n") + "\n" + c.Body
}
