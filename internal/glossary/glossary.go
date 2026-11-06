// Package glossary 是別名詞彙表：同一件事的不同叫法（例：回滾、rollback）。檔案放在 FAQ 來源根目錄的
// glossary.yaml，同步時整份收進資料庫；查詢時問題裡出現別名，就把文件的正式名稱（每組第一個）用括號補在它後面，
// 再拿去檢索與重排。文件側不動，改詞彙表不必重建索引。
package glossary

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Path 是詞彙表在 FAQ 來源裡的固定路徑。
const Path = "glossary.yaml"

// MinRunes 是一個叫法最短的字數。單字會在問題裡到處命中，補上去的詞反而把檢索帶偏。
const MinRunes = 2

// Parse 解析詞彙表：最外層是清單，每一項是一組叫法（字串清單）。結構不對回傳 error；內容規則由 Lint 檢查。
func Parse(content []byte) ([][]string, error) {
	var groups [][]string
	if err := yaml.Unmarshal(content, &groups); err != nil {
		return nil, fmt.Errorf("glossary：%w（格式是一組一行，例：- [回滾, rollback]）", err)
	}
	for i, g := range groups {
		for j, t := range g {
			groups[i][j] = strings.TrimSpace(t)
		}
	}
	return groups, nil
}

// Lint 回傳所有問題：每組至少兩個叫法、每個叫法至少 MinRunes 個字、同一個叫法（不分大小寫）只能出現一次。
// 一個叫法屬於兩組時，補上去的詞會把兩件事混在一起，所以有歧義的叫法不能放進來。
func Lint(groups [][]string) []string {
	var probs []string
	seen := map[string]int{} // 小寫叫法 → 第幾組（1 起算）
	for i, g := range groups {
		n := i + 1
		if len(g) < 2 {
			probs = append(probs, fmt.Sprintf("第 %d 組至少要兩個叫法", n))
		}
		for _, t := range g {
			if t == "" {
				probs = append(probs, fmt.Sprintf("第 %d 組有空的叫法", n))
				continue
			}
			if utf8.RuneCountInString(t) < MinRunes {
				probs = append(probs, fmt.Sprintf("第 %d 組的「%s」太短（至少 %d 個字）", n, t, MinRunes))
			}
			key := asciiLower(t)
			if prev, ok := seen[key]; ok {
				if prev == n {
					probs = append(probs, fmt.Sprintf("第 %d 組的「%s」重複", n, t))
				} else {
					probs = append(probs, fmt.Sprintf("「%s」同時出現在第 %d 組和第 %d 組；同一個叫法只能屬於一組", t, prev, n))
				}
				continue
			}
			seen[key] = n
		}
	}
	sort.Strings(probs)
	return probs
}

// Change 是一次提案對詞彙表的改動：新增一組，或在既有的一組加上新叫法。
type Change struct {
	Group []string // 改動之後的整組
	Added []string // 這次新加的別名；新的一組時不含第一個（第一個寫文件裡的正式名稱）
	New   bool     // 整組都是新的
}

// Compare 比對兩版詞彙表，回傳唯一的一組改動。審核代理只能核准「一次加一組」的提案：
// 拿掉叫法、併組、一次改好幾組都回傳 error，這些要人審。線上內容在提案之後變了也會被擋下來——
// 提案是整份內容，線上多出來的叫法在提案裡找不到，就算成「拿掉」。
func Compare(before, after [][]string) (*Change, error) {
	owner := func(groups [][]string) map[string]int {
		m := map[string]int{}
		for i, g := range groups {
			for _, t := range g {
				m[asciiLower(t)] = i
			}
		}
		return m
	}
	was, now := owner(before), owner(after)
	for i, g := range before {
		for _, t := range g {
			j, ok := now[asciiLower(t)]
			switch {
			case !ok:
				return nil, fmt.Errorf("提案裡沒有線上的「%s」：提案不能拿掉叫法；線上在提案之後改過的話要重新提案", t)
			case j != now[asciiLower(g[0])]:
				return nil, fmt.Errorf("提案把第 %d 組拆開了", i+1)
			}
		}
	}
	var changes []*Change
	for _, g := range after {
		ch := &Change{Group: g}
		from := -1
		for _, t := range g {
			i, ok := was[asciiLower(t)]
			if !ok {
				ch.Added = append(ch.Added, t)
				continue
			}
			if from >= 0 && from != i {
				return nil, fmt.Errorf("提案把線上的兩組併在一起（「%s」那組）", t)
			}
			from = i
		}
		if len(ch.Added) == 0 {
			continue
		}
		if ch.New = from < 0; ch.New {
			ch.Added = ch.Added[1:]
		}
		changes = append(changes, ch)
	}
	switch len(changes) {
	case 0:
		return nil, errors.New("提案沒有新增任何叫法")
	case 1:
		return changes[0], nil
	}
	return nil, fmt.Errorf("提案改了 %d 組，一次只能改一組：每組要各自審、各自撤回", len(changes))
}

// Mentions 回報 s 裡有沒有出現這個叫法，比對規則跟 Expand 一樣。
func Mentions(s, name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && find(asciiLower(s), term{lower: asciiLower(name), word: !hasCJK(name)}) >= 0
}

// Glossary 是查詢時用的詞彙表。nil 表示沒有詞彙表，Expand 原樣回傳。
type Glossary struct {
	groups [][]term
}

type term struct {
	text  string
	lower string // 只轉 ASCII 小寫，位元組長度不變，找到的位置能直接對回原字串
	word  bool   // 不含中日韓字：要整個詞對上（rollback 不能命中 rollbacks）
}

// New 建立詞彙表；空的叫法略過（入庫前已經過 Lint）。
func New(groups [][]string) *Glossary {
	g := &Glossary{}
	for _, grp := range groups {
		var ts []term
		for _, t := range grp {
			if t = strings.TrimSpace(t); t == "" {
				continue
			}
			ts = append(ts, term{text: t, lower: asciiLower(t), word: !hasCJK(t)})
		}
		if len(ts) >= 2 {
			g.groups = append(g.groups, ts)
		}
	}
	return g
}

// Expand 把問題裡的別名補上文件的正式名稱（每組第一個），例：「rollback 的權限」⇒「rollback（回滾）的權限」。
// 只有這一個方向：文件寫的是正式名稱，已經用正式名稱問的問題本來就查得到，再補別名只會拉低重排分數。
// 雙向展開實測會拉低重排分數，原本過得了閘門 A 的問題反而掉下去。
// 補在第一個出現的別名後面：實測補在原位比補在句尾重排分數高。問題裡已經有正式名稱就不補，
// 所以重複呼叫結果不變。英文叫法要整個詞對上、不分大小寫；含中文的叫法照字面找。
func (g *Glossary) Expand(s string) string {
	if g == nil || len(g.groups) == 0 {
		return s
	}
	lower := asciiLower(s)
	type insert struct {
		at   int
		text string
	}
	var ins []insert
	for _, grp := range g.groups {
		formal := grp[0]
		if find(lower, formal) >= 0 {
			continue
		}
		first, end := -1, -1
		for _, t := range grp[1:] {
			if pos := find(lower, t); pos >= 0 && (first < 0 || pos < first) {
				first, end = pos, pos+len(t.lower)
			}
		}
		if first >= 0 {
			ins = append(ins, insert{end, "（" + formal.text + "）"})
		}
	}
	if len(ins) == 0 {
		return s
	}
	sort.SliceStable(ins, func(i, j int) bool { return ins[i].at < ins[j].at })
	var b strings.Builder
	prev := 0
	for _, x := range ins {
		b.WriteString(s[prev:x.at])
		b.WriteString(x.text)
		prev = x.at
	}
	b.WriteString(s[prev:])
	return b.String()
}

// find 回傳 t 在 lower 裡第一個（英文叫法要前後都不是英數字的）位置，找不到回傳 -1。
func find(lower string, t term) int {
	for from := 0; from <= len(lower); {
		i := strings.Index(lower[from:], t.lower)
		if i < 0 {
			return -1
		}
		pos := from + i
		end := pos + len(t.lower)
		if !t.word || (!wordBefore(lower, pos) && !wordAfter(lower, end)) {
			return pos
		}
		_, size := utf8.DecodeRuneInString(lower[pos:])
		from = pos + size
	}
	return -1
}

func wordBefore(s string, i int) bool {
	if i == 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return isWordRune(r)
}

func wordAfter(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return isWordRune(r)
}

// isWordRune：英數字（不含中日韓字）。「rollback的」裡的「的」不算同一個詞。
func isWordRune(r rune) bool {
	return (unicode.IsLetter(r) || unicode.IsDigit(r)) && !isCJK(r)
}

func hasCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// isCJK 與 textseg 的定義相同。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// asciiLower 只把 A–Z 轉小寫。strings.ToLower 會改變部分非 ASCII 字的位元組長度，位置就對不回原字串。
func asciiLower(s string) string {
	b := []byte(s)
	changed := false
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
			changed = true
		}
	}
	if !changed {
		return s
	}
	return string(b)
}
