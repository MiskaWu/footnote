package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/miskawu/footnote/internal/eval"
	"github.com/miskawu/footnote/internal/llm"
	"github.com/miskawu/footnote/internal/qa"
)

type compareDoc struct {
	Source string  `json:"source"`
	Note   string  `json:"note,omitempty"`
	Score  float32 `json:"score"`
}

type compareRow struct {
	Q       string                 `json:"q"`
	Expect  string                 `json:"expect,omitempty"`
	Gate    string                 `json:"gate"` // docs | faq | no_data：現行閘門 A 的結果；只有 docs 會送模型
	Top     float32                `json:"top"`
	Docs    []compareDoc           `json:"docs,omitempty"`
	Answers map[string]*llm.Answer `json:"answers,omitempty"`
	CostUSD map[string]float64     `json:"cost_usd,omitempty"`
	Errors  map[string]string      `json:"errors,omitempty"`
}

// cmdCompare 用同一份檢索結果讓幾個模型各回答一次，比較品質、花費與速度（決定 MVP 用哪個模型）。
func (a *app) cmdCompare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	file := fs.String("f", "eval/compare.example.yaml", "題目（跟 eval.yaml 同格式，可帶 history）")
	models := fs.String("models", "claude-haiku-5-5,claude-sonnet-5-5,claude-opus-5-5", "逗號分隔的模型")
	effort := fs.String("effort", "low", "low | medium | high")
	out := fs.String("out", "", "結果存成 JSON")
	fs.Parse(args)
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return errors.New("ANTHROPIC_API_KEY 未設定（放在 ~/.config/footnote/secrets.env，跑之前 set -a; . 該檔; set +a）")
	}
	items, err := eval.Load(*file)
	if err != nil {
		return err
	}
	// 這裡只要檢索與閘門 A 的結果；每個模型由下面各自呼叫一次，不讓引擎自己先整理（否則會多花一次、狀態也不再是 docs）
	a.engine.Gen, a.engine.Budget = nil, nil
	var clients []*llm.Client
	for _, m := range strings.Split(*models, ",") {
		clients = append(clients, llm.New(key, strings.TrimSpace(m), *effort, 8000, a.cfg.Anthropic.Style)) // 用設定檔的寫法，比較才貼近上線
	}
	var rows []compareRow
	for _, it := range items {
		res, err := a.engine.Run(ctx, it.Q, it.History, nil)
		if err != nil {
			return fmt.Errorf("%q: %w", it.Q, err)
		}
		row := compareRow{Q: it.Q, Expect: it.Expect, Gate: res.Status, Top: res.Top}
		fmt.Printf("\n■ %s（閘門 A：%s %.3f）\n", it.Q, res.Status, res.Top)
		if res.Status != qa.StatusDocs {
			rows = append(rows, row)
			continue
		}
		hits := qa.GenHits(res.Ranked, a.cfg.QA.TauKeep, a.cfg.QA.KMax)
		docs := qa.GenDocs(hits)
		for i, d := range docs {
			row.Docs = append(row.Docs, compareDoc{Source: d.Source, Note: d.Note, Score: hits[i].Score})
		}
		row.Answers, row.CostUSD, row.Errors = map[string]*llm.Answer{}, map[string]float64{}, map[string]string{}
		for _, c := range clients {
			ans, err := c.Answer(ctx, it.Q, docs)
			if err != nil {
				row.Errors[c.Model] = err.Error()
				fmt.Printf("  %-18s 錯誤：%v\n", c.Model, err)
				continue
			}
			cost := llm.Cost(c.Model, ans.InputTokens, ans.OutputTokens)
			row.Answers[c.Model], row.CostUSD[c.Model] = ans, cost
			fmt.Printf("  %-18s %-8s %5.1fs  in=%d out=%d  $%.4f  %s\n", c.Model, ans.Status, ans.Latency.Seconds(),
				ans.InputTokens, ans.OutputTokens, cost, firstLine(ans.Text))
		}
		rows = append(rows, row)
	}
	if *out != "" {
		b, _ := json.MarshalIndent(map[string]any{"date": time.Now().Format(time.RFC3339), "effort": *effort, "rows": rows}, "", "  ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
		fmt.Println("\n結果存到", *out)
	}
	return nil
}

func firstLine(s string) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return string(r)
}
