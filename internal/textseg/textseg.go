// Package textseg 把文字切成全文檢索用的詞：ASCII 詞轉小寫，CJK 連續字切 bigram。
// 切完的詞以空白串起來交給 Postgres 的 'simple' 設定，查詢端用同一套切法，兩邊才對得上。
package textseg

import (
	"strings"
	"unicode"
)

// Tokens 回傳 s 的詞，依出現順序，可能重複。
func Tokens(s string) []string {
	var out []string
	var word []rune
	var cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, strings.ToLower(string(word)))
			word = word[:0]
		}
	}
	flushCJK := func() {
		switch {
		case len(cjk) == 1:
			out = append(out, string(cjk))
		case len(cjk) > 1:
			for i := 0; i+1 < len(cjk); i++ {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range s {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

// Document 回傳交給 to_tsvector('simple', …) 的字串。
func Document(s string) string { return strings.Join(Tokens(s), " ") }

// Query 回傳交給 websearch_to_tsquery('simple', …) 的字串：去重後以 or 串起來。
func Query(s string) string {
	seen := map[string]bool{}
	var uniq []string
	for _, t := range Tokens(s) {
		if !seen[t] {
			seen[t] = true
			uniq = append(uniq, t)
		}
	}
	return strings.Join(uniq, " or ")
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}
