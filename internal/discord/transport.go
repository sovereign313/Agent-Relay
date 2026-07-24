package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/sovereign313/Agent-Relay/internal/telegram"
	"github.com/sovereign313/Agent-Relay/internal/transport"
)

const maxMessageLength = 2000

type Config struct {
	Token               string
	AllowedUserIDs      map[string]struct{}
	PrivateChannelsOnly bool
}

type Gateway struct {
	session *discordgo.Session
	config  Config
	log     *slog.Logger
}

func New(config Config, logger *slog.Logger) (*Gateway, error) {
	session, err := discordgo.New("Bot " + config.Token)
	if err != nil {
		return nil, fmt.Errorf("create Discord session: %w", err)
	}
	session.Identify.Intents = discordgo.IntentsDirectMessages |
		discordgo.IntentsGuildMessages |
		discordgo.IntentMessageContent
	return &Gateway{session: session, config: config, log: logger}, nil
}

func (g *Gateway) Name() string {
	return transport.Discord
}

func (g *Gateway) MaxMessageLength() int {
	return maxMessageLength
}

func (g *Gateway) Run(ctx context.Context, sink transport.Sink) error {
	removeMessage := g.session.AddHandler(func(_ *discordgo.Session, event *discordgo.MessageCreate) {
		g.handleMessage(ctx, sink, event)
	})
	removeInteraction := g.session.AddHandler(func(_ *discordgo.Session, event *discordgo.InteractionCreate) {
		g.handleInteraction(ctx, sink, event)
	})
	defer removeMessage()
	defer removeInteraction()

	if err := g.session.Open(); err != nil {
		return fmt.Errorf("open Discord Gateway: %w", err)
	}
	defer g.session.Close()
	g.log.Info("Discord Gateway connected")
	<-ctx.Done()
	return nil
}

func (g *Gateway) Send(ctx context.Context, conversationID, message string) error {
	for _, part := range telegram.Split(telegram.Sanitize(message), maxMessageLength) {
		if _, err := g.session.ChannelMessageSend(conversationID, part, discordgo.WithContext(ctx)); err != nil {
			return fmt.Errorf("send Discord message: %w", err)
		}
	}
	return nil
}

func (g *Gateway) SendKeyboard(ctx context.Context, conversationID, message string, rows [][]transport.Button) error {
	parts := telegram.Split(telegram.Sanitize(message), maxMessageLength)
	for _, part := range parts[:len(parts)-1] {
		if _, err := g.session.ChannelMessageSend(conversationID, part, discordgo.WithContext(ctx)); err != nil {
			return fmt.Errorf("send Discord message: %w", err)
		}
	}
	components := make([]discordgo.MessageComponent, 0, len(rows))
	for _, row := range rows {
		if len(components) == 5 {
			break
		}
		buttons := make([]discordgo.MessageComponent, 0, len(row))
		for _, button := range row {
			if len(buttons) == 5 {
				break
			}
			buttons = append(buttons, discordgo.Button{
				Label:    button.Text,
				Style:    discordgo.SecondaryButton,
				CustomID: button.Data,
			})
		}
		components = append(components, discordgo.ActionsRow{Components: buttons})
	}
	_, err := g.session.ChannelMessageSendComplex(conversationID, &discordgo.MessageSend{
		Content:    parts[len(parts)-1],
		Components: components,
	}, discordgo.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("send Discord keyboard: %w", err)
	}
	return nil
}

func (g *Gateway) AnswerAction(context.Context, string, string) error {
	return nil
}

func (g *Gateway) handleMessage(ctx context.Context, sink transport.Sink, event *discordgo.MessageCreate) {
	if event == nil || event.Author == nil || event.Author.Bot {
		return
	}
	if _, allowed := g.config.AllowedUserIDs[event.Author.ID]; !allowed {
		g.log.Warn("unauthorized Discord message", "user_id", event.Author.ID, "channel_id", event.ChannelID)
		return
	}
	private := event.GuildID == ""
	if g.config.PrivateChannelsOnly && !private {
		g.log.Warn("non-private Discord message rejected", "user_id", event.Author.ID, "channel_id", event.ChannelID)
		return
	}
	text := strings.TrimSpace(event.Content)
	if strings.HasPrefix(text, "!") {
		text = "/" + strings.TrimPrefix(text, "!")
	}
	if text == "" {
		return
	}
	sequence, err := eventSequence(event.ID)
	if err != nil {
		g.log.Error("parse Discord event ID", "error_type", fmt.Sprintf("%T", err))
		return
	}
	if err := sink(ctx, transport.Inbound{
		EventID:  transport.Discord + ":" + event.ID,
		Sequence: sequence,
		Address:  transport.Address{Transport: transport.Discord, ConversationID: event.ChannelID},
		UserID:   event.Author.ID,
		Private:  private,
		Text:     text,
	}); err != nil && ctx.Err() == nil {
		g.log.Error("handle Discord message", "error_type", fmt.Sprintf("%T", err))
	}
}

func (g *Gateway) handleInteraction(ctx context.Context, sink transport.Sink, event *discordgo.InteractionCreate) {
	if event == nil || event.Type != discordgo.InteractionMessageComponent {
		return
	}
	user := event.User
	if user == nil && event.Member != nil {
		user = event.Member.User
	}
	if user == nil {
		return
	}
	if _, allowed := g.config.AllowedUserIDs[user.ID]; !allowed {
		g.log.Warn("unauthorized Discord interaction", "user_id", user.ID, "channel_id", event.ChannelID)
		return
	}
	private := event.GuildID == ""
	if g.config.PrivateChannelsOnly && !private {
		return
	}
	if err := g.session.InteractionRespond(event.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}); err != nil {
		g.log.Warn("acknowledge Discord interaction", "error_type", fmt.Sprintf("%T", err))
	}
	sequence, err := eventSequence(event.ID)
	if err != nil {
		return
	}
	if err := sink(ctx, transport.Inbound{
		EventID:  transport.Discord + ":" + event.ID,
		Sequence: sequence,
		Address:  transport.Address{Transport: transport.Discord, ConversationID: event.ChannelID},
		UserID:   user.ID,
		Private:  private,
		Action:   &transport.Action{ID: event.ID, Data: event.MessageComponentData().CustomID},
	}); err != nil && ctx.Err() == nil {
		g.log.Error("handle Discord interaction", "error_type", fmt.Sprintf("%T", err))
	}
}

func eventSequence(id string) (int64, error) {
	value, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, err
	}
	return -value, nil
}
