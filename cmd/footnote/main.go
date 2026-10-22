// footnote：出處式知識問答（線上不經 LLM 生成）。子命令見 docs/plan.md §3。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	"github.com/miskawu/footnote/internal/ingest"
	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/render"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
)

const usage = `用法：footnote [-c footnote.yaml] <指令> [參數]

  migrate                                   建表
  ingest [--source 名稱] [--full]            同步一次
  search "問題"                              只跑檢索，印出排名與分數
  ask "問題"                                 跑完整線上流程，印出會回的內容（不寫缺口）
  eval -f eval.yaml [--sweep] [--save]      跑評估集；--sweep 掃 τ_answer
  gaps [--open] [--json]                    列缺口
  gaps dismiss <id>                         一般技術問題
  gaps needs-human <id> --note "…"          查不到，需要人補
  gaps close <id>                           人工確認已解決（閘門 A 通過就通知提問者）
  faq lint [--check-sources] [檔案…]         檢查 FAQ 卡（不給檔案就檢查整個 FAQ 來源）
  faq stale [--json]                        列出過期的 FAQ 卡
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
	engine := &qa.Engine{Store: st, Embedder: tc, Reranker: tc, Cfg: cfg.QA, Log: log}
	return &app{
		cfg: cfg, store: st, tei: tc, engine: engine, render: ro, log: log,
		ingester: &ingest.Ingester{Store: st, Embedder: tc, Cfg: cfg, Log: log},
		gaps:     &gap.Service{Store: st, QA: engine, TauDup: cfg.QA.TauDup, Render: ro, Log: log},
	}, nil
}

func run(ctx context.Context, cfgPath, cmd string, args []string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if cmd == "serve" {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
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
	case "ask":
		return a.cmdAsk(ctx, args)
	case "eval":
		return a.cmdEval(ctx, args)
	case "gaps":
		return a.cmdGaps(ctx, args)
	case "faq":
		return a.cmdFAQ(ctx, args)
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
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\t重排\tRRF\t類型\t來源")
	for i, h := range ranked {
		mark := ""
		if float64(h.Score) >= a.cfg.QA.TauAnswer && i == 0 {
			mark = "  ← 通過閘門 A"
		}
		fmt.Fprintf(tw, "%d\t%.3f\t%.4f\t%s\t%s%s\n", i+1, h.Score, h.RRF, h.Kind, render.Label(h.Candidate), mark)
	}
	tw.Flush()
	fmt.Printf("τ_answer = %.2f，τ_keep = %.2f\n", a.cfg.QA.TauAnswer, a.cfg.QA.TauKeep)
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
	fmt.Printf("\n—— status=%s top=%.3f latency=%s\n", res.Status, res.Top, res.Latency.Round(time.Millisecond))
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
	for _, r := range rs {
		if (r.Expect == "answer" && (r.Rank == 0 || r.Rank > k)) || (r.Expect != "answer" && float64(r.Top) >= a.cfg.QA.TauAnswer) {
			fmt.Printf("✗ [%s] %s  top=%.3f rank=%d\n", r.Expect, r.Q, r.Top, r.Rank)
			for _, h := range r.TopHits {
				fmt.Printf("    %s\n", h)
			}
		}
	}
	cur := eval.Summarize(rs, a.cfg.QA.TauAnswer, k)
	fmt.Printf("\n%d 題（answer %d、拒答 %d）\n", len(rs), cur.AnswerN, cur.NegativeN)
	fmt.Printf("Recall@%d = %.2f，MRR = %.2f\n", k, cur.RecallAtK, cur.MRR)
	fmt.Printf("τ_answer = %.2f：誤導率 %.0f%%（%d/%d）、誤拒率 %.0f%%（%d/%d）\n",
		cur.Tau, cur.Misleading*100, cur.MisleadingN, cur.NegativeN, cur.FalseRefusal*100, cur.FalseRefusalN, cur.AnswerN)
	report := map[string]any{"date": time.Now().Format(time.RFC3339), "file": *file, "current": cur, "items": rs}
	if *sweep {
		rows := eval.Sweep(rs, k)
		fmt.Println("\n| τ_answer | 誤導率 | 誤拒率 |\n|---|---|---|")
		for _, r := range rows {
			fmt.Printf("| %.2f | %.0f%%（%d/%d） | %.0f%%（%d/%d） |\n", r.Tau, r.Misleading*100, r.MisleadingN, r.NegativeN, r.FalseRefusal*100, r.FalseRefusalN, r.AnswerN)
		}
		if best := eval.Pick(rows, 0.05); best != nil {
			fmt.Printf("\n建議 τ_answer = %.2f（誤導率 ≤ 5%% 中誤拒率最低）\n", best.Tau)
		} else {
			fmt.Println("\n沒有任何 τ 能讓誤導率 ≤ 5%")
		}
		report["sweep"] = rows
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
		fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return fmt.Errorf("用法：footnote gaps %s <id>", sub)
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(strings.ToUpper(fs.Arg(0)), "G"), 10, 64)
		if err != nil {
			return fmt.Errorf("缺口編號 %q 不對", fs.Arg(0))
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
				o.Asks = append(o.Asks, askOut{x.Question, x.Status, x.Reply, x.Platform, x.CreatedAt})
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
		return errors.New("用法：footnote faq lint|stale")
	}
	switch args[0] {
	case "lint":
		fs := flag.NewFlagSet("faq lint", flag.ExitOnError)
		check := fs.Bool("check-sources", false, "確認每個來源存在、且屬於同步範圍內的專案")
		fs.Parse(args[1:])
		listings, _, err := a.ingester.Listings(ctx)
		if err != nil {
			return err
		}
		var resolver faq.Resolver
		if *check {
			resolver = a.ingester.Resolver(listings)
		}
		paths := fs.Args()
		if len(paths) == 0 {
			if a.cfg.FAQ == nil {
				return errors.New("設定檔沒有 faq 來源；請指定要檢查的檔案")
			}
			for p := range listings[a.cfg.FAQ.ProjectPath] {
				if strings.HasSuffix(p, ".md") && !strings.EqualFold(filepath.Base(p), "README.md") && !strings.EqualFold(filepath.Base(p), "CLAUDE.md") {
					paths = append(paths, filepath.Join(a.cfg.FAQ.Dir, p))
				}
			}
			sort.Strings(paths)
		}
		bad := 0
		for _, p := range paths {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			card, err := faq.Parse(b)
			var probs []string
			if err != nil {
				probs = []string{err.Error()}
			} else {
				probs = faq.Lint(card, resolver)
				if *check && card.Gap != nil {
					if ok, err := a.store.GapExists(ctx, *card.Gap); err != nil {
						return err
					} else if !ok {
						probs = append(probs, fmt.Sprintf("gap G%d 不存在", *card.Gap))
					}
				}
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
			return fmt.Errorf("%d 張卡沒通過", bad)
		}
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

func (a *app) cmdServe(ctx context.Context) error {
	url, _ := a.cfg.DatabaseURL()
	if err := store.Migrate(ctx, url, a.cfg.TEI.EmbedDim); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	handler := bot.NewHandler(a.store, a.engine, a.gaps, a.tei, a.cfg, a.render, a.log)
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
	} else {
		a.log.Info("discord disabled: token env not set", "env", a.cfg.Bot.TokenEnv)
	}
	go bot.RunDeliverer(ctx, a.store, senders, 5*time.Second, a.log)
	go a.syncLoop(ctx)

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
func (a *app) syncLoop(ctx context.Context) {
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
		}
	}
}
