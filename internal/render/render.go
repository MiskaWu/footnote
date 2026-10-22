// Package render 把問答結果排成回覆文字（Markdown，連結用 masked link）。
// Discord 與本機測試頁共用同一份格式（plan §7.3、§7.8）。
package render

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

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
	GeneralText  = "內部文件沒有相關內容；這看起來是一般技術問題，建議查官方文件或改問通用的 AI 助理。"
	RateText     = "問得有點快，請稍等一下再問。"
	draftTag     = "（草稿）"
	maxQuoteRows = 4
)

// Reply 依結果排出回覆；超過 MaxLen 時先縮短摘錄，再減少筆數。
func Reply(r *qa.Result, gap *GapInfo, o Options) string {
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
		faqLink := Link(o.GitLabURL, h.ProjectPath, h.Ref, h.Path, 0)
		fmt.Fprintf(&b, "FAQ · [%s](<%s>)", esc(h.Path), faqLink)
		if r.FAQ != nil {
			fmt.Fprintf(&b, "（%s 確認）", r.FAQ.Verified.Format("2006-01-02"))
			if r.FAQ.StaleSince != nil {
				b.WriteString("\n⚠ 這張卡引用的檔案在確認後有變動，內容待重新確認。")
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
	default:
		if gap != nil && gap.Status == "dismissed" {
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

// Label 是片段的顯示名稱：專案末段 · 檔名 › 標題（xlsx 附列號）。
func Label(c store.Candidate) string {
	project := c.ProjectPath[strings.LastIndex(c.ProjectPath, "/")+1:]
	s := fmt.Sprintf("%s · %s › %s", project, path.Base(c.Path), c.Heading)
	if isSheet(c.Path) && c.LineStart > 0 {
		if c.LineEnd > c.LineStart {
			s += fmt.Sprintf("（第 %d–%d 列）", c.LineStart, c.LineEnd)
		} else {
			s += fmt.Sprintf("（第 %d 列）", c.LineStart)
		}
	}
	return s
}

func hitLink(h qa.Hit, o Options) string {
	s := fmt.Sprintf("[%s](<%s>)", esc(Label(h.Candidate)), Link(o.GitLabURL, h.ProjectPath, h.Ref, h.Path, h.LineStart))
	if h.Draft {
		s += draftTag
	}
	return s
}

func sourceLink(s store.FAQSourceRow, o Options) string {
	ref := o.Refs[s.SrcProject]
	if ref == "" {
		ref = "main"
	}
	label := s.SrcProject[strings.LastIndex(s.SrcProject, "/")+1:] + " · " + path.Base(s.SrcPath)
	if _, anchor, ok := strings.Cut(s.Ref, "#"); ok && anchor != "" {
		label += " › " + anchor
	}
	return fmt.Sprintf("[%s](<%s>)", esc(label), Link(o.GitLabURL, s.SrcProject, ref, s.SrcPath, 0))
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
