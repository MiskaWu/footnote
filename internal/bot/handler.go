// Package bot 是聊天平台共用的處理流程：發問、回饋、頻率限制、通知投遞。
// 各平台（discord、web）只負責收訊息與送訊息。
package bot

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/gap"
	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/render"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
)

type Incoming struct {
	Platform   string
	ChannelRef string
	ThreadRef  string
	UserRef    string
	Text       string
	NoHistory  bool // DM：每則訊息都當新問題
}

type Reply struct {
	AskID  int64  `json:"ask_id"`
	Status string `json:"status"`
	Text   string `json:"text"`
}

type Handler struct {
	Store    *store.Store
	QA       *qa.Engine
	Gaps     *gap.Service
	Embedder qa.Embedder
	Cfg      *config.Config
	Render   render.Options
	Log      *slog.Logger
	limiter  *Limiter
}

func NewHandler(st *store.Store, e *qa.Engine, g *gap.Service, emb qa.Embedder, cfg *config.Config, ro render.Options, log *slog.Logger) *Handler {
	return &Handler{Store: st, QA: e, Gaps: g, Embedder: emb, Cfg: cfg, Render: ro, Log: log,
		limiter: NewLimiter(cfg.Bot.RateLimit.PerMinute, cfg.Bot.RateLimit.PerDay)}
}

// OnQuestion 跑完整線上流程並寫 asks；maxLen 是平台的單則訊息上限（0 不限）。
func (h *Handler) OnQuestion(ctx context.Context, in Incoming, maxLen int) (Reply, error) {
	if !h.limiter.Allow(in.Platform+":"+in.UserRef, time.Now()) {
		return Reply{Status: "rate_limited", Text: render.RateText}, nil
	}
	var history []string
	if !in.NoHistory && h.Cfg.QA.HistoryTurns > 0 {
		var err error
		history, err = h.Store.RecentQuestions(ctx, in.Platform, in.ThreadRef, h.Cfg.QA.HistoryTurns)
		if err != nil {
			return Reply{}, err
		}
	}
	ask := store.Ask{Platform: in.Platform, ChannelRef: in.ChannelRef, ThreadRef: in.ThreadRef, UserRef: in.UserRef, Question: in.Text}
	res, err := h.QA.Run(ctx, in.Text, history, h.Cfg.Bot.Channels[in.ChannelRef])
	if err != nil {
		// 外部呼叫失敗記 reason=error，不記成知識缺口
		h.Log.Error("qa failed", "err", err, "question", in.Text)
		ask.Query, ask.Status, ask.Reason, ask.Reply = qa.BuildQuery(history, in.Text), qa.StatusNoData, "error", render.ErrorText
		id, _ := h.Store.InsertAsk(ctx, ask)
		_ = h.Store.SetAskReply(ctx, id, render.ErrorText)
		return Reply{AskID: id, Status: "error", Text: render.ErrorText}, nil
	}
	ask.Query, ask.Status, ask.Reason, ask.TopScore = res.Query, res.Status, res.Reason, res.Top
	ask.LatencyMS = int(res.Latency.Milliseconds())
	for _, hit := range res.Hits {
		ask.ChunkIDs = append(ask.ChunkIDs, hit.ID)
	}
	id, err := h.Store.InsertAsk(ctx, ask)
	if err != nil {
		return Reply{}, err
	}
	var gi *render.GapInfo
	if res.Status == qa.StatusNoData {
		g, err := h.Gaps.Record(ctx, id, in.Text, res.QueryVec, gap.KindNoData, res.NearMisses, nil)
		if err != nil {
			return Reply{}, err
		}
		gi = &render.GapInfo{ID: g.ID, Hits: g.Hits, Status: g.Status}
	}
	o := h.Render
	o.MaxLen = maxLen
	text := render.Reply(res, gi, o)
	if err := h.Store.SetAskReply(ctx, id, text); err != nil {
		return Reply{}, err
	}
	h.Log.Info("ask", "ask_id", id, "platform", in.Platform, "status", res.Status, "top", res.Top, "latency_ms", ask.LatencyMS)
	return Reply{AskID: id, Status: res.Status, Text: text}, nil
}

// OnFeedback 記錄 👍／👎。👎 記一筆缺口：回 FAQ 卡的是 wrong_answer，回文件段落的是 unhelpful。
func (h *Handler) OnFeedback(ctx context.Context, askID int64, user string, vote int) (string, error) {
	a, err := h.Store.GetAsk(ctx, askID)
	if errors.Is(err, store.ErrNotFound) {
		return "找不到這則回覆。", nil
	}
	if err != nil {
		return "", err
	}
	if user == a.UserRef {
		if err := h.Store.SetAskFeedback(ctx, askID, int16(vote)); err != nil {
			return "", err
		}
	}
	if vote >= 0 || a.Status == qa.StatusNoData || a.Reason == "error" {
		return "已記錄，謝謝。", nil
	}
	kind := gap.KindUnhelpful
	var faqPath *string
	if a.Status == qa.StatusFAQ && len(a.ChunkIDs) > 0 {
		kind = gap.KindWrongAnswer
		if c, err := h.chunkPath(ctx, a.ChunkIDs[0]); err == nil {
			faqPath = &c
		}
	}
	vecs, err := h.Embedder.Embed(ctx, []string{a.Question}, tei.Query)
	if err != nil {
		return "", err
	}
	g, err := h.Gaps.Record(ctx, 0, a.Question, vecs[0], kind, nil, faqPath)
	if err != nil {
		return "", err
	}
	if a.GapID == nil {
		if err := h.Store.SetAskGap(ctx, askID, g.ID); err != nil {
			return "", err
		}
	}
	return "已記錄為知識缺口 G" + strconv.FormatInt(g.ID, 10) + "，補完文件後會回到這裡通知。", nil
}

func (h *Handler) chunkPath(ctx context.Context, chunkID int64) (string, error) {
	var p string
	err := h.Store.Pool.QueryRow(ctx, `SELECT path FROM chunks WHERE id = $1`, chunkID).Scan(&p)
	return p, err
}

// ---- 頻率限制（記憶體內，單一實例足夠） ----

type Limiter struct {
	perMinute, perDay int
	mu                sync.Mutex
	seen              map[string][]time.Time
}

func NewLimiter(perMinute, perDay int) *Limiter {
	return &Limiter{perMinute: perMinute, perDay: perDay, seen: map[string][]time.Time{}}
}

func (l *Limiter) Allow(key string, now time.Time) bool {
	if l.perMinute <= 0 && l.perDay <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.seen[key]
	keep := ts[:0]
	minute := 0
	for _, t := range ts {
		if now.Sub(t) < 24*time.Hour {
			keep = append(keep, t)
			if now.Sub(t) < time.Minute {
				minute++
			}
		}
	}
	if (l.perMinute > 0 && minute >= l.perMinute) || (l.perDay > 0 && len(keep) >= l.perDay) {
		l.seen[key] = keep
		return false
	}
	l.seen[key] = append(keep, now)
	return true
}

// ---- 通知投遞 ----

// Sender 把 outbox 裡某平台的通知送出去。
type Sender interface {
	Platform() string
	Send(ctx context.Context, n store.Notice) error
}

// RunDeliverer 定期把待送的通知交給對應平台；失敗的留著下次再送。
func RunDeliverer(ctx context.Context, st *store.Store, senders []Sender, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for _, s := range senders {
			pending, err := st.PendingNotices(ctx, s.Platform())
			if err != nil {
				log.Error("pending notices", "err", err)
				continue
			}
			for _, n := range pending {
				err := s.Send(ctx, n)
				if err != nil {
					log.Warn("notice send failed", "id", n.ID, "platform", n.Platform, "err", err)
				}
				if err := st.MarkNoticeSent(ctx, n.ID, err); err != nil {
					log.Error("mark notice", "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
