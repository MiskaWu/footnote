// Package gap 管理知識缺口：去重、狀態指令、recheck、通知（plan §7.4、§7.5）。
// 缺口狀態只存在 gaps 表；通知只寫 notices（outbox），由 serve 依平台送出。
package gap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/render"
	"github.com/miskawu/footnote/internal/store"
)

const (
	KindNoData      = "no_data"
	KindUnhelpful   = "unhelpful"
	KindWrongAnswer = "wrong_answer"

	StatusOpen          = "open"
	StatusMaybeResolved = "maybe_resolved"
	StatusNeedsHuman    = "needs_human"
	StatusClosed        = "closed"
	StatusDismissed     = "dismissed"

	noticeMaxLen = 1800 // 留空間給 Discord 的 @提及
)

type Service struct {
	Store  *store.Store
	QA     *qa.Engine
	TauDup float64
	Render render.Options
	Log    *slog.Logger
}

type NearMiss struct {
	Project string  `json:"project"`
	Path    string  `json:"path"`
	Heading string  `json:"heading"`
	Score   float32 `json:"score"`
	Link    string  `json:"link"`
}

// Record 把這次發問記進缺口：相似度 ≥ τ_dup 的併入既有缺口，否則新建。
func (s *Service) Record(ctx context.Context, askID int64, question string, vec []float32, kind string, near []qa.Hit, faqPath *string) (*store.Gap, error) {
	g, sim, err := s.Store.NearestGap(ctx, vec)
	if err != nil {
		return nil, err
	}
	if g != nil && sim >= s.TauDup {
		g, err = s.Store.BumpGap(ctx, g.ID)
	} else {
		nm := make([]NearMiss, 0, len(near))
		for _, h := range near {
			nm = append(nm, NearMiss{
				Project: h.ProjectPath, Path: h.Path, Heading: h.Heading, Score: h.Score,
				Link: render.Link(s.Render.GitLabURL, h.ProjectPath, h.Ref, h.Path, h.LineStart),
			})
		}
		g, err = s.Store.InsertGap(ctx, question, kind, vec, nm, faqPath)
	}
	if err != nil {
		return nil, err
	}
	if askID > 0 {
		if err := s.Store.SetAskGap(ctx, askID, g.ID); err != nil {
			return nil, err
		}
	}
	return g, nil
}

func (s *Service) Dismiss(ctx context.Context, id int64) error {
	return s.Store.SetGapStatus(ctx, id, StatusDismissed, nil)
}

func (s *Service) NeedsHuman(ctx context.Context, id int64, note string) error {
	if note == "" {
		return errors.New("needs-human 要附 --note：查過哪裡、缺什麼、可能要問誰")
	}
	return s.Store.SetGapStatus(ctx, id, StatusNeedsHuman, &note)
}

// Close 是人工確認已解決：設為 closed，重跑一次，閘門 A 通過就通知。
func (s *Service) Close(ctx context.Context, id int64) (notified bool, err error) {
	g, err := s.Store.GetGap(ctx, id)
	if err != nil {
		return false, err
	}
	if err := s.Store.SetGapStatus(ctx, id, StatusClosed, nil); err != nil {
		return false, err
	}
	res, err := s.QA.Run(ctx, g.Question, nil, nil)
	if err != nil {
		return false, err
	}
	if res.Status == qa.StatusNoData {
		return false, nil
	}
	return s.notify(ctx, g, res)
}

type RecheckSummary struct {
	Checked, ClosedByFAQ, MaybeResolved, Notified int
}

// Recheck 依序看：有 FAQ 卡指向這個缺口 ⇒ closed 並通知；kind=no_data 重跑後閘門 A 通過 ⇒ maybe_resolved 並通知。
func (s *Service) Recheck(ctx context.Context) (RecheckSummary, error) {
	var sum RecheckSummary
	gaps, err := s.Store.ListGaps(ctx, []string{StatusOpen, StatusMaybeResolved, StatusNeedsHuman})
	if err != nil {
		return sum, err
	}
	for _, g := range gaps {
		g := g
		sum.Checked++
		faqs, err := s.Store.FAQsForGap(ctx, g.ID)
		if err != nil {
			return sum, err
		}
		if len(faqs) > 0 {
			res, err := s.FAQResult(ctx, faqs[0].ProjectPath, faqs[0].Path)
			if err != nil {
				return sum, err
			}
			if err := s.Store.SetGapStatus(ctx, g.ID, StatusClosed, nil); err != nil {
				return sum, err
			}
			sum.ClosedByFAQ++
			if ok, err := s.notify(ctx, &g, res); err != nil {
				return sum, err
			} else if ok {
				sum.Notified++
			}
			continue
		}
		// unhelpful／wrong_answer 當初就通過了閘門 A，分數過關不代表解決了
		if g.Kind == KindNoData && g.Status != StatusMaybeResolved {
			res, err := s.QA.Run(ctx, g.Question, nil, nil)
			if err != nil {
				return sum, err
			}
			if res.Status != qa.StatusNoData {
				if err := s.Store.SetGapStatus(ctx, g.ID, StatusMaybeResolved, nil); err != nil {
					return sum, err
				}
				sum.MaybeResolved++
				if ok, err := s.notify(ctx, &g, res); err != nil {
					return sum, err
				} else if ok {
					sum.Notified++
				}
				continue
			}
		}
		if err := s.Store.TouchGapChecked(ctx, g.ID); err != nil {
			return sum, err
		}
	}
	if sum.ClosedByFAQ+sum.MaybeResolved > 0 {
		s.Log.Info("recheck", "checked", sum.Checked, "closed_by_faq", sum.ClosedByFAQ, "maybe_resolved", sum.MaybeResolved, "notified", sum.Notified)
	}
	return sum, nil
}

// FAQResult 把一張已入庫的卡組成 FAQ 回覆用的結果。
func (s *Service) FAQResult(ctx context.Context, project, path string) (*qa.Result, error) {
	c, err := s.Store.FAQChunk(ctx, project, path)
	if err != nil {
		return nil, fmt.Errorf("faq %s: %w", path, err)
	}
	f, err := s.Store.GetFAQ(ctx, project, path)
	if err != nil {
		return nil, err
	}
	return &qa.Result{Status: qa.StatusFAQ, Hits: []qa.Hit{{Candidate: *c, Score: 1}}, FAQ: f}, nil
}

// notify 對問過這個缺口的每個討論串排一則通知；每個缺口只通知一次。
func (s *Service) notify(ctx context.Context, g *store.Gap, res *qa.Result) (bool, error) {
	first, err := s.Store.MarkGapNotified(ctx, g.ID)
	if err != nil || !first {
		return false, err
	}
	asks, err := s.Store.AsksForGap(ctx, g.ID)
	if err != nil {
		return false, err
	}
	o := s.Render
	o.MaxLen = noticeMaxLen
	body := render.Notice(g.Question, render.Reply(res, nil, o))
	seen := map[string]bool{}
	for _, a := range asks {
		key := a.Platform + "\x00" + a.ThreadRef + "\x00" + a.UserRef
		if a.Platform == "cli" || seen[key] {
			continue
		}
		seen[key] = true
		gid := g.ID
		if err := s.Store.EnqueueNotice(ctx, store.Notice{
			GapID: &gid, Platform: a.Platform, ChannelRef: a.ChannelRef, ThreadRef: a.ThreadRef, UserRef: a.UserRef, Body: body,
		}); err != nil {
			return false, err
		}
	}
	return true, nil
}
