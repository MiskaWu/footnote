// Package discord 是 Discord 平台實作：Gateway 連線、@ 或 DM 觸發、
// 頻道提問自動開討論串、👍／👎 按鈕、通知回原討論串並 @ 提問者。
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/miskawu/footnote/internal/bot"
	"github.com/miskawu/footnote/internal/kb"
	"github.com/miskawu/footnote/internal/render"
	"github.com/miskawu/footnote/internal/store"
)

const (
	Platform  = "discord"
	maxLen    = 2000
	fbPrefix  = "fn:fb:"
	rvPrefix  = "fn:rv:"
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
	if u := b.Session.State.User; u != nil { // Discord 沒回 READY 時 State.User 是空的
		b.Log.Info("discord connected", "user", u.Username, "id", u.ID)
	} else {
		b.Log.Warn("discord connected without READY; mentions may not work")
	}
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
	in := bot.Incoming{Platform: Platform, UserRef: m.Author.ID, Text: stripMention(m.Content, me), DM: dm}
	var ch *discordgo.Channel
	if !dm {
		ch = b.channel(m.ChannelID)
	}
	in.ChannelRef = m.ChannelID
	if ch != nil && ch.IsThread() {
		in.ChannelRef = ch.ParentID // 討論串的權限跟著上層頻道
	}
	// 先查這裡有沒有開放，再開討論串：沒開放的頻道不留下空的討論串（Handler 也會再擋一次）
	if _, ok := b.Handler.Cfg.Bot.Scope(in.ScopeKey()); !ok {
		b.Log.Info("question where not open", "channel", m.ChannelID, "scope", in.ScopeKey(), "user", m.Author.ID)
		s.ChannelMessageSendReply(m.ChannelID, render.ClosedText, m.Reference())
		return
	}
	if in.Text == "" {
		s.ChannelMessageSendReply(m.ChannelID, "請在 @ 後面直接寫問題，例如：@footnote 回滾要怎麼做？", m.Reference())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), askTimout)
	defer cancel()

	replyTo := m.ChannelID
	switch {
	case dm:
		in.ThreadRef = m.ChannelID
	case ch != nil && ch.IsThread():
		in.ThreadRef = ch.ID
	default:
		th, err := s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
			Name: threadName(in.Text), AutoArchiveDuration: 1440,
		})
		if err != nil {
			b.Log.Warn("start thread failed; replying in channel", "err", err)
			in.ThreadRef = m.ID
		} else {
			in.ThreadRef, replyTo = th.ID, th.ID
		}
	}
	s.ChannelTyping(replyTo)

	rep, err := b.Handler.OnQuestion(ctx, in, maxLen)
	if err != nil {
		b.Log.Error("on question", "err", err)
		s.ChannelMessageSend(replyTo, "系統暫時無法回答，請稍後再試。")
		return
	}
	msg := &discordgo.MessageSend{AllowedMentions: &discordgo.MessageAllowedMentions{}}
	if rep.Card != nil {
		msg.Embeds = []*discordgo.MessageEmbed{cardEmbed(rep.Card)}
	} else {
		msg.Content = rep.Text
		msg.Flags = discordgo.MessageFlagsSuppressEmbeds // 訊息裡的 GitLab 連結不要各自展開成預覽
	}
	if rep.Status == "faq" || rep.Status == "docs" || rep.Status == "answer" {
		msg.Components = feedbackButtons(rep.AskID)
	}
	if _, err := s.ChannelMessageSendComplex(replyTo, msg); err != nil {
		b.Log.Error("send reply", "err", err)
	}
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		switch id := i.MessageComponentData().CustomID; {
		case strings.HasPrefix(id, fbPrefix):
			b.onFeedback(s, i, id)
		case strings.HasPrefix(id, rvPrefix):
			b.onReviewButton(s, i, id)
		}
	case discordgo.InteractionModalSubmit:
		if d := i.ModalSubmitData(); strings.HasPrefix(d.CustomID, rvPrefix) {
			b.onRejectReason(s, i, d)
		}
	}
}

func interactionUser(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// ephemeral 回一則只有按的人看得到的訊息。
func ephemeral(s *discordgo.Session, i *discordgo.InteractionCreate, text string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral},
	})
}

func (b *Bot) onFeedback(s *discordgo.Session, i *discordgo.InteractionCreate, id string) {
	askID, vote, ok := parseFeedbackID(id)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), askTimout)
	defer cancel()
	text, err := b.Handler.OnFeedback(ctx, askID, interactionUser(i), vote)
	if err != nil {
		b.Log.Error("on feedback", "err", err)
		text = "記錄失敗，請稍後再試。"
	}
	ephemeral(s, i, text)
}

// ---- FAQ 提案的審核（bot.ReviewPoster）----

// PostReview 把提案貼到審核頻道，附核准、退回按鈕。
func (b *Bot) PostReview(ctx context.Context, c store.KBChange, p kb.Preview) (string, error) {
	msg, err := b.Session.ChannelMessageSendComplex(b.Handler.Cfg.Bot.ReviewChannel, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{reviewEmbed(c, p)},
		Components:      reviewButtons(c.ID),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

func (b *Bot) onReviewButton(s *discordgo.Session, i *discordgo.InteractionCreate, id string) {
	changeID, action, ok := parseReviewID(id)
	if !ok {
		return
	}
	user := interactionUser(i)
	if b.Handler.KB == nil || !b.Handler.KB.CanReview(user) {
		ephemeral(s, i, kb.ErrNotReviewer.Error()+"。")
		return
	}
	if action == "reject" {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseModal,
			Data: &discordgo.InteractionResponseData{
				CustomID: rvPrefix + strconv.FormatInt(changeID, 10) + ":reason",
				Title:    "退回提案",
				Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.TextInput{CustomID: "reason", Label: "退回原因（寫卡的代理會看到）", Style: discordgo.TextInputParagraph, Required: true, MaxLength: 500},
				}}},
			},
		})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), askTimout)
	defer cancel()
	err := b.Handler.KB.Approve(ctx, changeID, user)
	b.finishReview(s, i, err, "✅ <@"+user+"> 已核准，馬上同步上線。")
}

func (b *Bot) onRejectReason(s *discordgo.Session, i *discordgo.InteractionCreate, d discordgo.ModalSubmitInteractionData) {
	changeID, _, ok := parseReviewID(d.CustomID)
	if !ok || b.Handler.KB == nil {
		return
	}
	reason := modalText(d)
	user := interactionUser(i)
	ctx, cancel := context.WithTimeout(context.Background(), askTimout)
	defer cancel()
	err := b.Handler.KB.Reject(ctx, changeID, user, reason)
	b.finishReview(s, i, err, "↩️ <@"+user+"> 已退回："+reason)
}

// finishReview 收掉提案訊息的按鈕，把結果寫在訊息上；沒通過再次檢查時按鈕留著，只告訴按的人。
func (b *Bot) finishReview(s *discordgo.Session, i *discordgo.InteractionCreate, err error, done string) {
	var lint *kb.LintError
	switch {
	case errors.As(err, &lint):
		ephemeral(s, i, clip(err.Error(), maxLen))
		return
	case errors.Is(err, kb.ErrNotReviewer):
		ephemeral(s, i, err.Error()+"。")
		return
	case errors.Is(err, kb.ErrDecided):
		done = err.Error() + "。"
	case err != nil:
		b.Log.Error("review", "err", err)
		ephemeral(s, i, "審核失敗，請稍後再試。")
		return
	}
	data := &discordgo.InteractionResponseData{
		Content: done, Components: []discordgo.MessageComponent{}, AllowedMentions: &discordgo.MessageAllowedMentions{},
	}
	if i.Message != nil {
		data.Embeds = i.Message.Embeds // 這個欄位留空會把原本的卡片清掉
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseUpdateMessage, Data: data})
}

func modalText(d discordgo.ModalSubmitInteractionData) string {
	for _, c := range d.Components {
		row, ok := c.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, x := range row.Components {
			if t, ok := x.(*discordgo.TextInput); ok {
				return strings.TrimSpace(t.Value)
			}
		}
	}
	return ""
}

// reviewEmbed 是審核時看到的卡片：修改看差異，新增與刪除看卡片本身。
func reviewEmbed(c store.KBChange, p kb.Preview) *discordgo.MessageEmbed {
	var desc strings.Builder
	switch {
	case p.Diff != "":
		// 留空間給 code block 的框；內容裡的三個反引號拆開，不然會把框提早關掉
		desc.WriteString("```diff\n" + clip(strings.ReplaceAll(p.Diff, "```", "` ` `"), embedDescMax-16) + "\n```")
	default:
		desc.WriteString(clip(p.Body, embedDescMax))
	}
	e := &discordgo.MessageEmbed{
		Title:       clip(fmt.Sprintf("提案 #%d：%s", c.ID, p.Title), embedTitleMax),
		Description: desc.String(),
		Color:       cardColors["partial"],
		Footer:      &discordgo.MessageEmbedFooter{Text: "提案者：" + c.ProposedBy},
	}
	field := func(name, v string) {
		if v != "" {
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: name, Value: clip(v, embedFieldMax)})
		}
	}
	field("回答的缺口", p.Gap)
	field("說明", c.Note)
	field("問法與來源", strings.Join(p.Lines, "\n"))
	return e
}

func reviewButtons(id int64) []discordgo.MessageComponent {
	s := strconv.FormatInt(id, 10)
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "核准上線", Style: discordgo.SuccessButton, CustomID: rvPrefix + s + ":approve"},
		discordgo.Button{Label: "退回", Style: discordgo.SecondaryButton, CustomID: rvPrefix + s + ":reject"},
	}}}
}

func parseReviewID(id string) (int64, string, bool) {
	idPart, action, ok := strings.Cut(strings.TrimPrefix(id, rvPrefix), ":")
	n, err := strconv.ParseInt(idPart, 10, 64)
	if !ok || err != nil || (action != "approve" && action != "reject" && action != "reason") {
		return 0, "", false
	}
	return n, action, true
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

// ---- 卡片 ----

// 邊條顏色：綠＝完整回答、黃＝部分回答或 FAQ 卡過期、藍＝FAQ 卡。
var cardColors = map[string]int{"answered": 0x3BA55C, "partial": 0xF0B232, "stale": 0xF0B232, "faq": 0x5865F2}

// Discord embed 的長度上限（字元）；留一點餘裕。
const (
	embedTitleMax  = 250
	embedDescMax   = 4000
	embedFieldMax  = 1000
	embedFieldsMax = 20
)

// cardEmbed 把卡片轉成 embed：結論與條列放描述，注意事項一欄，出處放欄位（超過單欄上限就分成好幾欄），頁尾放提示。
// embed 裡的連結不會產生預覽，所以不用 SUPPRESS_EMBEDS。
func cardEmbed(c *render.Card) *discordgo.MessageEmbed {
	e := &discordgo.MessageEmbed{
		Title:       clip(c.Title, embedTitleMax),
		Description: clip(c.Body, embedDescMax),
		Color:       cardColors[c.Color],
	}
	if c.Footer != "" {
		e.Footer = &discordgo.MessageEmbedFooter{Text: c.Footer}
	}
	if len(c.Notes) > 0 {
		var lines []string
		for _, n := range c.Notes {
			lines = append(lines, "- "+n)
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "⚠ 注意", Value: clip(strings.Join(lines, "\n"), embedFieldMax)})
	}
	sourceStart := len(e.Fields)
	var cur []string
	size := 0
	flush := func() {
		if len(cur) == 0 || len(e.Fields) >= embedFieldsMax {
			return
		}
		name := "出處"
		if len(e.Fields) > sourceStart {
			name = "\u200b" // 續欄不重複標題
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: name, Value: strings.Join(cur, "\n")})
		cur, size = nil, 0
	}
	for _, s := range c.Sources {
		s = clip(s, embedFieldMax)
		n := len([]rune(s)) + 1
		if size+n > embedFieldMax {
			flush()
		}
		cur, size = append(cur, s), size+n
	}
	flush()
	return e
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
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
