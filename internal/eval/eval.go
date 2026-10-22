// Package eval 跑評估集並掃閾值（plan §8）。檢索與重排序每題只跑一次，τ 用存下的分數在本機掃。
package eval

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/miskawu/footnote/internal/qa"
)

type Item struct {
	Q       string   `yaml:"q" json:"q"`
	Expect  string   `yaml:"expect" json:"expect"`             // answer | no_data | general
	Sources []string `yaml:"sources" json:"sources,omitempty"` // project_path:path[#分頁或標題]，命中任一即可
}

type ItemResult struct {
	Item
	Top     float32  `json:"top"`
	Rank    int      `json:"rank"` // 第一個命中來源的名次（1 起算），0 表示前 candidates 名都沒有
	TopHits []string `json:"top_hits"`
}

type Summary struct {
	Tau           float64 `json:"tau"`
	AnswerN       int     `json:"answer_n"`
	NegativeN     int     `json:"negative_n"` // no_data + general
	RecallAtK     float64 `json:"recall_at_k"`
	MRR           float64 `json:"mrr"`
	MisleadingN   int     `json:"misleading_n"`    // 該拒答卻通過閘門 A
	FalseRefusalN int     `json:"false_refusal_n"` // 該回答卻被閘門 A 擋下
	Misleading    float64 `json:"misleading"`
	FalseRefusal  float64 `json:"false_refusal"`
}

func Load(path string) ([]Item, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []Item
	if err := yaml.Unmarshal(b, &items); err != nil {
		return nil, err
	}
	for i, it := range items {
		switch it.Expect {
		case "answer":
			if len(it.Sources) == 0 {
				return nil, fmt.Errorf("第 %d 題 expect=answer 但沒有 sources", i+1)
			}
		case "no_data", "general":
		default:
			return nil, fmt.Errorf("第 %d 題 expect=%q，應為 answer、no_data 或 general", i+1, it.Expect)
		}
	}
	return items, nil
}

func Run(ctx context.Context, e *qa.Engine, items []Item, progress io.Writer) ([]ItemResult, error) {
	out := make([]ItemResult, 0, len(items))
	for i, it := range items {
		ranked, _, err := e.Retrieve(ctx, it.Q, nil)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", it.Q, err)
		}
		r := ItemResult{Item: it}
		if len(ranked) > 0 {
			r.Top = ranked[0].Score
		}
		for j, h := range ranked {
			if j < 3 {
				r.TopHits = append(r.TopHits, fmt.Sprintf("%.3f %s:%s#%s", h.Score, h.ProjectPath, h.Path, h.Heading))
			}
			if r.Rank == 0 && matches(it.Sources, h) {
				r.Rank = j + 1
			}
		}
		out = append(out, r)
		if progress != nil {
			fmt.Fprintf(progress, "\r%d/%d", i+1, len(items))
		}
	}
	if progress != nil {
		fmt.Fprintln(progress)
	}
	return out, nil
}

func matches(sources []string, h qa.Hit) bool {
	for _, s := range sources {
		main, anchor, hasAnchor := strings.Cut(s, "#")
		if main == h.ProjectPath+":"+h.Path && (!hasAnchor || anchor == h.Heading) {
			return true
		}
	}
	return false
}

func Summarize(rs []ItemResult, tau float64, k int) Summary {
	s := Summary{Tau: tau}
	var mrr float64
	recalled := 0
	for _, r := range rs {
		pass := float64(r.Top) >= tau
		if r.Expect == "answer" {
			s.AnswerN++
			if r.Rank > 0 {
				mrr += 1 / float64(r.Rank)
				if r.Rank <= k {
					recalled++
				}
			}
			if !pass {
				s.FalseRefusalN++
			}
		} else {
			s.NegativeN++
			if pass {
				s.MisleadingN++
			}
		}
	}
	if s.AnswerN > 0 {
		s.RecallAtK = float64(recalled) / float64(s.AnswerN)
		s.MRR = mrr / float64(s.AnswerN)
		s.FalseRefusal = float64(s.FalseRefusalN) / float64(s.AnswerN)
	}
	if s.NegativeN > 0 {
		s.Misleading = float64(s.MisleadingN) / float64(s.NegativeN)
	}
	return s
}

// Sweep 掃 τ_answer 0.05..0.80，每 0.05 一格。
func Sweep(rs []ItemResult, k int) []Summary {
	var out []Summary
	for i := 1; i <= 16; i++ {
		out = append(out, Summarize(rs, float64(i)*0.05, k))
	}
	return out
}

// Pick 選誤導率 ≤ maxMisleading 的 τ 中誤拒率最低的；同樣最低的有好幾個時取中間那個，
// 離兩邊出錯的邊界都最遠（取最小的話，等於挑最靠近開始誤導的值）。找不到回傳 nil。
func Pick(rows []Summary, maxMisleading float64) *Summary {
	var best []*Summary
	for i := range rows {
		r := &rows[i]
		if r.Misleading > maxMisleading {
			continue
		}
		switch {
		case len(best) == 0 || r.FalseRefusal < best[0].FalseRefusal:
			best = []*Summary{r}
		case r.FalseRefusal == best[0].FalseRefusal:
			best = append(best, r)
		}
	}
	if len(best) == 0 {
		return nil
	}
	return best[(len(best)-1)/2]
}
