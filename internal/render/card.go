package render

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/miskawu/footnote/internal/qa"
)

// Card 是 AI 回答與 FAQ 卡的卡片版面（與平台無關），Discord 轉成 embed。
// 版面原則：結論粗體一行、空一行、真正的條列（換行會對齊）；注意事項獨立一塊；出處用上標小編號、同分頁合併；
// 頁尾一句提示。純文字版（Reply）與卡片共用出處分組（citeGroups）、版本標註（qa.Note）與連結（Link）。
type Card struct {
	Color   string   // answered | partial | faq | stale：決定邊條顏色
	Title   string   // FAQ 卡的代表問法；AI 回答沒有標題
	Body    string   // 主要內容（Markdown）：結論與條列
	Notes   []string // 注意事項（未確認設計、文件說法不一致、文件沒寫、卡片過期），一則一句
	Sources []string // 已編號的出處，一行一個（Markdown 連結）
	Footer  string   // 頁尾小字，純文字（Discord 的頁尾不支援連結）
}

const (
	staleNote = "⚠ 這張卡引用的檔案在確認後有變動，內容待重新確認。"
	CardNote  = "AI 依上列文件整理，請以原文為準"
)

// BuildCard 只處理 AI 回答與 FAQ 卡；其他狀態（文件段落、沒有資料）回傳 nil，照舊用純文字。
func BuildCard(r *qa.Result, o Options) *Card {
	switch r.Status {
	case qa.StatusAnswer:
		a := r.Answer
		num, groups := citeGroups(a.Text, r.Hits)
		c := &Card{Color: "answered", Footer: CardNote}
		if a.Status == "partial" {
			c.Color = "partial"
		}
		summary, points, caveats := a.Summary, a.Points, a.Caveats
		if strings.TrimSpace(summary) == "" && len(points) == 0 { // 舊資料只有組好的純文字
			summary = a.Text
		}
		var b strings.Builder
		if s := supMarks(strings.TrimSpace(summary), num); s != "" {
			b.WriteString("**" + s + "**")
		}
		var items []string
		for _, p := range points {
			if p = supMarks(cleanItem(p), num); p != "" {
				items = append(items, "- "+p)
			}
		}
		if len(items) > 0 {
			b.WriteString("\n\n" + strings.Join(items, "\n"))
		}
		c.Body = b.String()
		for _, cv := range caveats {
			if cv = supMarks(cleanItem(cv), num); cv != "" {
				c.Notes = append(c.Notes, cv)
			}
		}
		if m := strings.TrimSpace(a.Missing); a.Status == "partial" && m != "" {
			if !strings.HasPrefix(m, "文件") {
				m = "文件沒寫：" + m
			}
			c.Notes = append(c.Notes, m)
		}
		for i, h := range groups {
			s := sup(i+1) + " " + esc(shortLabel(h.Path, h.Heading)) // FAQ 卡沒有網址
			if h.Kind != "faq" {
				s = fmt.Sprintf("%s [%s](%s)", sup(i+1), esc(shortLabel(h.Path, h.Heading)), Link(o.GitLabURL, h.ProjectPath, h.Ref, h.Path, h.LineStart))
			}
			if note := qa.Note(h.Candidate); note != "" {
				s += "（" + note + "）"
			}
			c.Sources = append(c.Sources, s)
		}
		return c
	case qa.StatusFAQ:
		h := r.Hits[0]
		c := &Card{Color: "faq", Title: h.Heading, Footer: "FAQ 卡"}
		ident := map[int]int{}
		if r.FAQ != nil {
			for _, s := range r.FAQ.Sources {
				ident[s.Ord] = s.Ord
				_, url := sourceLabelURL(s, o)
				anchor := ""
				if _, a, ok := strings.Cut(s.Ref, "#"); ok {
					anchor = a
				}
				c.Sources = append(c.Sources, fmt.Sprintf("%s [%s](%s)", sup(s.Ord), esc(shortLabel(s.SrcPath, anchor)), url))
			}
			c.Footer = fmt.Sprintf("FAQ 卡 · %s 確認", r.FAQ.Verified.Format("2006-01-02"))
			if r.FAQ.StaleSince != nil {
				c.Color = "stale"
				c.Notes = append(c.Notes, strings.TrimPrefix(staleNote, "⚠ "))
			}
		}
		c.Body = supMarks(h.Content, ident)
		return c
	}
	return nil
}

// shortLabel 是卡片上的出處名稱：檔名（不含副檔名）› 分頁或標題。專案名稱與列號都省略，連結本身會帶到檔案。
func shortLabel(p, heading string) string {
	s := strings.TrimSuffix(path.Base(p), path.Ext(p))
	if heading != "" {
		s += " › " + heading
	}
	return s
}

// cleanItem 去掉模型自己加的條列符號與警示符號，版面由這裡統一加。
func cleanItem(s string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "-•・*⚠ "))
}

var supDigits = []rune("⁰¹²³⁴⁵⁶⁷⁸⁹")

// sup 把編號寫成上標腳註：⁽¹²⁾。
func sup(n int) string {
	var b strings.Builder
	b.WriteRune('⁽')
	for _, d := range strconv.Itoa(n) {
		b.WriteRune(supDigits[d-'0'])
	}
	b.WriteRune('⁾')
	return b.String()
}

// supMarks 把本文的 [n] 換成上標腳註（連前面的空白一起拿掉），不在對照表的拿掉，連續重複的合成一個。
func supMarks(text string, num map[int]int) string {
	text = citeSpRe.ReplaceAllStringFunc(text, func(m string) string {
		d := m[strings.LastIndex(m, "[")+1 : len(m)-1]
		c, _ := strconv.Atoi(d)
		if k, ok := num[c]; ok {
			return fmt.Sprintf("\x00%d\x01", k) // 先放暫時記號，去重後再換成上標
		}
		return ""
	})
	var b strings.Builder
	last := -1
	for i := 0; i < len(text); {
		if text[i] == 0 {
			j := strings.IndexByte(text[i:], 1)
			k, _ := strconv.Atoi(text[i+1 : i+j])
			if k != last {
				b.WriteString(sup(k))
			}
			last = k
			i += j + 1
			continue
		}
		last = -1
		b.WriteByte(text[i])
		i++
	}
	return strings.TrimSpace(b.String())
}
