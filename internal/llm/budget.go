package llm

import (
	"context"
	"time"
)

// Prices 是每百萬 token 的牌價（美元）：[輸入, 輸出]。2026-10 的價格；
// 預算與比較表都用這一份。Haiku 5.5 的價格是提示 ≤ 100K token 的檔位，footnote 的提示遠小於此。
var Prices = map[string][2]float64{
	"claude-haiku-5-5":  {0.10, 0.50},
	"claude-sonnet-5-5": {2, 10},
	"claude-opus-5-5":   {4, 20},
}

// Cost 估算一次呼叫的花費。不在價格表的模型用表上最貴的價格算，寧可高估讓預算提早擋下。
func Cost(model string, in, out int64) float64 {
	p, ok := Prices[model]
	if !ok {
		for _, q := range Prices {
			p[0], p[1] = max(p[0], q[0]), max(p[1], q[1])
		}
	}
	return float64(in)/1e6*p[0] + float64(out)/1e6*p[1]
}

// UsageFunc 回傳某時間點之後各模型累計的 [輸入, 輸出] token（由 asks 表加總）。
type UsageFunc func(ctx context.Context, since time.Time) (map[string][2]int64, error)

// Budget 是全域每月預算上限：本月累計花費 ≥ MonthlyUSD 就不再呼叫模型，回覆退回原文摘錄。
type Budget struct {
	MonthlyUSD float64
	Usage      UsageFunc
	Now        func() time.Time // 測試用；nil 用 time.Now
}

func (b *Budget) Spent(ctx context.Context) (float64, error) {
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	t := now()
	u, err := b.Usage(ctx, time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()))
	if err != nil {
		return 0, err
	}
	var sum float64
	for model, tok := range u {
		sum += Cost(model, tok[0], tok[1])
	}
	return sum, nil
}

func (b *Budget) Allow(ctx context.Context) (bool, error) {
	if b.MonthlyUSD <= 0 {
		return true, nil
	}
	spent, err := b.Spent(ctx)
	if err != nil {
		return false, err
	}
	return spent < b.MonthlyUSD, nil
}
