// Package chunk 把文件切成檢索用的片段。
package chunk

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MaxChars = 1200 // 片段上限（字元）
	MinChars = 80   // 太短的節併入下一節
)

type Chunk struct {
	Heading   string // "部署 › 回滾"；xlsx 是分頁名
	LineStart int    // Markdown 是行號；xlsx 是列號
	LineEnd   int
	Content   string
}

// Line 是帶行號（或列號）的一行文字。
type Line struct {
	N    int
	Text string
}

// Lines 把多行文字依序裝進片段：不切開單行，單行超過上限時在句號處切。
func Lines(heading string, lines []Line) []Chunk {
	var out []Chunk
	var cur []Line
	size := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		texts := make([]string, len(cur))
		for i, l := range cur {
			texts[i] = l.Text
		}
		out = append(out, Chunk{Heading: heading, LineStart: cur[0].N, LineEnd: cur[len(cur)-1].N, Content: strings.Join(texts, "\n")})
		cur, size = nil, 0
	}
	for _, l := range lines {
		n := utf8.RuneCountInString(l.Text)
		if n > MaxChars {
			flush()
			for _, piece := range splitSentences(l.Text, MaxChars) {
				out = append(out, Chunk{Heading: heading, LineStart: l.N, LineEnd: l.N, Content: piece})
			}
			continue
		}
		if size > 0 && size+1+n > MaxChars {
			flush()
		}
		cur = append(cur, l)
		size += n + 1
	}
	flush()
	return out
}

var sentenceEnd = regexp.MustCompile(`[。！？!?]|\.\s`)

// splitSentences 在句號處切成不超過 max 字元的段；找不到句號就硬切。
func splitSentences(s string, max int) []string {
	var out []string
	for utf8.RuneCountInString(s) > max {
		r := []rune(s)
		window := string(r[:max])
		cut := -1
		for _, loc := range sentenceEnd.FindAllStringIndex(window, -1) {
			cut = loc[1]
		}
		if cut <= 0 {
			cut = len(window)
		}
		out = append(out, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

var headingRe = regexp.MustCompile(`^(#{1,4})\s+(.+?)\s*#*\s*$`)

type section struct {
	path  []string
	start int
	lines []Line
}

// Markdown 依 H1–H4 切節；程式碼區塊不切開；長節在段落、句號處切；短節併入下一節。
func Markdown(src string) []Chunk {
	var sections []*section
	var stack []string
	cur := &section{start: 1}
	inFence := false
	for i, text := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		n := i + 1
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if !inFence {
			if m := headingRe.FindStringSubmatch(text); m != nil {
				sections = append(sections, cur)
				level := len(m[1])
				if len(stack) >= level {
					stack = stack[:level-1]
				}
				for len(stack) < level-1 {
					stack = append(stack, "")
				}
				stack = append(stack, m[2])
				cur = &section{path: compact(stack), start: n}
			}
		}
		cur.lines = append(cur.lines, Line{N: n, Text: text})
	}
	sections = append(sections, cur)

	var out []Chunk
	var carry *section // 太短、要併入下一節的內容
	for _, s := range sections {
		if strings.TrimSpace(joinLines(s.lines)) == "" {
			continue
		}
		if carry != nil {
			s.lines = append(carry.lines, s.lines...)
			s.path = carry.path
			carry = nil
		}
		if utf8.RuneCountInString(strings.TrimSpace(joinLines(s.lines))) < MinChars {
			carry = s
			continue
		}
		out = append(out, splitSection(s)...)
	}
	if carry != nil {
		if len(out) > 0 {
			last := &out[len(out)-1]
			last.Content += "\n" + joinLines(carry.lines)
			last.LineEnd = carry.lines[len(carry.lines)-1].N
		} else {
			out = append(out, splitSection(carry)...)
		}
	}
	return out
}

// splitSection 先依段落（空行；程式碼區塊視為一段）分塊，再用 Lines 裝箱。
func splitSection(s *section) []Chunk {
	heading := strings.Join(s.path, " › ")
	var blocks []Line
	var buf []Line
	inFence := false
	flush := func() {
		if len(buf) == 0 {
			return
		}
		text := strings.TrimSpace(joinLines(buf))
		if text != "" {
			blocks = append(blocks, Line{N: buf[0].N, Text: text})
		}
		buf = nil
	}
	for _, l := range s.lines {
		t := strings.TrimSpace(l.Text)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			if !inFence {
				flush()
			}
			inFence = !inFence
			buf = append(buf, l)
			if !inFence {
				flush()
			}
			continue
		}
		if !inFence && t == "" {
			flush()
			continue
		}
		buf = append(buf, l)
	}
	flush()

	end := s.lines[len(s.lines)-1].N
	chunks := linesKeepFence(heading, blocks)
	for i := range chunks {
		if i+1 < len(chunks) {
			chunks[i].LineEnd = chunks[i+1].LineStart - 1
		} else {
			chunks[i].LineEnd = end
		}
	}
	return chunks
}

// linesKeepFence 把段落裝箱（段落間空一行）。程式碼區塊超過上限時整塊獨立成片段、不切；
// 一般段落超過上限時在句號處切。
func linesKeepFence(heading string, blocks []Line) []Chunk {
	var out []Chunk
	var cur []string
	start, size := 0, 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, Chunk{Heading: heading, LineStart: start, Content: strings.Join(cur, "\n\n")})
			cur, size = nil, 0
		}
	}
	for _, b := range blocks {
		n := utf8.RuneCountInString(b.Text)
		if n > MaxChars {
			flush()
			if strings.HasPrefix(b.Text, "```") || strings.HasPrefix(b.Text, "~~~") {
				out = append(out, Chunk{Heading: heading, LineStart: b.N, Content: b.Text})
			} else {
				for _, piece := range splitSentences(b.Text, MaxChars) {
					out = append(out, Chunk{Heading: heading, LineStart: b.N, Content: piece})
				}
			}
			continue
		}
		if size > 0 && size+2+n > MaxChars {
			flush()
		}
		if len(cur) == 0 {
			start = b.N
		}
		cur = append(cur, b.Text)
		size += n + 2
	}
	flush()
	return out
}

func joinLines(ls []Line) string {
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = l.Text
	}
	return strings.Join(parts, "\n")
}

func compact(path []string) []string {
	var out []string
	for _, p := range path {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
