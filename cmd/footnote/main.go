// footnote：內部開發知識問答。子命令見 docs/plan.md §3。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/miskawu/footnote/internal/bot"
	"github.com/miskawu/footnote/internal/bot/discord"
	"github.com/miskawu/footnote/internal/bot/web"
	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/eval"
	"github.com/miskawu/footnote/internal/faq"
	"github.com/miskawu/footnote/internal/gap"
	"github.com/miskawu/footnote/internal/glossary"
	"github.com/miskawu/footnote/internal/ingest"
	"github.com/miskawu/footnote/internal/kb"
	"github.com/miskawu/footnote/internal/llm"
	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/render"
	"github.com/miskawu/footnote/internal/source"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
)

const usage = `用法：footnote [-c footnote.yaml] <指令> [參數]

  migrate                                   建表
  ingest [--source 名稱] [--full]            同步一次
  search "問題"                              只跑檢索，印出排名與分數
  grep "叫法" [--limit 80]                   全部文件裡出現這個叫法的每一行，比對規則跟詞彙表相同
  ask "問題"                                 跑完整線上流程，印出會回的內容（不寫缺口）
  eval -f eval.yaml [--sweep] [--save]      跑評估集；--sweep 掃 τ_answer
  compare [-f 題目] [--models a,b] [--out]   同一份檢索結果讓幾個模型各回答一次（需要 ANTHROPIC_API_KEY）
  gaps [--open] [--json]                    列缺口
  gaps dismiss <id>                         一般技術問題
  gaps needs-human <id> --note "…"          查不到，需要人補
  gaps close <id>                           人工確認已解決（閘門 A 通過就通知提問者）
  faq lint [--check-sources] [檔案…]         檢查 FAQ 卡與詞彙表（不給檔案就檢查線上的全部內容）
  faq stale [--json]                        列出過期的 FAQ 卡
  faq list [--json]                         列出線上的卡與詞彙表
  faq cat <路徑>                             印出線上的內容（要修改時從這裡開始）
  faq propose <路徑> [--file 檔案] --note "…"  提案新增或修改，內容從檔案或標準輸入讀；核准後才上線
  faq retire <路徑> --note "…"               提案刪除
  faq changes [--status pending] [--json]   列出提案與審核結果（含退回原因）
  faq import <目錄>                          第一次把現有內容寫進資料庫，只能做一次
  glossary candidates [--limit 50] [--json] 靠翻譯補查才答得出來、詞彙表還沒收的提問
  glossary propose [--file 檔案] --note "…"  提案加一組叫法，由審核代理決定（不貼 Discord）
  glossary check <id> --ask "問句=>出處" --trap "問句"   跑程式檢查，不改狀態
  glossary accept <id> --ask … --trap … --note "…"   檢查全過才核准；只有審核代理能跑
  glossary reject <id> --note "…"           退回
  glossary revert <id> [--note "…"]         撤回審核代理核准的一筆，馬上生效；只有人能跑
  show "project_path:path[#分頁]"            印出文件原文（跟入庫看到的一樣；xlsx 附列號，Markdown 印整份）
  serve                                     機器人 + 本機測試頁 + 排程同步 + recheck

設定檔預設 ./footnote.yaml，可用 -c 或環境變數 FOOTNOTE_CONFIG 指定。
資料庫連線字串放在設定檔 database_url_env 指定的環境變數（預設 FOOTNOTE_DATABASE_URL）。
`

func main() {
	cfgPath := os.Getenv("FOOTNOTE_CONFIG")
	if cfgPath == "" {
		cfgPath = "footnote.yaml"
	}
	args := os.Args[1:]
	if len(args) >= 2 && (args[0] == "-c" || args[0] == "--config") {
		cfgPath, args = args[1], args[2:]
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" || args[0] == "--help" {
		fmt.Print(usage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfgPath, args[0], args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "錯誤：", err)
		os.Exit(1)
	}
}

type app struct {
	cfg      *config.Config
	store    *store.Store
	tei      *tei.Client
	engine   *qa.Engine
	ingester *ingest.Ingester
	gaps     *gap.Service
	kb       *kb.Service
	render   render.Options
	log      *slog.Logger
}

func newApp(ctx context.Context, cfgPath string, log *slog.Logger) (*app, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	url, err := cfg.DatabaseURL()
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	tc := tei.New(cfg.TEI.EmbedURL, cfg.TEI.RerankURL, cfg.TEI.QueryPrefix, cfg.TEI.Timeout)
	refs := map[string]string{}
	for _, s := range cfg.AllSources() {
		refs[s.ProjectPath] = s.Ref
	}
	ro := render.Options{GitLabURL: cfg.GitLabURL, Refs: refs, ExcerptChars: cfg.QA.ExcerptChars}
	engine := &qa.Engine{Store: st, Embedder: tc, Reranker: tc, Cfg: cfg.QA, Log: log, Aliases: st, Titles: st}
	if an := cfg.Anthropic; an.APIKeyEnv != "" {
		if key := os.Getenv(an.APIKeyEnv); key != "" {
			if an.AnswerModel != "" {
				engine.Gen = llm.New(key, an.AnswerModel, an.Effort, an.MaxTokens, an.Style)
			}
			if an.RewriteModel != "" {
				engine.Rewriter = llm.NewRewriter(key, an.RewriteModel)
			}
			// 整理回答與翻譯補查共用每月預算
			engine.Budget = &llm.Budget{MonthlyUSD: an.MonthlyBudgetUSD, Usage: st.GenUsageSince}
		}
	}
	ingester := &ingest.Ingester{Store: st, Embedder: tc, Cfg: cfg, Log: log}
	return &app{
		cfg: cfg, store: st, tei: tc, engine: engine, render: ro, log: log,
		ingester: ingester,
		gaps:     &gap.Service{Store: st, QA: engine, TauDup: cfg.QA.TauDup, Render: ro, Log: log},
		kb: &kb.Service{Store: st, Reviewers: cfg.Bot.Reviewers,
			Lint: func(ctx context.Context) (ingest.LintFunc, error) { return ingester.Linter(ctx, true) }},
	}, nil
}

func run(ctx context.Context, cfgPath, cmd string, args []string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if cmd == "serve" {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if cmd == "show" { // 只讀來源，不需要資料庫與 TEI
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		return show(ctx, cfg, args)
	}
	if cmd == "migrate" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		url, err := cfg.DatabaseURL()
		if err != nil {
			return err
		}
		if err := store.Migrate(ctx, url, cfg.TEI.EmbedDim); err != nil {
			return err
		}
		fmt.Println("migrate 完成")
		return nil
	}
	a, err := newApp(ctx, cfgPath, log)
	if err != nil {
		return err
	}
	defer a.store.Close()
	switch cmd {
	case "ingest":
		return a.cmdIngest(ctx, args)
	case "search":
		return a.cmdSearch(ctx, args)
	case "grep":
		return a.cmdGrep(ctx, args)
	case "ask":
		return a.cmdAsk(ctx, args)
	case "eval":
		return a.cmdEval(ctx, args)
	case "compare":
		return a.cmdCompare(ctx, args)
	case "gaps":
		return a.cmdGaps(ctx, args)
	case "faq":
		return a.cmdFAQ(ctx, args)
	case "glossary":
		return a.cmdGlossary(ctx, args)
	case "serve":
		return a.cmdServe(ctx)
	default:
		return fmt.Errorf("不認得的指令 %q\n\n%s", cmd, usage)
	}
}

func (a *app) cmdIngest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	only := fs.String("source", "", "只同步這個來源（name 或 project_path）")
	full := fs.Bool("full", false, "忽略 sha，全部重建")
	fs.Parse(args)
	start := time.Now()
	results, err := a.ingester.Run(ctx, *only, *full)
	changed := false
	for _, r := range results {
		fmt.Printf("%s：新增 %d、更新 %d、刪除 %d、略過 %d、未變 %d", r.Source, r.Added, r.Updated, r.Deleted, r.Skipped, r.Unchanged)
		if len(r.StaleFAQs) > 0 {
			fmt.Printf("、新過期 FAQ %d", len(r.StaleFAQs))
		}
		fmt.Println()
		changed = changed || r.Changed()
	}
	if err != nil {
		return err
	}
	files, chunks, skipped, err := a.store.Counts(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("索引內共 %d 個檔案（略過 %d）、%d 個片段；耗時 %s\n", files, skipped, chunks, time.Since(start).Round(time.Second))
	if sk, _ := a.store.SkippedFiles(ctx); len(sk) > 0 {
		fmt.Println("被略過的檔案：")
		for _, f := range sk {
			fmt.Printf("  %s:%s  %s\n", f.ProjectPath, f.Path, f.Reason)
		}
	}
	if changed {
		sum, err := a.gaps.Recheck(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("recheck：檢查 %d 個缺口，FAQ 解決 %d、可能已解決 %d、排入通知 %d\n", sum.Checked, sum.ClosedByFAQ, sum.MaybeResolved, sum.Notified)
	}
	return nil
}

func (a *app) cmdSearch(ctx context.Context, args []string) error {
	q := strings.Join(args, " ")
	if q == "" {
		return errors.New(`用法：footnote search "問題"`)
	}
	ranked, _, err := a.engine.Retrieve(ctx, q, nil)
	if err != nil {
		return err
	}
	if g, err := a.engine.Glossary(ctx); err == nil && g.Expand(q) != q {
		fmt.Printf("檢索字串（已補上詞彙表的別名）：%s\n", g.Expand(q))
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\t重排\tRRF\t類型\t列／行\t引用（可直接給 footnote show）")
	for i, h := range ranked {
		mark := ""
		if float64(h.Score) >= a.cfg.QA.TauAnswer && i == 0 {
			mark = "  ← 通過閘門 A"
		}
		fmt.Fprintf(tw, "%d\t%.3f\t%.4f\t%s\t%d–%d\t%s:%s#%s%s\n", i+1, h.Score, h.RRF, h.Kind, h.LineStart, h.LineEnd, h.ProjectPath, h.Path, h.Heading, mark)
	}
	tw.Flush()
	fmt.Printf("τ_answer = %.2f，τ_keep = %.2f\n", a.cfg.QA.TauAnswer, a.cfg.QA.TauKeep)
	return nil
}

// cmdGrep 印出索引裡出現這個叫法的每一行。比對規則跟詞彙表展開相同（英文整個詞、不分大小寫），
// 審核代理看到的就是這個叫法收進去之後會命中的範圍。
func (a *app) cmdGrep(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("grep", flag.ExitOnError)
	limit := fs.Int("limit", 80, "最多印幾行，總數一律會印")
	word, ok := idArg(fs, args)
	if !ok {
		return errors.New(`用法：footnote grep "叫法" [--limit 80]`)
	}
	chunks, err := a.store.ChunksContaining(ctx, word)
	if err != nil {
		return err
	}
	const width = 160
	seen, tabs := map[string]bool{}, map[string]bool{}
	printed := ""
	lines := 0
	for _, c := range chunks {
		ref := c.ProjectPath + ":" + c.Path + "#" + c.Heading
		for _, l := range strings.Split(c.Content, "\n") {
			l = strings.TrimSpace(l)
			if seen[ref+"\n"+l] || !glossary.Mentions(l, word) {
				continue // 相鄰片段有重疊，同一行只算一次
			}
			seen[ref+"\n"+l] = true
			tabs[ref] = true
			lines++
			if lines > *limit {
				continue
			}
			if ref != printed {
				fmt.Println(ref)
				printed = ref
			}
			if r := []rune(l); len(r) > width {
				l = string(r[:width]) + "…"
			}
			fmt.Printf("    %s\n", l)
		}
	}
	fmt.Printf("共 %d 行，在 %d 個分頁", lines, len(tabs))
	if lines > *limit {
		fmt.Printf("（只印前 %d 行，--limit 調大可以看全部）", *limit)
	}
	fmt.Println()
	return nil
}

func (a *app) cmdAsk(ctx context.Context, args []string) error {
	q := strings.Join(args, " ")
	if q == "" {
		return errors.New(`用法：footnote ask "問題"`)
	}
	res, err := a.engine.Run(ctx, q, nil, nil)
	if err != nil {
		return err
	}
	fmt.Println(render.Reply(res, nil, a.render))
	fmt.Printf("\n—— status=%s reason=%s top=%.3f latency=%s\n", res.Status, res.Reason, res.Top, res.Latency.Round(time.Millisecond))
	if rs := res.Rescue; rs != nil {
		switch {
		case rs.Skipped != "":
			fmt.Printf("—— 翻譯補查：沒查成（%s）\n", rs.Skipped)
		default:
			fmt.Printf("—— 翻譯補查：%q top=%.3f rescued=%v\n", rs.Query, rs.Top, rs.Rescued)
		}
	}
	return nil
}

func (a *app) cmdEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	file := fs.String("f", "eval.yaml", "評估集")
	sweep := fs.Bool("sweep", false, "掃 τ_answer")
	save := fs.Bool("save", false, "結果存進 eval/results/{date}.json")
	fs.Parse(args)
	items, err := eval.Load(*file)
	if err != nil {
		return err
	}
	rs, err := eval.Run(ctx, a.engine, items, os.Stderr)
	if err != nil {
		return err
	}
	k := a.cfg.QA.KReply
	tau, tauF := a.cfg.QA.TauAnswer, a.cfg.QA.TauFollow
	for _, r := range rs {
		pass := r.Pass(tau, tauF)
		if (r.Expect == "answer" && (r.Rank == 0 || r.Rank > k || !pass)) || (r.Expect != "answer" && pass) {
			follow := ""
			if r.Follow != nil {
				follow = fmt.Sprintf(" follow=%.3f（追問，前一題：%s）", *r.Follow, strings.Join(r.History, " / "))
			}
			fmt.Printf("✗ [%s] %s  top=%.3f rank=%d%s\n", r.Expect, r.Q, r.Top, r.Rank, follow)
			for _, h := range r.TopHits {
				fmt.Printf("    %s\n", h)
			}
			if x := r.Rescue; x != nil {
				printRescue(x)
			}
		}
	}
	if a.engine.Rewriter == nil {
		fmt.Println("\n翻譯補查：沒有啟用（沒設 anthropic.rewrite_model 或沒有 API key）")
	} else {
		var n, rescued int
		var in, out, ms int64
		var cost float64
		for _, r := range rs {
			if x := r.Rescue; x != nil {
				n++
				in, out, ms = in+x.InputTokens, out+x.OutputTokens, ms+x.LatencyMS
				cost += llm.Cost(x.Model, x.InputTokens, x.OutputTokens)
			}
			if r.Rescued(tau, tauF) {
				if rescued++; rescued == 1 {
					fmt.Println("\n翻譯補查救回的題目（第一次沒過閘門 A、補查後通過）：")
				}
				mark := "✓"
				if r.Expect != "answer" {
					mark = "✗ 誤導"
				}
				fmt.Printf("%s [%s] %s  top=%.3f ⇒ %.3f\n", mark, r.Expect, r.Q, r.Top, r.Rescue.Top)
				printRescue(r.Rescue)
			}
		}
		if n > 0 {
			fmt.Printf("\n翻譯補查：每題都試 %d 次（線上只在沒過閘門 A 時呼叫），平均輸入 %d、輸出 %d token，平均 %d ms，合計 $%.4f（每次 $%.5f）\n",
				n, in/int64(n), out/int64(n), ms/int64(n), cost, cost/float64(n))
		}
	}
	cur := eval.Summarize(rs, tau, tauF, k)
	fmt.Printf("\n%d 題（answer %d、拒答 %d）\n", len(rs), cur.AnswerN, cur.NegativeN)
	fmt.Printf("Recall@%d = %.2f，MRR = %.2f\n", k, cur.RecallAtK, cur.MRR)
	fmt.Printf("τ_answer = %.2f、τ_follow = %.2f：誤導率 %.0f%%（%d/%d）、誤拒率 %.0f%%（%d/%d）\n",
		cur.Tau, cur.TauFollow, cur.Misleading*100, cur.MisleadingN, cur.NegativeN, cur.FalseRefusal*100, cur.FalseRefusalN, cur.AnswerN)
	report := map[string]any{"date": time.Now().Format(time.RFC3339), "file": *file, "current": cur, "items": rs}
	if *sweep {
		printSweep := func(name string, rows []eval.Summary, val func(eval.Summary) float64) {
			fmt.Printf("\n| %s | 誤導率 | 誤拒率 |\n|---|---|---|\n", name)
			for _, r := range rows {
				fmt.Printf("| %.2f | %.0f%%（%d/%d） | %.0f%%（%d/%d） |\n", val(r), r.Misleading*100, r.MisleadingN, r.NegativeN, r.FalseRefusal*100, r.FalseRefusalN, r.AnswerN)
			}
			if best := eval.Pick(rows, 0.05); best != nil {
				fmt.Printf("\n建議 %s = %.2f（誤導率 ≤ 5%% 中誤拒率最低）\n", name, val(*best))
			} else {
				fmt.Printf("\n沒有任何 %s 能讓誤導率 ≤ 5%%\n", name)
			}
		}
		rows := eval.Sweep(rs, tauF, k)
		printSweep(fmt.Sprintf("τ_answer（τ_follow = %.2f）", tauF), rows, func(s eval.Summary) float64 { return s.Tau })
		report["sweep"] = rows
		if frows := eval.SweepFollow(rs, tau, k); frows != nil {
			fmt.Printf("\n追問題（%d 題）：", frows[0].AnswerN+frows[0].NegativeN)
			printSweep(fmt.Sprintf("τ_follow（τ_answer = %.2f）", tau), frows, func(s eval.Summary) float64 { return s.TauFollow })
			report["sweep_follow"] = frows
		}
	}
	if *save {
		if err := os.MkdirAll("eval/results", 0o755); err != nil {
			return err
		}
		path := filepath.Join("eval/results", time.Now().Format("2006-01-02")+".json")
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		fmt.Println("結果存到", path)
	}
	return nil
}

func (a *app) cmdGaps(ctx context.Context, args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub := args[0]
		fs := flag.NewFlagSet("gaps "+sub, flag.ExitOnError)
		note := fs.String("note", "", "needs-human：查過哪裡、缺什麼、可能要問誰")
		arg, ok := idArg(fs, args[1:])
		if !ok {
			return fmt.Errorf(`用法：footnote gaps %s <id> [--note "…"]`, sub)
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(strings.ToUpper(arg), "G"), 10, 64)
		if err != nil {
			return fmt.Errorf("缺口編號 %q 不對", arg)
		}
		switch sub {
		case "dismiss":
			err = a.gaps.Dismiss(ctx, id)
		case "needs-human":
			err = a.gaps.NeedsHuman(ctx, id, *note)
		case "close":
			var notified bool
			notified, err = a.gaps.Close(ctx, id)
			if err == nil && notified {
				fmt.Println("已排入通知，serve 會送到原討論串")
			} else if err == nil {
				fmt.Println("閘門 A 仍擋下這題，不通知提問者")
			}
		default:
			return fmt.Errorf("不認得的 gaps 子指令 %q", sub)
		}
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("找不到缺口 G%d", id)
		}
		if err == nil {
			fmt.Printf("G%d 已更新\n", id)
		}
		return err
	}
	fs := flag.NewFlagSet("gaps", flag.ExitOnError)
	open := fs.Bool("open", false, "只列待處理（open）")
	asJSON := fs.Bool("json", false, "輸出 JSON（給代理讀，含每次的問法與回覆）")
	fs.Parse(args)
	var statuses []string
	if *open {
		statuses = []string{gap.StatusOpen}
	}
	list, err := a.store.ListGaps(ctx, statuses)
	if err != nil {
		return err
	}
	if *asJSON {
		type askOut struct {
			Question  string    `json:"question"`
			Query     string    `json:"query"`                  // 檢索字串；討論串追問時含同串前一題
			Rescue    string    `json:"rescue_query,omitempty"` // 翻譯補查：改寫成文件用語後再查的字串（試過還是沒找到才會留在缺口裡）
			Status    string    `json:"status"`
			Reply     string    `json:"reply"`
			Platform  string    `json:"platform"`
			CreatedAt time.Time `json:"created_at"`
		}
		type gapOut struct {
			store.Gap
			Ref  string   `json:"ref"`
			Asks []askOut `json:"asks"`
		}
		out := []gapOut{}
		for _, g := range list {
			asks, err := a.store.AsksForGap(ctx, g.ID)
			if err != nil {
				return err
			}
			o := gapOut{Gap: g, Ref: fmt.Sprintf("G%d", g.ID), Asks: []askOut{}}
			for _, x := range asks {
				o.Asks = append(o.Asks, askOut{x.Question, x.Query, x.RescueQuery, x.Status, x.Reply, x.Platform, x.CreatedAt})
			}
			out = append(out, o)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "編號\t狀態\t種類\t次數\t問題")
	for _, g := range list {
		fmt.Fprintf(tw, "G%d\t%s\t%s\t%d\t%s\n", g.ID, g.Status, g.Kind, g.Hits, g.Question)
	}
	return tw.Flush()
}

func (a *app) cmdFAQ(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("用法：footnote faq lint|stale|list|cat|propose|retire|changes|import")
	}
	switch args[0] {
	case "lint":
		fs := flag.NewFlagSet("faq lint", flag.ExitOnError)
		check := fs.Bool("check-sources", false, "確認每個來源存在、且屬於同步範圍內的專案")
		fs.Parse(args[1:])
		lint, err := a.ingester.Linter(ctx, *check)
		if err != nil {
			return err
		}
		docs := map[string][]byte{}
		for _, p := range fs.Args() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			docs[p] = b
		}
		if fs.NArg() == 0 {
			cur, err := a.store.KBCurrent(ctx)
			if err != nil {
				return err
			}
			for p, c := range cur {
				docs[p] = []byte(c)
			}
		}
		paths := make([]string, 0, len(docs))
		for p := range docs {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		bad := 0
		for _, p := range paths {
			probs, err := lint(ctx, p, docs[p])
			if err != nil {
				return err
			}
			if len(probs) == 0 {
				fmt.Printf("✓ %s\n", p)
				continue
			}
			bad++
			fmt.Printf("✗ %s\n", p)
			for _, x := range probs {
				fmt.Printf("    %s\n", x)
			}
		}
		if bad > 0 {
			return fmt.Errorf("%d 個檔案沒通過", bad)
		}
		return nil
	case "list":
		fs := flag.NewFlagSet("faq list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "輸出 JSON")
		fs.Parse(args[1:])
		cur, err := a.store.KBCurrent(ctx)
		if err != nil {
			return err
		}
		paths := make([]string, 0, len(cur))
		for p := range cur {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(paths)
		}
		for _, p := range paths {
			fmt.Println(p)
		}
		return nil
	case "cat":
		if len(args) != 2 {
			return errors.New("用法：footnote faq cat <路徑>")
		}
		cur, err := a.store.KBCurrent(ctx)
		if err != nil {
			return err
		}
		c, ok := cur[args[1]]
		if !ok {
			return fmt.Errorf("線上沒有 %s", args[1])
		}
		fmt.Print(c)
		return nil
	case "propose":
		fs := flag.NewFlagSet("faq propose", flag.ExitOnError)
		file := fs.String("file", "", "內容所在的檔案；沒給就讀標準輸入（kubectl exec -i 用）")
		note := fs.String("note", "", "提案說明：對應的缺口、為什麼改")
		by := fs.String("by", proposer(), "提案者")
		p, ok := idArg(fs, args[1:])
		if !ok || strings.TrimSpace(*note) == "" {
			return errors.New(`用法：footnote faq propose <路徑> [--file 檔案] --note "說明"`)
		}
		var data []byte
		var err error
		if *file != "" {
			data, err = os.ReadFile(*file)
		} else {
			data, err = io.ReadAll(os.Stdin)
		}
		if err != nil {
			return err
		}
		id, err := a.kb.Propose(ctx, p, data, *note, *by)
		if err != nil {
			return err
		}
		fmt.Printf("提案 #%d 已送出，等人在 Discord 核准後上線\n", id)
		return nil
	case "retire":
		fs := flag.NewFlagSet("faq retire", flag.ExitOnError)
		note := fs.String("note", "", "為什麼要刪")
		by := fs.String("by", proposer(), "提案者")
		p, ok := idArg(fs, args[1:])
		if !ok {
			return errors.New(`用法：footnote faq retire <路徑> --note "原因"`)
		}
		id, err := a.kb.Retire(ctx, p, *note, *by)
		if err != nil {
			return err
		}
		fmt.Printf("提案 #%d 已送出，等人在 Discord 核准後刪除\n", id)
		return nil
	case "changes":
		fs := flag.NewFlagSet("faq changes", flag.ExitOnError)
		status := fs.String("status", "", "只列這個狀態：pending、approved、rejected、superseded")
		limit := fs.Int("limit", 50, "最多列幾筆")
		asJSON := fs.Bool("json", false, "輸出 JSON")
		fs.Parse(args[1:])
		list, err := a.store.KBList(ctx, *status, *limit)
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if list == nil {
				list = []store.KBChange{}
			}
			return enc.Encode(list)
		}
		for _, c := range list {
			via := ""
			if c.ReviewVia == "agent" {
				via = " [review_via=agent]"
			}
			fmt.Printf("#%d %s %s（%s）%s%s\n", c.ID, c.Status, c.Path, c.ProposedBy, c.Note, via)
			if c.ReviewNote != "" {
				label := "審核紀錄"
				if c.Status == "rejected" {
					label = "退回原因"
				}
				fmt.Printf("    %s（%s）：%s\n", label, c.Reviewer, c.ReviewNote)
			}
		}
		return nil
	case "import":
		if len(args) != 2 {
			return errors.New("用法：footnote faq import <目錄>")
		}
		n, err := a.kb.Import(ctx, args[1], proposer())
		if err != nil {
			return err
		}
		fmt.Printf("匯入 %d 份；內容跟原本一樣，下一次同步不會重建\n", n)
		return nil
	case "stale":
		fs := flag.NewFlagSet("faq stale", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "輸出 JSON")
		fs.Parse(args[1:])
		listings, _, err := a.ingester.Listings(ctx)
		if err != nil {
			return err
		}
		list, err := a.store.ListStaleFAQs(ctx, listings)
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if list == nil {
				list = []store.StaleFAQ{}
			}
			return enc.Encode(list)
		}
		if len(list) == 0 {
			fmt.Println("沒有過期的 FAQ 卡")
		}
		for _, f := range list {
			fmt.Printf("%s（%s 確認，%s 起過期）\n", f.Path, f.Verified.Format("2006-01-02"), f.StaleSince.Format("2006-01-02 15:04"))
			for _, c := range f.Changed {
				fmt.Printf("    來源有變動：%s\n", c)
			}
		}
		return nil
	default:
		return fmt.Errorf("不認得的 faq 子指令 %q", args[0])
	}
}

// multiFlag 收同一個參數的多個值（--ask、--trap）。
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, "、") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func (a *app) cmdGlossary(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("用法：footnote glossary candidates|propose|check|accept|reject|revert")
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Print(usage)
		return nil
	case "candidates":
		return a.glossaryCandidates(ctx, args[1:])
	case "propose":
		fs := flag.NewFlagSet("glossary propose", flag.ExitOnError)
		file := fs.String("file", "", "改好的整份 glossary.yaml；沒給就讀標準輸入（kubectl exec -i 用）")
		note := fs.String("note", "", "叫法與來源的編號（G12、提問 #50），不要寫判斷的理由：審核代理要自己判斷")
		by := fs.String("by", proposer(), "提案者")
		fs.Parse(args[1:])
		if strings.TrimSpace(*note) == "" {
			return errors.New(`用法：footnote glossary propose [--file 檔案] --note "叫法與來源編號"`)
		}
		var data []byte
		var err error
		if *file != "" {
			data, err = os.ReadFile(*file)
		} else {
			data, err = io.ReadAll(os.Stdin)
		}
		if err != nil {
			return err
		}
		id, err := a.kb.ProposeGlossary(ctx, data, *note, *by)
		if err != nil {
			return err
		}
		fmt.Printf("提案 #%d 已送出，等審核代理決定\n", id)
		return nil
	case "check", "accept":
		return a.glossaryVerify(ctx, args[0], args[1:])
	case "reject":
		fs := flag.NewFlagSet("glossary reject", flag.ExitOnError)
		note := fs.String("note", "", "退回的原因")
		raw, ok := idArg(fs, args[1:])
		id, err := strconv.ParseInt(raw, 10, 64)
		if !ok || err != nil {
			return errors.New(`用法：footnote glossary reject <id> --note "原因"`)
		}
		if err := a.kb.RejectGlossary(ctx, id, *note); err != nil {
			return err
		}
		fmt.Printf("提案 #%d 已退回\n", id)
		return nil
	case "revert":
		fs := flag.NewFlagSet("glossary revert", flag.ExitOnError)
		note := fs.String("note", "", "為什麼撤回")
		raw, ok := idArg(fs, args[1:])
		id, err := strconv.ParseInt(raw, 10, 64)
		if !ok || err != nil {
			return errors.New(`用法：footnote glossary revert <id> [--note "原因"]`)
		}
		newID, err := a.kb.RevertGlossary(ctx, id, proposer(), *note)
		if err != nil {
			return err
		}
		fmt.Printf("#%d 已撤回（新紀錄 #%d）\n", id, newID)
		return a.syncFAQ(ctx)
	default:
		return fmt.Errorf("不認得的 glossary 子指令 %q", args[0])
	}
}

// glossaryCandidates 列出靠翻譯補查才答得出來的提問。詞彙表已經收了的問法不再列：那筆是收之前留下的紀錄。
func (a *app) glossaryCandidates(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("glossary candidates", flag.ExitOnError)
	limit := fs.Int("limit", 50, "最多看幾筆")
	asJSON := fs.Bool("json", false, "輸出 JSON")
	fs.Parse(args)
	asks, err := a.store.RescuedAsks(ctx, *limit)
	if err != nil {
		return err
	}
	g, err := a.engine.Glossary(ctx)
	if err != nil {
		return err
	}
	type row struct {
		ID          int64     `json:"id"`
		Platform    string    `json:"platform"` // discord 是同事問的；web 是本機測試頁
		Question    string    `json:"question"`
		RescueQuery string    `json:"rescue_query"`
		TopScore    float32   `json:"top_score"`
		Feedback    *int16    `json:"feedback"`
		CreatedAt   time.Time `json:"created_at"`
	}
	rows := []row{}
	for _, x := range asks {
		if g.Expand(x.Question) == x.Question {
			rows = append(rows, row{x.ID, x.Platform, x.Question, x.RescueQuery, x.TopScore, x.Feedback, x.CreatedAt})
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	for _, r := range rows {
		fmt.Printf("#%d [%s] %s\n    補查改寫成：%s（%.3f）\n", r.ID, r.Platform, r.Question, r.RescueQuery, r.TopScore)
	}
	if len(rows) == 0 {
		fmt.Println("沒有候選")
	}
	return nil
}

// glossaryVerify 是 check 與 accept：兩者跑同一套檢查，accept 全過才核准。
func (a *app) glossaryVerify(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet("glossary "+cmd, flag.ExitOnError)
	var asks, traps multiFlag
	fs.Var(&asks, "ask", "要查得到的問題：問句=>project_path:path#分頁，出處自己讀過文件再寫；可重複")
	fs.Var(&traps, "trap", "用這個叫法問、文件沒有答案的問題；可重複")
	file := fs.String("f", "eval/eval.example.yaml", "評估集")
	note := fs.String("note", "", "判斷的依據：文件哪裡證明這個叫法就是那件事（accept 必填）")
	asJSON := fs.Bool("json", false, "輸出 JSON")
	raw, ok := idArg(fs, args)
	id, err := strconv.ParseInt(raw, 10, 64)
	if !ok || err != nil {
		return fmt.Errorf(`用法：footnote glossary %s <id> --ask "問句=>出處" --trap "問句"`, cmd)
	}
	if cmd == "accept" && strings.TrimSpace(*note) == "" {
		return errors.New("核准要寫判斷的依據（--note）")
	}
	in := eval.AliasInput{K: a.cfg.QA.KReply, Traps: traps}
	for _, v := range asks {
		i := strings.LastIndex(v, "=>")
		if i < 0 {
			return fmt.Errorf("--ask %q 的格式是 問句=>project_path:path#分頁", v)
		}
		in.Asks = append(in.Asks, eval.AliasAsk{Q: strings.TrimSpace(v[:i]), Source: strings.TrimSpace(v[i+2:])})
	}
	if in.Items, err = eval.Load(*file); err != nil {
		return fmt.Errorf("讀評估集：%w", err)
	}
	verify := func(ctx context.Context, p *kb.GlossaryProposal) (string, error) {
		in.Before, in.After, in.Aliases = p.Before, p.After, p.Diff.Added
		r, err := eval.CheckAlias(ctx, a.engine, in)
		if err != nil {
			return "", err
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(r); err != nil {
				return "", err
			}
		} else {
			printAliasReport(p, r)
		}
		if !r.OK() {
			return "", errors.New("檢查沒通過，提案的狀態沒有改")
		}
		return r.Evidence() + "。依據：" + *note, nil
	}
	if cmd == "check" {
		p, err := a.kb.GlossaryProposal(ctx, id)
		if err != nil {
			return err
		}
		_, err = verify(ctx, p)
		return err
	}
	if err := a.kb.AcceptGlossary(ctx, id, verify); err != nil {
		return err
	}
	fmt.Printf("提案 #%d 已核准\n", id)
	return a.syncFAQ(ctx)
}

func printAliasReport(p *kb.GlossaryProposal, r *eval.AliasReport) {
	kind := "在既有的一組加叫法"
	if p.Diff.New {
		kind = "新增一組"
	}
	fmt.Printf("提案 #%d：%s %v，要驗的叫法：%s\n", p.ID, kind, p.Diff.Group, strings.Join(p.Diff.Added, "、"))
	mark := func(ok bool) string {
		if ok {
			return "✓"
		}
		return "✗"
	}
	for _, x := range r.Asks {
		fmt.Printf("%s 問：%s\n    檢索字串：%s\n    線上 %.3f ⇒ 提案 %.3f，第一名：%s\n", mark(x.OK), x.Q, x.Query, x.Before, x.Top, x.First)
	}
	for _, x := range r.Traps {
		fmt.Printf("%s 陷阱：%s\n    檢索字串：%s（%.3f）\n", mark(x.OK), x.Q, x.Query, x.Top)
	}
	if len(r.Asks) > 0 {
		fmt.Printf("評估集：%d 題有 %d 題受影響、%d 題變差\n", r.Total, r.Affected, len(r.Worse))
		for _, q := range r.Items {
			fmt.Printf("    %s\n", q)
		}
	}
	if r.OK() {
		fmt.Println("程式檢查通過")
		return
	}
	fmt.Println("沒通過：")
	for _, x := range r.Problems {
		fmt.Printf("    %s\n", x)
	}
}

// syncFAQ 同步 FAQ 來源，詞彙表改了馬上生效並重跑缺口（serve 的 Kick 叫不到這個行程）。
func (a *app) syncFAQ(ctx context.Context) error {
	if a.cfg.FAQ == nil {
		return nil
	}
	return a.cmdIngest(ctx, []string{"--source", a.cfg.FAQ.Name})
}

func (a *app) cmdServe(ctx context.Context) error {
	if a.cfg.Anthropic.AnswerModel != "" && a.engine.Gen == nil {
		a.log.Warn("answer generation disabled: API key env not set; replies use excerpts", "env", a.cfg.Anthropic.APIKeyEnv)
	}
	if a.cfg.Anthropic.RewriteModel != "" && a.engine.Rewriter == nil {
		a.log.Warn("rescue rewrite disabled: API key env not set", "env", a.cfg.Anthropic.APIKeyEnv)
	}
	url, _ := a.cfg.DatabaseURL()
	if err := store.Migrate(ctx, url, a.cfg.TEI.EmbedDim); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	kick := make(chan struct{}, 1) // 核准提案後叫同步馬上跑；已經有一次在等就不用再排
	a.kb.Kick = func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	handler := bot.NewHandler(a.store, a.engine, a.gaps, a.tei, a.cfg, a.render, a.log)
	handler.KB = a.kb
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.Pool.Ping(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	(&web.Server{Handler: handler, Store: a.store}).Register(mux)

	senders := []bot.Sender{web.Sender{}}
	if tok := os.Getenv(a.cfg.Bot.TokenEnv); a.cfg.Bot.TokenEnv != "" && tok != "" {
		d, err := discord.New(tok, handler, a.log)
		if err != nil {
			return err
		}
		senders = append(senders, d)
		go func() {
			if err := d.Run(ctx); err != nil {
				a.log.Error("discord", "err", err)
			}
		}()
		if a.cfg.Bot.ReviewChannel != "" && len(a.cfg.Bot.Reviewers) > 0 {
			go bot.RunReviewPoster(ctx, a.store, a.kb, d, 5*time.Second, a.log)
		} else {
			a.log.Warn("FAQ review disabled: bot.review_channel 或 bot.reviewers 沒設，提案會一直待審")
		}
	} else {
		a.log.Info("discord disabled: token env not set", "env", a.cfg.Bot.TokenEnv)
	}
	go bot.RunDeliverer(ctx, a.store, senders, 5*time.Second, a.log)
	go a.syncLoop(ctx, kick)

	srv := &http.Server{Addr: a.cfg.Server.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	a.log.Info("serving", "addr", "http://"+a.cfg.Server.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// syncLoop：啟動時同步一次，之後每 sync.interval 一次；有變動就 recheck，另外每天全量 recheck 一次。
func (a *app) syncLoop(ctx context.Context, kick <-chan struct{}) {
	tick := time.NewTicker(a.cfg.Sync.Interval)
	defer tick.Stop()
	lastFull := time.Time{}
	for {
		results, err := a.ingester.Run(ctx, "", false)
		if errors.Is(err, ingest.ErrBusy) {
			a.log.Info("sync skipped", "reason", err)
		} else if err != nil {
			a.log.Error("sync", "err", err)
		}
		changed := false
		for _, r := range results {
			changed = changed || r.Changed()
		}
		if changed || time.Since(lastFull) > 24*time.Hour {
			if _, err := a.gaps.Recheck(ctx); err != nil {
				a.log.Error("recheck", "err", err)
			} else if !changed {
				lastFull = time.Now()
			}
			if lastFull.IsZero() {
				lastFull = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-kick:
		}
	}
}

// proposer 是提案紀錄上的名字：指令列跑的就是這台機器的使用者。
func proposer() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "agent"
}

func printRescue(x *eval.Rescue) {
	if x.Skipped != "" {
		fmt.Printf("    補查：%s\n", x.Skipped)
		return
	}
	follow := ""
	if x.Follow != nil {
		follow = fmt.Sprintf(" follow=%.3f", *x.Follow)
	}
	fmt.Printf("    補查：%q  top=%.3f rank=%d%s\n", x.Query, x.Top, x.Rank, follow)
	for _, h := range x.TopHits {
		fmt.Printf("      %s\n", h)
	}
}

// show 印出一份文件抽出來的原文，跟入庫走同一條路（ingest.Extract）。給寫 FAQ 卡的代理讀來源用：
// xlsx 不能直接讀，而且代理讀到的必須就是機器人會引用的內容（略過的分頁、含密鑰的檔案都不印）。
func show(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New(`用法：footnote show "project_path:path[#分頁]"`)
	}
	ref, err := faq.ParseRef(args[0])
	if err != nil {
		return err
	}
	var src *config.Source
	for _, s := range cfg.AllSources() {
		if s.ProjectPath == ref.Project {
			src = &s
		}
	}
	if src == nil {
		return fmt.Errorf("設定裡沒有專案 %s，不在同步範圍內的內容不能引用", ref.Project)
	}
	if cfg.IsFAQ(ref.Project) {
		return errors.New("FAQ 卡不能當來源引用；要看卡片內容用 footnote faq cat <路徑>")
	}
	l, err := source.NewLocal(ctx, *src)
	if err != nil {
		return err
	}
	if len(l.Selected(map[string]string{ref.Path: ""})) == 0 {
		return fmt.Errorf("%s 不在 %s 的索引範圍（include／exclude）", ref.Path, ref.Project)
	}
	data, err := l.Read(ctx, ref.Path)
	if err != nil {
		return err
	}
	secs, reason := ingest.Extract(*src, ref.Path, data)
	if reason != "" {
		return fmt.Errorf("這份文件不會被索引，也不能引用：%s", reason)
	}
	if newer := src.SupersededBy(ref.Path); newer != "" {
		fmt.Printf("※ 舊版，新版是 %s:%s；寫卡以新版為準\n", ref.Project, newer)
	} else if l.IsDraft(ref.Path) {
		fmt.Println("※ 未確認設計")
	}
	var names []string
	found := false
	for _, sec := range secs {
		names = append(names, sec.Heading)
		if ref.Anchor != "" && sec.Heading != "" && sec.Heading != ref.Anchor {
			continue
		}
		found = true
		if sec.Heading != "" {
			fmt.Printf("## %s\n", sec.Heading)
		}
		for _, ln := range sec.Lines {
			fmt.Printf("%d\t%s\n", ln.N, ln.Text)
		}
	}
	if !found {
		return fmt.Errorf("找不到分頁 %q；這份文件可引用的分頁：%s", ref.Anchor, strings.Join(names, "、"))
	}
	return nil
}

// idArg 取出子指令唯一的位置參數（缺口編號），旗標寫在它前面或後面都可以。
// Go 的 flag 遇到第一個非旗標參數就停止解析，但文件寫的是 `needs-human <id> --note "…"`（編號在前）。
func idArg(fs *flag.FlagSet, args []string) (string, bool) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return "", false
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != 1 {
		return "", false
	}
	return pos[0], true
}
