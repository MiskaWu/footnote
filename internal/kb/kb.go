// Package kb 管 FAQ 卡與詞彙表的修改：代理提案、人審核，核准後上線。
//
// 內容的正本是資料庫的 kb_changes：每次修改一筆，現行內容就是每張卡最新一筆核准的修改。
// FAQ 卡的核准只能由 Reviewers 名單裡的人在聊天平台按，指令列沒有這條路：代理可以提案，不能審自己的卡。
// 唯一的例外是詞彙表一次加一組的提案，由審核代理原樣核准或退回（glossary.go）。
package kb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/miskawu/footnote/internal/faq"
	"github.com/miskawu/footnote/internal/glossary"
	"github.com/miskawu/footnote/internal/ingest"
	"github.com/miskawu/footnote/internal/store"
)

// 寫入資料庫的路徑：主題目錄/卡片檔名.md，或詞彙表。
var pathRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9-]*\.md$`)

var (
	ErrNotReviewer = errors.New("你不在審核名單裡")
	ErrDecided     = errors.New("這個提案已經審過，或被新的提案取代了")
)

// LintError 是內容沒通過檢查：提案時回給代理，核准時回給審核的人。
type LintError struct{ Problems []string }

func (e *LintError) Error() string { return "沒通過檢查：" + strings.Join(e.Problems, "；") }

type Service struct {
	Store     *store.Store
	Lint      func(ctx context.Context) (ingest.LintFunc, error) // 檢查要用來源的現況，每次重新取
	Reviewers []string
	Kick      func() // 核准後叫同步馬上跑；沒設就等下一輪排程
}

func ValidPath(p string) error {
	if p == glossary.Path || pathRe.MatchString(p) {
		return nil
	}
	return fmt.Errorf("路徑 %q 不對：要是 主題/卡片檔名.md（英文小寫、用 - 連接），或 %s", p, glossary.Path)
}

func (s *Service) lint(ctx context.Context, p string, data []byte) error {
	lint, err := s.Lint(ctx)
	if err != nil {
		return err
	}
	probs, err := lint(ctx, p, data)
	if err != nil {
		return err
	}
	if len(probs) > 0 {
		return &LintError{Problems: probs}
	}
	return nil
}

// Propose 提案新增或修改一張卡。內容沒通過檢查就不收，審核的人只會看到格式正確、來源存在的提案。
func (s *Service) Propose(ctx context.Context, p string, content []byte, note, by string) (int64, error) {
	if err := ValidPath(p); err != nil {
		return 0, err
	}
	if err := s.lint(ctx, p, content); err != nil {
		return 0, err
	}
	cur, err := s.Store.KBCurrent(ctx)
	if err != nil {
		return 0, err
	}
	text := string(content)
	if old, ok := cur[p]; ok && old == text {
		return 0, fmt.Errorf("%s 的內容跟線上的一樣，不用提案", p)
	}
	return s.Store.KBPropose(ctx, store.KBChange{Path: p, Content: &text, Note: note, ProposedBy: by})
}

// Retire 提案刪除一張卡。
func (s *Service) Retire(ctx context.Context, p, note, by string) (int64, error) {
	cur, err := s.Store.KBCurrent(ctx)
	if err != nil {
		return 0, err
	}
	if _, ok := cur[p]; !ok {
		return 0, fmt.Errorf("線上沒有 %s", p)
	}
	if strings.TrimSpace(note) == "" {
		return 0, errors.New("刪除要寫原因（--note）")
	}
	return s.Store.KBPropose(ctx, store.KBChange{Path: p, Note: note, ProposedBy: by})
}

func (s *Service) CanReview(user string) bool {
	return user != "" && slices.Contains(s.Reviewers, user)
}

// Approve 核准提案並觸發同步。來源可能在提案之後變了，所以再檢查一次，
// 讓審核的人當場知道，而不是核准後同步時才被略過。
func (s *Service) Approve(ctx context.Context, id int64, user string) error {
	if !s.CanReview(user) {
		return ErrNotReviewer
	}
	c, err := s.Store.KBGet(ctx, id)
	if err != nil {
		return err
	}
	if c.Status != "pending" {
		return ErrDecided
	}
	if c.Content != nil {
		if err := s.lint(ctx, c.Path, []byte(*c.Content)); err != nil {
			return err
		}
	}
	ok, err := s.Store.KBDecide(ctx, id, "approved", user, "")
	if err != nil {
		return err
	}
	if !ok {
		return ErrDecided
	}
	if s.Kick != nil {
		s.Kick()
	}
	return nil
}

// Reject 退回提案。原因留在紀錄裡，代理用 footnote faq changes 看得到。
func (s *Service) Reject(ctx context.Context, id int64, user, note string) error {
	if !s.CanReview(user) {
		return ErrNotReviewer
	}
	ok, err := s.Store.KBDecide(ctx, id, "rejected", user, note)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDecided
	}
	return nil
}

// Import 把目錄裡現有的卡片與詞彙表寫進資料庫，只能做一次。
func (s *Service) Import(ctx context.Context, dir, by string) (int, error) {
	docs := map[string]string{}
	cards, err := filepath.Glob(filepath.Join(dir, "*", "*.md"))
	if err != nil {
		return 0, err
	}
	for _, f := range append(cards, filepath.Join(dir, glossary.Path)) {
		rel, err := filepath.Rel(dir, f)
		if err != nil {
			return 0, err
		}
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) && rel == glossary.Path {
			continue // 沒有詞彙表也可以
		}
		if err != nil {
			return 0, err
		}
		if err := ValidPath(rel); err != nil {
			return 0, err
		}
		docs[rel] = string(b)
	}
	if len(docs) == 0 {
		return 0, fmt.Errorf("%s 裡沒有卡片", dir)
	}
	return len(docs), s.Store.KBImport(ctx, docs, by)
}

// Preview 是審核時看到的內容，跟平台無關。
type Preview struct {
	Title string   // 新增／修改／刪除 路徑
	Note  string   // 提案說明
	Gap   string   // 回答哪個缺口：G7「金絲雀什麼時候可以全量？」
	Body  string   // 新卡的本文，或刪除前的本文
	Lines []string // 新卡的問法與來源
	Diff  string   // 修改時的差異（- 舊、+ 新）
}

// BuildPreview 組審核要看的內容：修改看差異，新增與刪除看卡片本身。
func (s *Service) BuildPreview(ctx context.Context, c store.KBChange) (Preview, error) {
	cur, err := s.Store.KBCurrent(ctx)
	if err != nil {
		return Preview{}, err
	}
	old, had := cur[c.Path]
	p := Preview{Note: c.Note}
	shown := old
	switch {
	case c.Content == nil:
		p.Title = "刪除 " + c.Path
	case had:
		p.Title = "修改 " + c.Path
		p.Diff = Diff(old, *c.Content)
		shown = *c.Content
	default:
		p.Title = "新增 " + c.Path
		shown = *c.Content
	}
	if c.Path == glossary.Path {
		if p.Diff == "" {
			p.Body = shown
		}
		return p, nil
	}
	card, err := faq.Parse([]byte(shown))
	if err != nil {
		return p, nil // 提案時檢查過，這裡只是預覽，解析不了就只給標題
	}
	if card.Gap != nil {
		p.Gap = fmt.Sprintf("G%d", *card.Gap)
		if q, err := s.Store.GapQuestion(ctx, *card.Gap); err == nil {
			p.Gap += "「" + q + "」"
		}
	}
	if p.Diff != "" {
		return p, nil
	}
	p.Body = card.Body
	p.Lines = append(p.Lines, "問法："+strings.Join(card.Questions, "／"))
	for i, r := range card.Sources {
		p.Lines = append(p.Lines, fmt.Sprintf("[%d] %s", i+1, r.Raw))
	}
	return p, nil
}

// Diff 列出兩版之間改動的行：- 是舊的、+ 是新的，沒變的行不列。卡片很短，用最簡單的 LCS 就夠。
func Diff(before, after string) string {
	a, b := strings.Split(strings.TrimRight(before, "\n"), "\n"), strings.Split(strings.TrimRight(after, "\n"), "\n")
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			i, j = i+1, j+1
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]): // 同一處的改動，舊行排在新行前面
			out = append(out, "- "+a[i])
			i++
		default:
			out = append(out, "+ "+b[j])
			j++
		}
	}
	return strings.Join(out, "\n")
}
