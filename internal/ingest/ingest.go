// Package ingest 同步知識來源：比對 blob sha、抽文字、密鑰掃描、切塊、嵌入、寫入。
package ingest

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/miskawu/footnote/internal/chunk"
	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/faq"
	"github.com/miskawu/footnote/internal/glossary"
	"github.com/miskawu/footnote/internal/secret"
	"github.com/miskawu/footnote/internal/source"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
	"github.com/miskawu/footnote/internal/textseg"
	"github.com/miskawu/footnote/internal/xlsx"
)

// ErrBusy 表示另一個同步正在進行（可能是 serve 的排程，也可能是手動的 footnote ingest）。
var ErrBusy = errors.New("另一個同步正在進行，這次略過")

const lockKey = 0x666f6f746e6f7465 // "footnote"

type Embedder interface {
	Embed(ctx context.Context, texts []string, kind tei.EmbedKind) ([][]float32, error)
}

type Ingester struct {
	Store    *store.Store
	Embedder Embedder
	Cfg      *config.Config
	Log      *slog.Logger
}

type Result struct {
	Source                                      string
	Added, Updated, Deleted, Skipped, Unchanged int
	StaleFAQs                                   []string
}

func (r Result) Changed() bool { return r.Added+r.Updated+r.Deleted > 0 || len(r.StaleFAQs) > 0 }

// Run 同步所有來源（文件在前、FAQ 在後）。only 非空時只同步那個來源（比對 name 或 project_path），
// 但仍會列出所有來源，因為 FAQ 卡要用其他來源的現況檢查引用與過期。only 對不上任何來源就報錯，
// 不拿鎖、不 fetch。
func (in *Ingester) Run(ctx context.Context, only string, full bool) ([]Result, error) {
	targets, err := selectSources(in.Cfg.AllSources(), only)
	if err != nil {
		return nil, err
	}
	release, ok, err := in.Store.TryLock(ctx, lockKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrBusy
	}
	defer release()
	in.fetchSources(ctx)
	listings, locals, err := in.Listings(ctx)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, src := range targets {
		res, err := in.syncSource(ctx, locals[src.ProjectPath], listings, full)
		if err != nil {
			return results, fmt.Errorf("source %s: %w", src.Name, err)
		}
		results = append(results, res)
	}
	return results, nil
}

// selectSources 挑出要同步的來源：only 為空時全部，否則挑 name 或 project_path 等於 only 的。
// 一個都對不上就回錯誤並列出可用的名稱，免得打錯字時看起來像同步成功、其實什麼都沒做。
func selectSources(all []config.Source, only string) ([]config.Source, error) {
	if only == "" {
		return all, nil
	}
	var out []config.Source
	for _, src := range all {
		if only == src.Name || only == src.ProjectPath {
			out = append(out, src)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	names := make([]string, 0, len(all))
	for _, src := range all {
		if src.Name == src.ProjectPath {
			names = append(names, src.Name)
		} else {
			names = append(names, fmt.Sprintf("%s（%s）", src.Name, src.ProjectPath))
		}
	}
	return nil, fmt.Errorf("找不到來源 %q；可用的來源（name 或括號裡的 project_path 都可以）：%s", only, strings.Join(names, "、"))
}

// fetchSources 對設了 fetch 的來源先抓遠端最新的 commit；失敗只記 warn，沿用上次抓到的內容。
// 只在同步時抓，faq stale 等只讀指令不打網路。
func (in *Ingester) fetchSources(ctx context.Context) {
	for _, src := range in.Cfg.AllSources() {
		if !src.Fetch {
			continue
		}
		l, err := source.NewLocal(ctx, src) // 設了 fetch 的一定是 git 來源
		if err == nil {
			err = l.Fetch(ctx)
		}
		if err != nil {
			in.Log.Warn("fetch failed; using last fetched content", "source", src.Name, "err", err)
		}
	}
}

// Listings 列出每個來源目前所有檔案的 path → sha。
func (in *Ingester) Listings(ctx context.Context) (map[string]map[string]string, map[string]source.Reader, error) {
	listings := map[string]map[string]string{}
	locals := map[string]source.Reader{}
	for _, src := range in.Cfg.AllSources() {
		l, err := in.open(ctx, src)
		if err != nil {
			return nil, nil, err
		}
		all, err := l.List(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("source %s: %w", src.Name, err)
		}
		listings[src.ProjectPath] = all
		locals[src.ProjectPath] = l
	}
	return listings, locals, nil
}

func (in *Ingester) open(ctx context.Context, src config.Source) (source.Reader, error) {
	if src.Type == config.TypeDB {
		return source.NewDB(src, in.Store), nil
	}
	return source.NewLocal(ctx, src)
}

// Resolver 用目前的列表回答 FAQ 卡的來源存不存在。FAQ 來源本身不算可引用的專案。
func (in *Ingester) Resolver(listings map[string]map[string]string) faq.Resolver {
	return func(project, p string) (string, bool, bool) {
		files, ok := listings[project]
		if !ok || in.Cfg.IsFAQ(project) {
			return "", false, false
		}
		sha, exists := files[p]
		return sha, exists, true
	}
}

func (in *Ingester) syncSource(ctx context.Context, l source.Reader, listings map[string]map[string]string, full bool) (Result, error) {
	src := l.Config()
	res := Result{Source: src.Name}
	selected := l.Selected(listings[src.ProjectPath])
	for _, g := range src.Versions {
		for _, v := range g {
			if _, ok := selected[v]; !ok { // 打錯字的話整組版本設定會默默失效，所以要叫出來
				in.Log.Warn("versions 列的檔案不在索引範圍內", "source", src.Name, "path", v)
			}
		}
	}
	pipeline := Fingerprint(src)
	existing, err := in.Store.ListFiles(ctx, src.ProjectPath)
	if err != nil {
		return res, err
	}
	if src.Type == config.TypeDB && len(selected) == 0 && len(existing) > 0 {
		// 程式換版了但還沒匯入：這時照常同步會把線上的卡片全部刪掉。正常刪光的話 kb_changes 會有紀錄。
		empty, err := in.Store.KBEmpty(ctx)
		if err != nil {
			return res, err
		}
		if empty {
			in.Log.Error("資料庫裡還沒有 FAQ 卡的紀錄，這次不動線上的卡片；請先跑 footnote faq import <目錄>", "cards", len(existing))
			return res, nil
		}
	}
	paths := make([]string, 0, len(selected))
	for p := range selected {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		sha := selected[p]
		old, had := existing[p]
		sameBlob := had && old.BlobSHA == sha
		if sameBlob && old.Pipeline == pipeline && !full {
			res.Unchanged++
			continue
		}
		start := time.Now()
		skipped, err := in.syncFile(ctx, l, p, sha, pipeline, listings, sameBlob)
		if err != nil {
			return res, fmt.Errorf("%s: %w", p, err)
		}
		in.Log.Info("file", "source", src.Name, "path", p, "ms", time.Since(start).Milliseconds())
		switch {
		case skipped != "":
			res.Skipped++
			in.Log.Warn("skipped", "source", src.Name, "path", p, "reason", skipped)
		case had:
			res.Updated++
		default:
			res.Added++
		}
	}
	for p := range existing {
		if _, ok := selected[p]; !ok {
			if err := in.Store.DeleteFile(ctx, src.ProjectPath, p); err != nil {
				return res, err
			}
			res.Deleted++
		}
	}
	if !in.Cfg.IsFAQ(src.ProjectPath) {
		res.StaleFAQs, err = in.Store.MarkStaleFAQs(ctx, src.ProjectPath, listings[src.ProjectPath])
		if err != nil {
			return res, err
		}
		for _, f := range res.StaleFAQs {
			in.Log.Warn("faq stale", "faq", f, "source", src.Name)
		}
	}
	in.Log.Info("synced", "source", src.Name, "added", res.Added, "updated", res.Updated, "deleted", res.Deleted, "skipped", res.Skipped, "unchanged", res.Unchanged)
	return res, nil
}

// syncFile 處理單一檔案，回傳略過原因（空字串表示有收）。sameBlob 表示內容沒變、只是重新嵌入。
func (in *Ingester) syncFile(ctx context.Context, l source.Reader, p, sha, pipeline string, listings map[string]map[string]string, sameBlob bool) (string, error) {
	src := l.Config()
	meta := store.FileMeta{ProjectPath: src.ProjectPath, Path: p, Ref: src.Ref, BlobSHA: sha, Kind: "doc", SupersededBy: src.SupersededBy(p), Pipeline: pipeline}
	meta.Draft = l.IsDraft(p) && meta.SupersededBy == "" // 舊版只標「舊版」，標籤在這裡定一次
	skip := func(reason string) (string, error) {
		meta.Skipped = reason
		return reason, in.Store.ReplaceFile(ctx, meta, nil, nil)
	}
	data, err := l.Read(ctx, p)
	if err != nil {
		return "", err
	}

	var pieces []chunk.Chunk
	var embedTexts []string
	var faqRow *store.FAQRow
	ext := strings.ToLower(path.Ext(p))
	switch {
	case in.Cfg.IsFAQ(src.ProjectPath) && p == glossary.Path:
		meta.Kind = "glossary"
		groups, err := glossary.Parse(data)
		if err != nil {
			return skip("bad_glossary: " + err.Error())
		}
		if probs := glossary.Lint(groups); len(probs) > 0 {
			return skip("bad_glossary: " + strings.Join(probs, "; "))
		}
		return "", in.Store.ReplaceGlossary(ctx, meta, groups)
	case in.Cfg.IsFAQ(src.ProjectPath):
		meta.Kind = "faq"
		card, err := faq.Parse(data)
		if err != nil {
			return skip("bad_faq: " + err.Error())
		}
		if probs := faq.Lint(card, in.Resolver(listings)); len(probs) > 0 {
			return skip("bad_faq: " + strings.Join(probs, "; "))
		}
		lines := strings.Count(card.Body, "\n") + 1
		pieces = []chunk.Chunk{{Heading: card.Questions[0], LineStart: card.BodyLine, LineEnd: card.BodyLine + lines - 1, Content: card.Body}}
		embedTexts = []string{card.EmbedText(src.ProjectPath, p)}
		faqRow = &store.FAQRow{GapID: card.Gap, Verified: card.Verified}
		if card.Gap != nil { // 編號寫錯或資料庫重建過：保留卡片、拿掉連結，不要讓外鍵擋掉整張卡
			if ok, err := in.Store.GapExists(ctx, *card.Gap); err != nil {
				return "", err
			} else if !ok {
				in.Log.Warn("faq refers to unknown gap; link dropped", "path", p, "gap", *card.Gap)
				faqRow.GapID = nil
			}
		}
		for i, r := range card.Sources {
			faqRow.Sources = append(faqRow.Sources, store.FAQSourceRow{
				Ord: i + 1, Ref: r.Raw, SrcProject: r.Project, SrcPath: r.Path, SrcBlobSHA: listings[r.Project][r.Path],
			})
		}
		if sameBlob { // 內容沒變只是重新嵌入：沿用原本的過期基準與狀態
			if old, err := in.Store.GetFAQ(ctx, src.ProjectPath, p); err == nil {
				faqRow.Sources, faqRow.StaleSince = old.Sources, old.StaleSince
			}
		}
	default:
		secs, reason := Extract(src, p, data)
		if reason != "" {
			return skip(reason)
		}
		if ext == ".xlsx" {
			for _, sec := range secs {
				pieces = append(pieces, chunk.Lines(sec.Heading, sec.Lines)...)
			}
		} else {
			pieces = chunk.Markdown(string(data)) // Markdown 依標題切，原文交給 chunk
		}
	}

	if len(pieces) == 0 {
		return skip("empty")
	}
	if embedTexts == nil {
		for _, c := range pieces {
			embedTexts = append(embedTexts, EmbedText(src.ProjectPath, p, c.Heading, c.Content))
		}
	}
	vecs, err := in.Embedder.Embed(ctx, embedTexts, tei.Document)
	if err != nil {
		return "", err
	}
	rows := make([]store.ChunkRow, len(pieces))
	for i, c := range pieces {
		rows[i] = store.ChunkRow{
			Heading: c.Heading, LineStart: c.LineStart, LineEnd: c.LineEnd, Content: c.Content,
			Embedding: vecs[i], TSV: textseg.Document(embedTexts[i]),
		}
	}
	return "", in.Store.ReplaceFile(ctx, meta, rows, faqRow)
}

// LintFunc 檢查一張卡或詞彙表，回傳所有問題。
type LintFunc func(ctx context.Context, p string, data []byte) ([]string, error)

// Linter 是卡片與詞彙表的檢查：footnote faq lint、提案、核准都走這一條。
// check 為 true 時另外確認來源檔案存在、屬於同步範圍，以及 gap 指到的缺口存在。
func (in *Ingester) Linter(ctx context.Context, check bool) (LintFunc, error) {
	var resolver faq.Resolver
	if check {
		listings, _, err := in.Listings(ctx)
		if err != nil {
			return nil, err
		}
		resolver = in.Resolver(listings)
	}
	return func(ctx context.Context, p string, data []byte) ([]string, error) {
		if path.Base(p) == glossary.Path {
			groups, err := glossary.Parse(data)
			if err != nil {
				return []string{err.Error()}, nil
			}
			return glossary.Lint(groups), nil
		}
		card, err := faq.Parse(data)
		if err != nil {
			return []string{err.Error()}, nil
		}
		probs := faq.Lint(card, resolver)
		if check && card.Gap != nil {
			ok, err := in.Store.GapExists(ctx, *card.Gap)
			if err != nil {
				return nil, err
			}
			if !ok {
				probs = append(probs, fmt.Sprintf("gap G%d 不存在", *card.Gap))
			}
		}
		return probs, nil
	}, nil
}

// Section 是一份文件抽出來的一段：xlsx 的一個分頁（行號是列號），或整份 Markdown（行號是行號）。
type Section struct {
	Heading string
	Lines   []chunk.Line
}

// Extract 照入庫的規則把文件抽成文字：略過隱藏分頁與 skip_sheets、做大小與密鑰檢查。
// 入庫與 footnote show 共用這一條路，代理寫卡時讀到的就是機器人會引用的內容。
// reason 非空表示這份文件不收（原因同 source_files.skipped）。
func Extract(src config.Source, p string, data []byte) (secs []Section, reason string) {
	var all strings.Builder
	switch strings.ToLower(path.Ext(p)) {
	case ".xlsx":
		wb, err := xlsx.Read(data)
		if err != nil {
			return nil, "parse_error: " + err.Error()
		}
		for _, sh := range wb.Sheets {
			if sh.Hidden || skipSheet(src.SkipSheets, sh.Name) {
				continue
			}
			sec := Section{Heading: sh.Name}
			for _, ln := range sh.Lines {
				text := ln.Text
				if ln.TextBox {
					text = "【文字方塊】" + text
				}
				sec.Lines = append(sec.Lines, chunk.Line{N: ln.Row, Text: text})
				all.WriteString(text + "\n")
			}
			secs = append(secs, sec)
		}
	case ".md", ".mdx":
		var sec Section
		for i, ln := range strings.Split(string(data), "\n") {
			sec.Lines = append(sec.Lines, chunk.Line{N: i + 1, Text: ln})
		}
		all.Write(data)
		secs = []Section{sec}
	default:
		return nil, "unsupported_type"
	}
	if r := checkText(src, all.String()); r != "" {
		return nil, r
	}
	return secs, ""
}

func checkText(src config.Source, text string) string {
	if kb := len(text) / 1024; kb > src.MaxTextKB {
		return fmt.Sprintf("too_large: 文字 %d KB 超過 %d KB", kb, src.MaxTextKB)
	}
	if f := secret.Scan(text); len(f) > 0 {
		return fmt.Sprintf("secret: 規則 %s，第 %d 行", f[0].Rule, f[0].Line)
	}
	return ""
}

// pipelineVersion 在抽文字、切塊、嵌入用文字的程式邏輯改變時手動加一，讓所有檔案重建。
const pipelineVersion = 1

// Fingerprint 是會影響「同一份內容產出哪些片段」的設定指紋。內容沒變但指紋變了，也要重建。
func Fingerprint(src config.Source) string {
	b, _ := json.Marshal(map[string]any{
		"v": pipelineVersion, "draft": src.Draft, "versions": src.Versions, "skip_sheets": src.SkipSheets, "max_text_kb": src.MaxTextKB,
		"max_chars": chunk.MaxChars, "min_chars": chunk.MinChars,
	})
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:8])
}

// EmbedText 是片段的嵌入／全文檢索用文字：「專案 › 路徑 › 標題」前綴。
func EmbedText(project, p, heading, content string) string {
	return project + " › " + p + " › " + heading + "\n" + content
}

func skipSheet(patterns []string, name string) bool {
	lower := strings.ToLower(name)
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
