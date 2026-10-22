// Package discord 是 Discord 平台實作（plan §7.8）：Gateway 連線、@ 或 DM 觸發、
// 頻道提問自動開討論串、👍／👎 按鈕、通知回原討論串並 @ 提問者。
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/miskawu/footnote/internal/bot"
	"github.com/miskawu/footnote/internal/store"
)

const (
	Platform  = "discord"
	maxLen    = 2000
	fbPrefix  = "fn:fb:"
	askTimout = 90 * time.Second
)

type Bot struct {
	Handler *bot.Handler
	Session *discordgo.Session
	Log     *slog.Logger
}

func New(token string, h *bot.Handler, log *slog.Logger) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	// 只靠 @ 與 DM 觸發，不需要 Message Content 特權 intent
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages
	b := &Bot{Handler: h, Session: s, Log: log}
	s.AddHandler(b.onMessage)
	s.AddHandler(b.onInteraction)
	return b, nil
}

// Run 連上 Gateway，直到 ctx 結束。
func (b *Bot) Run(ctx context.Context) error {
	if err := b.Session.Open(); err != nil {
		return fmt.Errorf("discord open: %w", err)
	}
	b.Log.Info("discord connected", "user", b.Session.State.User.Username)
	<-ctx.Done()
	return b.Session.Close()
}

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || s.State.User == nil {
		return
	}
	me := s.State.User.ID
	dm := m.GuildID == ""
	if !dm && !mentioned(m.Message, me) {
		return
	}
	text := stripMention(m.Content, me)
	if text == "" {
		s.ChannelMessageSendReply(m.ChannelID, "請在 @ 後面直接寫問題，例如：@footnote 正式環境要怎麼回滾？", m.Reference())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), askTimout)
	defer cancel()

	in := bot.Incoming{Platform: Platform, UserRef: m.Author.ID, Text: text, NoHistory: dm}
	replyTo := m.ChannelID
	switch ch := b.channel(m.ChannelID); {
	case dm:
		in.ChannelRef, in.ThreadRef = m.ChannelID, m.ChannelID
	case ch != nil && ch.IsThread():
		in.ChannelRef, in.ThreadRef = ch.ParentID, ch.ID
	default:
		th, err := s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
			Name: threadName(text), AutoArchiveDuration: 1440,
		})
		if err != nil {
			b.Log.Warn("start thread failed; replying in channel", "err", err)
			in.ChannelRef, in.ThreadRef = m.ChannelID, m.ID
		} else {
			in.ChannelRef, in.ThreadRef, replyTo = m.ChannelID, th.ID, th.ID
		}
	}
	s.ChannelTyping(replyTo)

	rep, err := b.Handler.OnQuestion(ctx, in, maxLen)
	if err != nil {
		b.Log.Error("on question", "err", err)
		s.ChannelMessageSend(replyTo, "系統暫時無法回答，請稍後再試。")
		return
	}
	msg := &discordgo.MessageSend{
		Content:         rep.Text,
		Flags:           discordgo.MessageFlagsSuppressEmbeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}
	if rep.Status == "faq" || rep.Status == "docs" {
		msg.Components = feedbackButtons(rep.AskID)
	}
	if _, err := s.ChannelMessageSendComplex(replyTo, msg); err != nil {
		b.Log.Error("send reply", "err", err)
	}
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionMessageComponent {
		return
	}
	id := i.MessageComponentData().CustomID
	if !strings.HasPrefix(id, fbPrefix) {
		return
	}
	askID, vote, ok := parseFeedbackID(id)
	if !ok {
		return
	}
	user := ""
	if i.Member != nil && i.Member.User != nil {
		user = i.Member.User.ID
	} else if i.User != nil {
		user = i.User.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), askTimout)
	defer cancel()
	text, err := b.Handler.OnFeedback(ctx, askID, user, vote)
	if err != nil {
		b.Log.Error("on feedback", "err", err)
		text = "記錄失敗，請稍後再試。"
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral},
	})
}

// ---- 通知（bot.Sender）----

func (b *Bot) Platform() string { return Platform }

func (b *Bot) Send(ctx context.Context, n store.Notice) error {
	if ch, err := b.Session.Channel(n.ThreadRef); err == nil && ch.ThreadMetadata != nil && ch.ThreadMetadata.Archived {
		if ch.ThreadMetadata.Locked {
			return fmt.Errorf("thread %s is locked", n.ThreadRef)
		}
		archived := false
		if _, err := b.Session.ChannelEditComplex(n.ThreadRef, &discordgo.ChannelEdit{Archived: &archived}); err != nil {
			return fmt.Errorf("unarchive: %w", err)
		}
	}
	_, err := b.Session.ChannelMessageSendComplex(n.ThreadRef, &discordgo.MessageSend{
		Content:         "<@" + n.UserRef + "> " + n.Body,
		Flags:           discordgo.MessageFlagsSuppressEmbeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{Users: []string{n.UserRef}},
	})
	return err
}

// ---- 小工具 ----

func (b *Bot) channel(id string) *discordgo.Channel {
	if ch, err := b.Session.State.Channel(id); err == nil {
		return ch
	}
	ch, err := b.Session.Channel(id)
	if err != nil {
		return nil
	}
	return ch
}

func mentioned(m *discordgo.Message, me string) bool {
	for _, u := range m.Mentions {
		if u.ID == me {
			return true
		}
	}
	return false
}

func stripMention(content, me string) string {
	content = strings.ReplaceAll(content, "<@"+me+">", "")
	content = strings.ReplaceAll(content, "<@!"+me+">", "")
	return strings.TrimSpace(content)
}

func threadName(q string) string {
	r := []rune(strings.ReplaceAll(q, "\n", " "))
	if len(r) > 50 {
		return string(r[:50]) + "…"
	}
	return string(r)
}

func feedbackButtons(askID int64) []discordgo.MessageComponent {
	id := strconv.FormatInt(askID, 10)
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "有幫助", Emoji: &discordgo.ComponentEmoji{Name: "👍"}, Style: discordgo.SecondaryButton, CustomID: fbPrefix + id + ":1"},
		discordgo.Button{Label: "沒幫助", Emoji: &discordgo.ComponentEmoji{Name: "👎"}, Style: discordgo.SecondaryButton, CustomID: fbPrefix + id + ":-1"},
	}}}
}

func parseFeedbackID(id string) (int64, int, bool) {
	parts := strings.Split(strings.TrimPrefix(id, fbPrefix), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	askID, err1 := strconv.ParseInt(parts[0], 10, 64)
	vote, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || (vote != 1 && vote != -1) {
		return 0, 0, false
	}
	return askID, vote, true
}
