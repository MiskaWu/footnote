package kb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/miskawu/footnote/internal/glossary"
	"github.com/miskawu/footnote/internal/store"
)

// AgentReviewer 是審核代理核准或退回時記在 kb_changes.reviewer 的名字。
const AgentReviewer = "agent:glossary-reviewer"

const viaAgent = "agent"

// ErrNotAgentReview：這筆提案不是給審核代理決定的。FAQ 卡只能由人在 Discord 核准，這條路只有詞彙表。
var ErrNotAgentReview = errors.New("這筆不是待審核代理決定的詞彙表提案")

// GlossaryProposal 是一筆待審核代理決定的詞彙表提案。
type GlossaryProposal struct {
	store.KBChange
	Before, After [][]string
	Diff          *glossary.Change
}

// currentGlossary 讀線上的詞彙表；線上沒有時是空的。
func (s *Service) currentGlossary(ctx context.Context) (string, [][]string, error) {
	cur, err := s.Store.KBCurrent(ctx)
	if err != nil {
		return "", nil, err
	}
	text := cur[glossary.Path]
	groups, err := glossary.Parse([]byte(text))
	return text, groups, err
}

// ProposeGlossary 提案加一組叫法，由審核代理決定：一次只能改一組，每組才能各自審、各自撤回。
// 詞彙表只有一份，同時只能有一個待審提案，所以人還沒審完的提案不能被代理的新提案取代。
func (s *Service) ProposeGlossary(ctx context.Context, content []byte, note, by string) (int64, error) {
	if err := s.lint(ctx, glossary.Path, content); err != nil {
		return 0, err
	}
	_, before, err := s.currentGlossary(ctx)
	if err != nil {
		return 0, err
	}
	after, err := glossary.Parse(content)
	if err != nil {
		return 0, err
	}
	if _, err := glossary.Compare(before, after); err != nil {
		return 0, err
	}
	switch p, err := s.Store.KBPending(ctx, glossary.Path); {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return 0, err
	case p.ReviewVia != viaAgent:
		return 0, fmt.Errorf("提案 #%d 還在等人審，審完再提", p.ID)
	}
	text := string(content)
	return s.Store.KBPropose(ctx, store.KBChange{Path: glossary.Path, Content: &text, Note: note, ProposedBy: by, ReviewVia: viaAgent})
}

// GlossaryProposal 讀出一筆待審核代理決定的提案，並跟線上現況比對。內容在 kb_changes 裡不能改寫，
// 所以審核代理只能原樣核准或退回；要改就退回，由提案者重新提，再審一次（改了卻沒人審，正是這條規則要擋的）。
func (s *Service) GlossaryProposal(ctx context.Context, id int64) (*GlossaryProposal, error) {
	c, err := s.Store.KBGet(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.Path != glossary.Path || c.ReviewVia != viaAgent {
		return nil, ErrNotAgentReview
	}
	if c.Status != "pending" {
		return nil, ErrDecided
	}
	if c.Content == nil {
		return nil, ErrNotAgentReview
	}
	_, before, err := s.currentGlossary(ctx)
	if err != nil {
		return nil, err
	}
	after, err := glossary.Parse([]byte(*c.Content))
	if err != nil {
		return nil, err
	}
	diff, err := glossary.Compare(before, after)
	if err != nil {
		return nil, err
	}
	return &GlossaryProposal{KBChange: *c, Before: before, After: after, Diff: diff}, nil
}

// AcceptGlossary 是審核代理唯一的核准路徑。verify 跑程式檢查，通過才回傳要留下的紀錄；
// 檢查跟核准綁在同一個呼叫裡，沒有「先核准、檢查另外跑」的路可走。
func (s *Service) AcceptGlossary(ctx context.Context, id int64, verify func(context.Context, *GlossaryProposal) (string, error)) error {
	p, err := s.GlossaryProposal(ctx, id)
	if err != nil {
		return err
	}
	if err := s.lint(ctx, glossary.Path, []byte(*p.Content)); err != nil {
		return err
	}
	evidence, err := verify(ctx, p)
	if err != nil {
		return err
	}
	ok, err := s.Store.KBDecide(ctx, id, "approved", AgentReviewer, evidence)
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

// RejectGlossary 退回一筆待審核代理決定的提案；原因留在紀錄裡，下一輪用 footnote faq changes 看得到，不會重提。
func (s *Service) RejectGlossary(ctx context.Context, id int64, note string) error {
	if strings.TrimSpace(note) == "" {
		return errors.New("退回要寫原因（--note）")
	}
	c, err := s.Store.KBGet(ctx, id)
	if err != nil {
		return err
	}
	if c.Path != glossary.Path || c.ReviewVia != viaAgent {
		return ErrNotAgentReview
	}
	ok, err := s.Store.KBDecide(ctx, id, "rejected", AgentReviewer, note)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDecided
	}
	return nil
}

// RevertGlossary 撤回一筆審核代理核准的修改，負責人一個指令就生效（hook 擋住代理，見 .claude/hooks）。
// 只拿掉那一筆加的東西：之後別的提案加的叫法留著。寫新的一筆，舊紀錄不動。
func (s *Service) RevertGlossary(ctx context.Context, id int64, by, note string) (int64, error) {
	c, err := s.Store.KBGet(ctx, id)
	if err != nil {
		return 0, err
	}
	if c.Path != glossary.Path || c.Status != "approved" || c.Reviewer != AgentReviewer || c.Content == nil {
		return 0, fmt.Errorf("只能撤回審核代理核准的詞彙表修改；#%d 是 %s %s（%s）", id, c.Status, c.Path, c.Reviewer)
	}
	before, err := s.Store.KBBefore(ctx, c.Path, id)
	if err != nil {
		return 0, err
	}
	cur, _, err := s.currentGlossary(ctx)
	if err != nil {
		return 0, err
	}
	text, err := undo(cur, before, *c.Content)
	if err != nil {
		return 0, fmt.Errorf("#%d %w", id, err)
	}
	if err := s.lint(ctx, glossary.Path, []byte(text)); err != nil {
		return 0, err
	}
	msg := fmt.Sprintf("撤回 #%d", id)
	if strings.TrimSpace(note) != "" {
		msg += "：" + note
	}
	newID, err := s.Store.KBRevert(ctx, store.KBChange{Path: c.Path, Content: &text, Note: msg, ProposedBy: by})
	if err != nil {
		return 0, err
	}
	if s.Kick != nil {
		s.Kick()
	}
	return newID, nil
}

// undo 從現行內容拿掉 after 比 before 多出來的行，被換掉的行放回原位。
func undo(cur, before, after string) (string, error) {
	var added, removed []string
	for _, l := range strings.Split(Diff(before, after), "\n") {
		text, ok := strings.CutPrefix(l, "+ ")
		switch {
		case ok && strings.TrimSpace(text) != "": // 空行留著無妨，刪錯地方反而亂
			added = append(added, text)
		case strings.HasPrefix(l, "- "):
			removed = append(removed, strings.TrimPrefix(l, "- "))
		}
	}
	lines := strings.Split(cur, "\n")
	at := len(lines)
	for _, a := range added {
		i := slices.Index(lines, a)
		if i < 0 {
			return "", fmt.Errorf("撤回不了：「%s」這行已經不在線上（撤回過，或之後又改過），要另外提案修改", a)
		}
		lines = slices.Delete(lines, i, i+1)
		at = min(at, i)
	}
	return strings.Join(slices.Insert(lines, at, removed...), "\n"), nil
}
