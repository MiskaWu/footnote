// Package eval 跑評估集並掃閾值。檢索與重排序每題只跑一次，τ 用存下的分數在本機掃。
package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/miskawu/footnote/internal/glossary"
	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/store"
)

type Item struct {
	Q       string   `yaml:"q" json:"q"`
	Expect  string   `yaml:"expect" json:"expect"`             // answer | no_data | general
	Sources []string `yaml:"sources" json:"sources,omitempty"` // project_path:path[#分頁或標題]，命中任一即可；FAQ 卡引用了其中之一也算
	History []string `yaml:"history" json:"history,omitempty"` // 討論串追問：同串前面的問題（跟線上一樣串在本題前面檢索）
}

type ItemResult struct {
	Item
	Top     float32  `json:"top"`
	Follow  *float32 `json:"follow,omitempty"` // 追問題：本題單獨的分數（qa.FollowScore）
	Rank    int      `json:"rank"`             // 第一個命中來源的名次（1 起算），0 表示前 candidates 名都沒有
	TopHits []string `json:"top_hits"`
	Rescue  *Rescue  `json:"rescue,omitempty"` // 翻譯補查；有設改寫模型時每題都試，τ 才掃得準（線上只在沒過閘門 A 時做）
}

// Rescue 是一題翻譯補查的結果：改寫後再跑一次跟線上同一條的 Attempt。
type Rescue struct {
	Query        string   `json:"query"`
	Top          float32  `json:"top"`
	Follow       *float32 `json:"follow,omitempty"`
	Rank         int      `json:"rank"`
	TopHits      []string `json:"top_hits"`
	Skipped      string   `json:"skipped,omitempty"` // error（呼叫失敗或結果不能用）、unchanged（模型沒補任何字）
	Model        string   `json:"model,omitempty"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	LatencyMS    int64    `json:"latency_ms"`
}

type Summary struct {
	Tau           float64 `json:"tau"`        // τ_answer
	TauFollow     float64 `json:"tau_follow"` // 追問題本題單獨的門檻
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

// Run 每題跑一次跟線上同一條的 qa.Attempt（展開別名、檢索、重排、追問分數）；有設改寫模型時，
// 再跑一次翻譯補查（qa.RewriteQuestion 加 Attempt）。分數都存下來，τ 在本機掃。
func Run(ctx context.Context, e *qa.Engine, items []Item, progress io.Writer) ([]ItemResult, error) {
	g, err := e.Glossary(ctx)
	if err != nil {
		return nil, err
	}
	return runWith(ctx, e, g, items, progress)
}

// runWith 用指定的詞彙表跑評估集：審核提案時要比較線上版與提案版。
func runWith(ctx context.Context, e *qa.Engine, g *glossary.Glossary, items []Item, progress io.Writer) ([]ItemResult, error) {
	out := make([]ItemResult, 0, len(items))
	faqRefs := map[string][]string{} // 卡片 project:path → sources，多題命中同一張卡時不重查
	for i, it := range items {
		first, err := e.Attempt(ctx, g, it.History, it.Q, nil)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", it.Q, err)
		}
		r := ItemResult{Item: it, Top: first.Top, Follow: first.Follow}
		if r.Rank, r.TopHits, err = rankOf(ctx, e.Store, first.Ranked, it.Sources, faqRefs); err != nil {
			return nil, fmt.Errorf("%q: %w", it.Q, err)
		}
		if e.Rewriter != nil {
			if r.Rescue, err = rescue(ctx, e, g, first, it.Sources, faqRefs); err != nil {
				return nil, fmt.Errorf("%q: %w", it.Q, err)
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

// rescue 對一題做翻譯補查。first 是第一次的結果（history 與 question 已展開別名）。
func rescue(ctx context.Context, e *qa.Engine, g *glossary.Glossary, first *qa.Result, sources []string, cache map[string][]string) (*Rescue, error) {
	hist, q, rw, err := e.RewriteQuestion(ctx, first.History(), first.Question())
	rs := &Rescue{}
	if rw != nil {
		rs.Model, rs.InputTokens, rs.OutputTokens, rs.LatencyMS = rw.Model, rw.InputTokens, rw.OutputTokens, rw.Latency.Milliseconds()
	}
	switch {
	case err != nil:
		rs.Skipped = "error"
		return rs, nil
	case q == first.Question() && slices.Equal(hist, first.History()):
		rs.Skipped = "unchanged"
		return rs, nil
	}
	r, err := e.Attempt(ctx, g, hist, q, nil)
	if err != nil {
		return nil, err
	}
	rs.Query, rs.Top, rs.Follow = r.Query, r.Top, r.Follow
	rs.Rank, rs.TopHits, err = rankOf(ctx, e.Store, r.Ranked, sources, cache)
	return rs, err
}

// rankOf 回傳第一個命中預期出處的名次（0 表示沒有）與前三名的摘要。
func rankOf(ctx context.Context, s qa.Searcher, ranked []qa.Hit, sources []string, cache map[string][]string) (int, []string, error) {
	rank := 0
	var top []string
	for j, h := range ranked {
		if j < 3 {
			top = append(top, fmt.Sprintf("%.3f %s:%s#%s", h.Score, h.ProjectPath, h.Path, h.Heading))
		}
		if rank > 0 {
			continue
		}
		refs, err := provenance(ctx, s, h, cache)
		if err != nil {
			return 0, nil, err
		}
		if matches(sources, refs) {
			rank = j + 1
		}
	}
	return rank, top, nil
}

// provenance 是一個檢索結果代表的出處（project_path:path#anchor）。文件片段是它自己；
// FAQ 卡是卡片本身，加上卡片 sources 列的每一筆——卡片的答案就是從那些出處整理來的。
func provenance(ctx context.Context, s qa.Searcher, h qa.Hit, cache map[string][]string) ([]string, error) {
	self := h.ProjectPath + ":" + h.Path + "#" + h.Heading
	if h.Kind != "faq" {
		return []string{self}, nil
	}
	key := h.ProjectPath + ":" + h.Path
	refs, ok := cache[key]
	if !ok {
		f, err := s.GetFAQ(ctx, h.ProjectPath, h.Path)
		switch {
		case errors.Is(err, store.ErrNotFound): // 片段在、卡片列不在：只算卡片本身
		case err != nil:
			return nil, err
		default:
			for _, src := range f.Sources {
				refs = append(refs, src.Ref)
			}
		}
		cache[key] = refs
	}
	return append([]string{self}, refs...), nil
}

// matches：預期出處有寫 anchor 時，anchor 也要相同。FAQ 卡只引用整個檔案、預期出處寫到分頁時不算命中，
// 跟文件片段同一把尺（只寫檔名會讓同檔案裡無關的分頁也算數）。
func matches(sources, refs []string) bool {
	for _, s := range sources {
		main, anchor, hasAnchor := strings.Cut(s, "#")
		for _, ref := range refs {
			rmain, ranchor, _ := strings.Cut(ref, "#")
			if main == rmain && (!hasAnchor || anchor == ranchor) {
				return true
			}
		}
	}
	return false
}

// Pass：這題在這組門檻下會不會通過閘門 A。跟線上走同一個判斷（qa.Gate）。
func (r ItemResult) Pass(tau, tauFollow float64) bool {
	ok, _ := r.At(tau, tauFollow)
	return ok
}

// At 是這題在這組門檻下的結果：第一次就過閘門，用第一次的名次；沒過但翻譯補查過了，用補查的名次
// （跟線上一樣，補查只在第一次沒過時才用）。都沒過時回傳 false 與第一次的名次。
func (r ItemResult) At(tau, tauFollow float64) (pass bool, rank int) {
	if ok, _ := qa.Gate(r.Top, r.Follow, tau, tauFollow); ok {
		return true, r.Rank
	}
	if rs := r.Rescue; rs != nil && rs.Skipped == "" {
		if ok, _ := qa.Gate(rs.Top, rs.Follow, tau, tauFollow); ok {
			return true, rs.Rank
		}
	}
	return false, r.Rank
}

// Rescued：這題在這組門檻下是靠翻譯補查才通過的。
func (r ItemResult) Rescued(tau, tauFollow float64) bool {
	first, _ := qa.Gate(r.Top, r.Follow, tau, tauFollow)
	pass, _ := r.At(tau, tauFollow)
	return pass && !first
}

func Summarize(rs []ItemResult, tau, tauFollow float64, k int) Summary {
	s := Summary{Tau: tau, TauFollow: tauFollow}
	var mrr float64
	recalled := 0
	for _, r := range rs {
		pass, rank := r.At(tau, tauFollow)
		if r.Expect == "answer" {
			s.AnswerN++
			if rank > 0 {
				mrr += 1 / float64(rank)
				if rank <= k {
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

// Sweep 掃 τ_answer 0.05..0.80，每 0.05 一格（τ_follow 固定）。
func Sweep(rs []ItemResult, tauFollow float64, k int) []Summary {
	var out []Summary
	for i := 1; i <= 16; i++ {
		out = append(out, Summarize(rs, float64(i)*0.05, tauFollow, k))
	}
	return out
}

// SweepFollow 只看追問題，掃 τ_follow 0.05..0.80（τ_answer 固定）。沒有追問題時回傳 nil。
func SweepFollow(rs []ItemResult, tau float64, k int) []Summary {
	var follow []ItemResult
	for _, r := range rs {
		if r.Follow != nil {
			follow = append(follow, r)
		}
	}
	if len(follow) == 0 {
		return nil
	}
	var out []Summary
	for i := 1; i <= 16; i++ {
		out = append(out, Summarize(follow, tau, float64(i)*0.05, k))
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
