// Package render 把問答結果排成回覆文字（Markdown，連結用 masked link）。
// Discord 與本機測試頁共用同一份格式。
package render

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/miskawu/footnote/internal/llm"
	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/store"
)

type Options struct {
	GitLabURL    string
	Refs         map[string]string // project_path → ref（組 FAQ 來源連結用）
	ExcerptChars int
	MaxLen       int // 0 表示不限；Discord 是 2000
}

// GapInfo 是 no_data 回覆要顯示的缺口資訊。
type GapInfo struct {
	ID     int64
	Hits   int
	Status string
}

const (
	ErrorText    = "系統暫時無法回答，請稍後再試。"
	GeneralText  = "內部文件沒有相關內容；這看起來是一般技術問題，可以直接問 Claude。"
	RateText     = "問得有點快，請稍等一下再問。"
	ClosedText   = "這裡沒有開放提問。"
	AnswerNote   = "🤖 AI 依上列文件整理，請以原文為準｜沒幫上忙請按 👎"
	maxQuoteRows = 4
)

// Reply 依結果排出回覆；超過 MaxLen 時先縮短摘錄，再減少筆數。
func Reply(r *qa.Result, gap *GapInfo, o Options) string {
	if r.Status == qa.StatusAnswer && o.MaxLen > 0 {
		return fitAnswer(r, o)
	}
	if r.Status != qa.StatusDocs || o.MaxLen <= 0 {
		return reply(r, gap, o, len(r.Hits))
	}
	for n := len(r.Hits); n >= 1; n-- {
		for _, ex := range []int{o.ExcerptChars, o.ExcerptChars / 2, 0} {
			oo := o
			oo.ExcerptChars = ex
			if s := reply(r, gap, oo, n); utf8.RuneCountInString(s) <= o.MaxLen {
				return s
			}
		}
	}
	return truncate(reply(r, gap, o, 1), o.MaxLen)
}

func reply(r *qa.Result, gap *GapInfo, o Options, n int) string {
	var b strings.Builder
	switch r.Status {
	case qa.StatusFAQ:
		h := r.Hits[0]
		b.WriteString(h.Content)
		b.WriteString("\n──\n")
		if r.FAQ != nil {
			for _, s := range r.FAQ.Sources {
				fmt.Fprintf(&b, "[%d] %s\n", s.Ord, sourceLink(s, o))
			}
		}
		fmt.Fprintf(&b, "FAQ · %s", esc(h.Path)) // 卡片存在資料庫，沒有網址
		if r.FAQ != nil {
			fmt.Fprintf(&b, "（%s 確認）", r.FAQ.Verified.Format("2006-01-02"))
			if r.FAQ.StaleSince != nil {
				b.WriteString("\n" + staleNote)
			}
		}
	case qa.StatusDocs:
		b.WriteString("內部文件裡最相關的段落：\n")
		for i, h := range r.Hits[:n] {
			fmt.Fprintf(&b, "%d. %s\n", i+1, hitLink(h, o))
			ex := Excerpt(h.Content, o.ExcerptChars)
			if h.Excerpt != "" && o.ExcerptChars > 0 {
				ex = fitExcerpt(h.Excerpt, o.ExcerptChars)
			}
			if ex != "" {
				for _, line := range strings.Split(ex, "\n") {
					fmt.Fprintf(&b, "> %s\n", line)
				}
			}
		}
		b.WriteString("沒找到想要的？按 👎，我會記成知識缺口。")
	case qa.StatusAnswer:
		b.WriteString(answerReply(r, o, r.Answer.Text))
	default:
		if (gap != nil && gap.Status == "dismissed") || (r.Answer != nil && r.Answer.Scope == "general") {
			return GeneralText
		}
		b.WriteString("目前內部文件沒有相關資料。\n")
		if gap != nil {
			fmt.Fprintf(&b, "已記錄為知識缺口 G%d（這個問題被問過 %d 次）。", gap.ID, gap.Hits)
		}
		if len(r.NearMisses) > 0 {
			b.WriteString("最接近的文件是：\n")
			for _, h := range r.NearMisses[:min(2, len(r.NearMisses))] {
				fmt.Fprintf(&b, "· %s\n", hitLink(h, o))
			}
		} else {
			b.WriteString("\n")
		}
		if gap != nil {
			b.WriteString("補完文件後我會回到這個討論串通知你。")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Notice 是缺口解決後回原討論串的通知內容。
func Notice(question, reply string) string {
	return fmt.Sprintf("這題現在有答案了：「%s」\n\n%s", question, reply)
}

func hitLink(h qa.Hit, o Options) string {
	s := textLink(qa.Label(h.Candidate), h, o)
	if n := qa.Note(h.Candidate); n != "" {
		s += "（" + n + "）"
	}
	return s
}

// textLink 是純文字版的片段連結。FAQ 卡存在資料庫、沒有網址，只留文字。
func textLink(label string, h qa.Hit, o Options) string {
	if h.Kind == "faq" {
		return esc(label)
	}
	return fmt.Sprintf("[%s](<%s>)", esc(label), Link(o.GitLabURL, h.ProjectPath, h.Ref, h.Path, h.LineStart))
}

func sourceLink(s store.FAQSourceRow, o Options) string {
	label, url := sourceLabelURL(s, o)
	return fmt.Sprintf("[%s](<%s>)", esc(label), url)
}

// sourceMarkdown 是給卡片用的連結：embed 裡不會產生預覽，不需要角括號。
func sourceMarkdown(s store.FAQSourceRow, o Options) string {
	label, url := sourceLabelURL(s, o)
	return fmt.Sprintf("[%s](%s)", esc(label), url)
}

func sourceLabelURL(s store.FAQSourceRow, o Options) (string, string) {
	ref := o.Refs[s.SrcProject]
	if ref == "" {
		ref = "main"
	}
	label := s.SrcProject[strings.LastIndex(s.SrcProject, "/")+1:] + " · " + path.Base(s.SrcPath)
	if _, anchor, ok := strings.Cut(s.Ref, "#"); ok && anchor != "" {
		label += " › " + anchor
	}
	return label, Link(o.GitLabURL, s.SrcProject, ref, s.SrcPath, 0)
}

// Link 組 GitLab blob 連結。Markdown 用 ?plain=1#L{n}，因為 rendered Markdown 的行號錨點不可靠；
// xlsx 沒有行號錨點，只能連到檔案。
func Link(gitlab, project, ref, p string, line int) string {
	u := strings.TrimRight(gitlab, "/") + "/" + escapePath(project) + "/-/blob/" + url.PathEscape(ref) + "/" + escapePath(p)
	ext := strings.ToLower(path.Ext(p))
	if (ext == ".md" || ext == ".mdx") && line > 0 {
		u += fmt.Sprintf("?plain=1#L%d", line)
	}
	return u
}

// Excerpt 取片段開頭 n 個字，盡量在句號或換行處截斷，最多 maxQuoteRows 行。
func Excerpt(content string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(content))
	cut := false
	if len(r) > n {
		r, cut = r[:n], true
	}
	s := string(r)
	if cut {
		if i := strings.LastIndexAny(s, "。！？\n"); i > len(s)/2 {
			s = s[:i+len(string([]rune(s[i:])[0]))]
		}
		s = strings.TrimSpace(s) + "…"
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxQuoteRows {
		lines = append(lines[:maxQuoteRows], "…")
	}
	return strings.Join(lines, "\n")
}

// fitExcerpt 把挑好的摘錄縮到 n 字以內（排版塞不下時用）。
func fitExcerpt(s string, n int) string {
	if utf8.RuneCountInString(s) <= n+4 { // 容許前後的「…」
		return s
	}
	return Excerpt(strings.Trim(s, "…\n"), n)
}

func isSheet(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return ext == ".xlsx" || ext == ".xlsm"
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

var labelEscaper = strings.NewReplacer("[", `\[`, "]", `\]`)

func esc(s string) string { return labelEscaper.Replace(s) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

var (
	citeRe    = regexp.MustCompile(`\[(\d+)\]`)
	citeSpRe  = regexp.MustCompile(`\s*\[(\d+)\]`)
	dupTextRe = regexp.MustCompile(`(\[\d+\])(\s*\[\d+\])+`)
)

// citeGroups 把回答引用到的段落（本文出現的 [n]，與閘門 C 同一個 llm.Cites；只留 1..len(hits)）依「同一檔案、同一分頁／標題」合併，
// 由小到大編成 1、2、3……。xlsx 的連結只能到檔案，同一分頁的幾段分開列只是重複。
// 回傳：原段落編號 → 新編號，以及每個新編號的代表段落（groups[k-1]）。純文字版與卡片都用它，編號一致。
func citeGroups(text string, hits []qa.Hit) (map[int]int, []qa.Hit) {
	var idx []int
	for _, c := range llm.Cites(text) {
		if c >= 1 && c <= len(hits) {
			idx = append(idx, c)
		}
	}
	sort.Ints(idx)
	num := map[int]int{}
	byKey := map[string]int{}
	var groups []qa.Hit
	for _, c := range idx {
		h := hits[c-1]
		key := h.ProjectPath + "\x00" + h.Path + "\x00" + h.Heading
		k, ok := byKey[key]
		if !ok {
			groups = append(groups, h)
			k = len(groups)
			byKey[key] = k
		}
		num[c] = k
	}
	return num, groups
}

// relabelText 把本文的 [n] 換成新編號（純文字版）；不在對照表的拿掉，連續重複的合成一個。
func relabelText(text string, num map[int]int) string {
	text = citeRe.ReplaceAllStringFunc(text, func(m string) string {
		c, _ := strconv.Atoi(m[1 : len(m)-1])
		if k, ok := num[c]; ok {
			return fmt.Sprintf("[%d]", k)
		}
		return ""
	})
	return dedupMarks(text, citeRe)
}

// dedupMarks 把連在一起的同一個編號（例如兩段同分頁的 [1][1]）合成一個。
func dedupMarks(text string, re *regexp.Regexp) string {
	return dupTextRe.ReplaceAllStringFunc(text, func(run string) string {
		seen := map[string]bool{}
		var out []string
		for _, m := range re.FindAllString(run, -1) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
		return strings.Join(out, "")
	})
}

// groupLabel 是合併後出處的名稱：到分頁／標題為止，不帶列號（連結也只到檔案）。
func groupLabel(h qa.Hit) string {
	project := h.ProjectPath[strings.LastIndex(h.ProjectPath, "/")+1:]
	return fmt.Sprintf("%s · %s › %s", project, path.Base(h.Path), h.Heading)
}

// answerReply 排整理過的回答（純文字版）。出處一律從完整的 a.Text 算，跟閘門 C 看的是同一份；
// body 是實際顯示的本文，超過單則上限時 fitAnswer 傳縮短過的版本，切掉句尾的 [n] 也不會讓出處跟著消失。
func answerReply(r *qa.Result, o Options, body string) string {
	a := r.Answer
	num, groups := citeGroups(a.Text, r.Hits)
	var b strings.Builder
	b.WriteString(relabelText(body, num))
	if l := missingLine(a.Missing); a.Status == "partial" && l != "" {
		b.WriteString("\n" + l)
	}
	b.WriteString("\n──\n")
	for i, h := range groups {
		fmt.Fprintf(&b, "[%d] %s", i+1, textLink(groupLabel(h), h, o))
		if n := qa.Note(h.Candidate); n != "" {
			b.WriteString("（" + n + "）")
		}
		b.WriteString("\n")
	}
	b.WriteString(AnswerNote)
	return b.String()
}

// fitAnswer：整理過的回答超過單則上限時，縮短回答本文，出處與提示保留。
func fitAnswer(r *qa.Result, o Options) string {
	s := reply(r, nil, o, len(r.Hits))
	over := utf8.RuneCountInString(s) - o.MaxLen
	if over <= 0 {
		return s
	}
	text := []rune(r.Answer.Text)
	body := string(text[:max(0, len(text)-over-1)]) + "…"
	return truncate(answerReply(r, o, body), o.MaxLen)
}

// missingLine 是「文件沒寫」的那一行。模型的句子已經以「文件」開頭（例如「文件沒寫明……」）就原樣用，
// 否則補上「文件沒寫：」；不去切模型的句子，免得切到詞中間。
func missingLine(m string) string {
	m = strings.TrimSpace(m)
	switch {
	case m == "":
		return ""
	case strings.HasPrefix(m, "文件"):
		return "⚠ " + m
	}
	return "⚠ 文件沒寫：" + m
}
