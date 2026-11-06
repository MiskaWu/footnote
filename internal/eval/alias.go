package eval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/miskawu/footnote/internal/glossary"
	"github.com/miskawu/footnote/internal/qa"
)

// AliasAsk 是一題要查得到的問題；Source 是答案所在的分頁（project_path:path#分頁），要自己讀過文件才寫得出來。
type AliasAsk struct {
	Q      string `json:"q"`
	Source string `json:"source"`
}

// AliasInput 是審核一個詞彙表提案的材料。
type AliasInput struct {
	Before, After [][]string // 線上版、提案版
	Aliases       []string   // 這次新加的叫法：每個都要有 Asks 與 Traps 提到它
	Asks          []AliasAsk // 補上叫法之後要查得到
	Traps         []string   // 用這個叫法問、文件沒有答案的問題：補上之後仍要回沒有資料
	Items         []Item     // 評估集
	K             int
}

type AliasAskResult struct {
	AliasAsk
	Query  string  `json:"query"`  // 補上叫法之後實際拿去檢索的字串
	Before float32 `json:"before"` // 線上版的最高分
	Top    float32 `json:"top"`    // 提案版的最高分
	First  string  `json:"first"`  // 提案版的第一名
	OK     bool    `json:"ok"`
}

type AliasTrapResult struct {
	Q       string  `json:"q"`
	Query   string  `json:"query"`
	Top     float32 `json:"top"`
	Rescued bool    `json:"rescued"` // 第一次沒過、被翻譯補查救成答案
	OK      bool    `json:"ok"`
}

// AliasReport 是檢查結果；Problems 是空的才算通過。
type AliasReport struct {
	Problems []string          `json:"problems"`
	Asks     []AliasAskResult  `json:"asks"`
	Traps    []AliasTrapResult `json:"traps"`
	Total    int               `json:"eval_total"`    // 評估集題數
	Affected int               `json:"eval_affected"` // 其中問題文字會被這次改動影響的題數
	Items    []string          `json:"eval_items"`    // 受影響的題目
	Worse    []string          `json:"eval_worse"`    // 變差的題目
}

func (r *AliasReport) OK() bool { return len(r.Problems) == 0 }

// Evidence 是留在 kb_changes 的紀錄：核准時憑的數字，負責人事後要看、要撤回都靠它。
func (r *AliasReport) Evidence() string {
	var parts []string
	for _, a := range r.Asks {
		parts = append(parts, fmt.Sprintf("問「%s」%.3f ⇒ %.3f，第一名 %s", a.Q, a.Before, a.Top, a.First))
	}
	for _, t := range r.Traps {
		parts = append(parts, fmt.Sprintf("陷阱「%s」%.3f 回沒有資料", t.Q, t.Top))
	}
	parts = append(parts, fmt.Sprintf("評估集 %d 題有 %d 題受影響、%d 題變差", r.Total, r.Affected, len(r.Worse)))
	return strings.Join(parts, "；")
}

// CheckAlias 跑提案版詞彙表的程式檢查。走跟線上同一條路（qa.Engine），分數是實際展開之後量到的，不是想像的字串：
// 代理憑想像改寫的叫法看起來會過，真的展開重跑常常過不了。
//
// 分數分不出叫法對錯（意思不對的叫法也可能過門檻），所以這裡只擋得住「沒效果」與「把沒資料變成有答案」，
// 叫法到底對不對要靠審核代理讀文件。Asks 與 Traps 由審核代理自己寫，提案者影響不到。
func CheckAlias(ctx context.Context, e *qa.Engine, in AliasInput) (*AliasReport, error) {
	if e.Rewriter == nil {
		return nil, errors.New("這個檢查要跑翻譯補查，線上的陷阱題才驗得準：要有 anthropic.rewrite_model 與 ANTHROPIC_API_KEY")
	}
	r := &AliasReport{Total: len(in.Items)}
	for _, a := range in.Aliases {
		if !slices.ContainsFunc(in.Asks, func(x AliasAsk) bool { return glossary.Mentions(x.Q, a) }) {
			r.Problems = append(r.Problems, fmt.Sprintf("「%s」沒有 --ask：要用提問者原話驗證查得到", a))
		}
		if !slices.ContainsFunc(in.Traps, func(q string) bool { return glossary.Mentions(q, a) }) {
			r.Problems = append(r.Problems, fmt.Sprintf("「%s」沒有 --trap：要有一題用這個叫法問、文件沒有答案的問題", a))
		}
	}
	for _, a := range in.Asks {
		if !strings.Contains(a.Source, "#") {
			r.Problems = append(r.Problems, fmt.Sprintf("「%s」的出處 %q 要寫到分頁（#分頁）", a.Q, a.Source))
		}
	}
	if len(r.Problems) > 0 { // 題目不齊就不花檢索與模型的錢
		return r, nil
	}
	before, after := glossary.New(in.Before), glossary.New(in.After)
	if err := checkAsks(ctx, e, before, after, in.Asks, r); err != nil {
		return nil, err
	}
	if err := checkTraps(ctx, e, after, in.Traps, r); err != nil {
		return nil, err
	}
	if err := checkItems(ctx, e, before, after, in, r); err != nil {
		return nil, err
	}
	return r, nil
}

// checkAsks：提案版要過閘門 A、第一名是答案分頁；只看第一次檢索，靠補查才過的不算，加詞彙就是要省掉補查。
// 線上版本來就查得到的題目證明不了這個叫法有用。
func checkAsks(ctx context.Context, e *qa.Engine, before, after *glossary.Glossary, asks []AliasAsk, r *AliasReport) error {
	cache := map[string][]string{}
	for _, a := range asks {
		sources := []string{a.Source}
		was, err := e.Attempt(ctx, before, nil, a.Q, nil)
		if err != nil {
			return fmt.Errorf("%q: %w", a.Q, err)
		}
		wasRank, _, err := rankOf(ctx, e.Store, was.Ranked, sources, cache)
		if err != nil {
			return fmt.Errorf("%q: %w", a.Q, err)
		}
		now, err := e.Attempt(ctx, after, nil, a.Q, nil)
		if err != nil {
			return fmt.Errorf("%q: %w", a.Q, err)
		}
		rank, top, err := rankOf(ctx, e.Store, now.Ranked, sources, cache)
		if err != nil {
			return fmt.Errorf("%q: %w", a.Q, err)
		}
		res := AliasAskResult{AliasAsk: a, Query: now.Query, Before: was.Top, Top: now.Top}
		if len(top) > 0 {
			res.First = top[0]
		}
		switch {
		case was.Status != qa.StatusNoData && wasRank == 1:
			r.Problems = append(r.Problems, fmt.Sprintf("「%s」沒補這個叫法也查得到（%.3f、第一名就是答案），證明不了它有用", a.Q, was.Top))
		case now.Status == qa.StatusNoData:
			r.Problems = append(r.Problems, fmt.Sprintf("「%s」補上之後還是沒過閘門 A（%.3f）", now.Query, now.Top))
		case rank != 1:
			r.Problems = append(r.Problems, fmt.Sprintf("「%s」第一名不是答案分頁：%s", now.Query, res.First))
		default:
			res.OK = true
		}
		r.Asks = append(r.Asks, res)
	}
	return nil
}

// checkTraps：提案版跑完整的查詢（含翻譯補查），要回沒有資料。補查沒跑起來就是沒驗到，不能算過。
func checkTraps(ctx context.Context, e *qa.Engine, after *glossary.Glossary, traps []string, r *AliasReport) error {
	for _, q := range traps {
		res, err := e.Search(ctx, after, nil, q, nil)
		if err != nil {
			return fmt.Errorf("%q: %w", q, err)
		}
		t := AliasTrapResult{Q: q, Query: res.Query, Top: res.Top}
		rs := res.Rescue
		t.Rescued = rs != nil && rs.Rescued
		switch {
		case res.Status != qa.StatusNoData:
			r.Problems = append(r.Problems, fmt.Sprintf("陷阱題「%s」沒有回沒有資料（%.3f）：把沒資料的問題變成有答案", res.Query, res.Top))
		case rs != nil && (rs.Skipped == "budget" || rs.Skipped == "error"):
			r.Problems = append(r.Problems, fmt.Sprintf("陷阱題「%s」的翻譯補查沒跑成（%s），這題沒驗到", q, rs.Skipped))
		default:
			t.OK = true
		}
		r.Traps = append(r.Traps, t)
	}
	return nil
}

// checkItems 確認評估集沒有退步。只重跑問題文字會被這次改動影響的題目：詞彙表只改查詢字串，
// 字串沒變的題目結果也不會變；全跑一次要好幾分鐘，兩個版本又要各跑一次。
func checkItems(ctx context.Context, e *qa.Engine, before, after *glossary.Glossary, in AliasInput, r *AliasReport) error {
	var hit []Item
	for _, it := range in.Items {
		if slices.ContainsFunc(append([]string{it.Q}, it.History...), func(s string) bool { return before.Expand(s) != after.Expand(s) }) {
			hit = append(hit, it)
			r.Items = append(r.Items, it.Q)
		}
	}
	r.Affected = len(hit)
	if len(hit) == 0 {
		return nil
	}
	was, err := runWith(ctx, e, before, hit, nil)
	if err != nil {
		return err
	}
	now, err := runWith(ctx, e, after, hit, nil)
	if err != nil {
		return err
	}
	for i := range hit {
		if why := worse(was[i], now[i], e.Cfg.TauAnswer, e.Cfg.TauFollow, in.K); why != "" {
			line := fmt.Sprintf("「%s」%s", hit[i].Q, why)
			r.Worse = append(r.Worse, line)
			r.Problems = append(r.Problems, "評估集退步："+line)
		}
	}
	return nil
}

// worse 比較同一題在兩個版本的結果，回傳退步的原因；沒退步回傳空字串。本來就錯的題目不算退步。
func worse(was, now ItemResult, tau, tauFollow float64, k int) string {
	wasPass, wasRank := was.At(tau, tauFollow)
	nowPass, nowRank := now.At(tau, tauFollow)
	if was.Expect != "answer" {
		if !wasPass && nowPass {
			return "本來回沒有資料，現在會回答"
		}
		return ""
	}
	switch {
	case wasPass && !nowPass:
		return "本來回答得出來，現在被擋下"
	case wasRank > 0 && wasRank <= k && (nowRank == 0 || nowRank > k):
		return fmt.Sprintf("答案從第 %d 名掉出前 %d 名", wasRank, k)
	}
	return ""
}
