// Package ingest 同步知識來源：比對 blob sha、抽文字、密鑰掃描、切塊、嵌入、寫入（plan §7.1）。
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

// Run 同步所有來源（文件在前、FAQ 在後）。only 非空時只同步那個來源，但仍會列出所有來源，
// 因為 FAQ 卡要用其他來源的現況檢查引用與過期。
func (in *Ingester) Run(ctx context.Context, only string, full bool) ([]Result, error) {
	release, ok, err := in.Store.TryLock(ctx, lockKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrBusy
	}
	defer release()
	listings, locals, err := in.Listings(ctx)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, src := range in.Cfg.AllSources() {
		if only != "" && only != src.Name && only != src.ProjectPath {
			continue
		}
		res, err := in.syncSource(ctx, locals[src.ProjectPath], listings, full)
		if err != nil {
			return results, fmt.Errorf("source %s: %w", src.Name, err)
		}
		results = append(results, res)
	}
	return results, nil
}

// Listings 列出每個來源目前所有檔案的 path → sha。
func (in *Ingester) Listings(ctx context.Context) (map[string]map[string]string, map[string]*source.Local, error) {
	listings := map[string]map[string]string{}
	locals := map[string]*source.Local{}
	for _, src := range in.Cfg.AllSources() {
		l, err := source.NewLocal(ctx, src)
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

func (in *Ingester) syncSource(ctx context.Context, l *source.Local, listings map[string]map[string]string, full bool) (Result, error) {
	src := l.Cfg
	res := Result{Source: src.Name}
	selected := l.Selected(listings[src.ProjectPath])
	pipeline := Fingerprint(src)
	existing, err := in.Store.ListFiles(ctx, src.ProjectPath)
	if err != nil {
		return res, err
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
func (in *Ingester) syncFile(ctx context.Context, l *source.Local, p, sha, pipeline string, listings map[string]map[string]string, sameBlob bool) (string, error) {
	src := l.Cfg
	meta := store.FileMeta{ProjectPath: src.ProjectPath, Path: p, Ref: src.Ref, BlobSHA: sha, Kind: "doc", Draft: l.IsDraft(p), Pipeline: pipeline}
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
	case ext == ".xlsx":
		wb, err := xlsx.Read(data)
		if err != nil {
			return skip("parse_error: " + err.Error())
		}
		var all strings.Builder
		for _, sh := range wb.Sheets {
			if sh.Hidden || skipSheet(src.SkipSheets, sh.Name) {
				continue
			}
			var lines []chunk.Line
			for _, ln := range sh.Lines {
				text := ln.Text
				if ln.TextBox {
					text = "【文字方塊】" + text
				}
				lines = append(lines, chunk.Line{N: ln.Row, Text: text})
				all.WriteString(text + "\n")
			}
			pieces = append(pieces, chunk.Lines(sh.Name, lines)...)
		}
		if r := in.checkText(src, all.String()); r != "" {
			return skip(r)
		}
	case ext == ".md" || ext == ".mdx":
		text := string(data)
		if r := in.checkText(src, text); r != "" {
			return skip(r)
		}
		pieces = chunk.Markdown(text)
	default:
		return skip("unsupported_type")
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

func (in *Ingester) checkText(src config.Source, text string) string {
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
		"v": pipelineVersion, "draft": src.Draft, "skip_sheets": src.SkipSheets, "max_text_kb": src.MaxTextKB,
		"max_chars": chunk.MaxChars, "min_chars": chunk.MinChars,
	})
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:8])
}

// EmbedText 是片段的嵌入／全文檢索用文字：「專案 › 路徑 › 標題」前綴（plan D7）。
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
