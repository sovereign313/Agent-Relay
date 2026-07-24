package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

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

	mu           sync.RWMutex
	health       transport.Health
	interactions map[string]*discordgo.Interaction
}

func New(config Config, logger *slog.Logger) (*Gateway, error) {
	session, err := discordgo.New("Bot " + config.Token)
	if err != nil {
		return nil, fmt.Errorf("create Discord session: %w", err)
	}
	session.Identify.Intents = discordgo.IntentsDirectMessages |
		discordgo.IntentsGuildMessages |
		discordgo.IntentMessageContent
	return &Gateway{
		session:      session,
		config:       config,
		log:          logger,
		health:       transport.Health{State: "starting"},
		interactions: make(map[string]*discordgo.Interaction),
	}, nil
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
		g.setHealth("failed", "Gateway connection failed")
		return fmt.Errorf("open Discord Gateway: %w", err)
	}
	defer g.session.Close()
	if _, err := g.session.ApplicationCommandBulkOverwrite(g.session.State.User.ID, "", applicationCommands(), discordgo.WithContext(ctx)); err != nil {
		g.setHealth("failed", "command registration failed")
		return fmt.Errorf("register Discord commands: %w", err)
	}
	g.setHealth("connected", "Gateway and slash commands")
	g.log.Info("Discord Gateway connected")
	<-ctx.Done()
	g.setHealth("stopped", "daemon stopping")
	return nil
}

func (g *Gateway) Send(ctx context.Context, conversationID, message string) error {
	parts := telegram.Split(telegram.Sanitize(message), maxMessageLength)
	for _, part := range parts {
		if _, err := g.session.ChannelMessageSend(conversationID, part, discordgo.WithContext(ctx)); err != nil {
			return fmt.Errorf("send Discord message: %w", err)
		}
	}
	return nil
}

func (g *Gateway) SendKeyboard(ctx context.Context, conversationID, message string, rows [][]transport.Button) error {
	parts := telegram.Split(telegram.Sanitize(message), maxMessageLength)
	components := discordComponents(rows)
	for _, part := range parts[:len(parts)-1] {
		if _, err := g.session.ChannelMessageSend(conversationID, part, discordgo.WithContext(ctx)); err != nil {
			return fmt.Errorf("send Discord message: %w", err)
		}
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

func (g *Gateway) AnswerAction(ctx context.Context, actionID, message string) error {
	interaction := g.takeInteraction(actionID)
	if interaction == nil {
		return nil
	}
	components := []discordgo.MessageComponent{}
	if _, err := g.session.InteractionResponseEdit(interaction, &discordgo.WebhookEdit{
		Content: &message, Components: &components,
	}, discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("answer Discord action: %w", err)
	}
	return nil
}

func (g *Gateway) SendResponse(ctx context.Context, responseID, message string, rows [][]transport.Button) error {
	interaction := g.takeInteraction(responseID)
	if interaction == nil {
		return fmt.Errorf("Discord interaction %s is unavailable", responseID)
	}
	parts := telegram.Split(telegram.Sanitize(message), maxMessageLength)
	components := discordComponents(rows)
	responseComponents := components
	if len(parts) > 1 {
		responseComponents = []discordgo.MessageComponent{}
	}
	content := parts[0]
	if _, err := g.session.InteractionResponseEdit(interaction, &discordgo.WebhookEdit{
		Content: &content, Components: &responseComponents,
	}, discordgo.WithContext(ctx)); err != nil {
		_ = g.session.InteractionResponseDelete(interaction, discordgo.WithContext(ctx))
		return fmt.Errorf("edit Discord interaction response: %w", err)
	}
	if len(parts) == 1 {
		return nil
	}
	for _, part := range parts[1 : len(parts)-1] {
		if _, err := g.session.ChannelMessageSend(interaction.ChannelID, part, discordgo.WithContext(ctx)); err != nil {
			return fmt.Errorf("send Discord response continuation: %w", err)
		}
	}
	_, err := g.session.ChannelMessageSendComplex(interaction.ChannelID, &discordgo.MessageSend{
		Content: parts[len(parts)-1], Components: components,
	}, discordgo.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("send Discord response continuation: %w", err)
	}
	return nil
}

func (g *Gateway) CreateStatus(ctx context.Context, conversationID, message string, rows [][]transport.Button) (string, error) {
	sent, err := g.session.ChannelMessageSendComplex(conversationID, &discordgo.MessageSend{
		Content:    telegram.Sanitize(message),
		Components: discordComponents(rows),
	}, discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("create Discord status: %w", err)
	}
	return sent.ID, nil
}

func (g *Gateway) EditStatus(ctx context.Context, conversationID, messageID, message string, rows [][]transport.Button) error {
	components := discordComponents(rows)
	edit := discordgo.NewMessageEdit(conversationID, messageID).SetContent(telegram.Sanitize(message))
	edit.Components = &components
	if _, err := g.session.ChannelMessageEditComplex(edit, discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("edit Discord status: %w", err)
	}
	return nil
}

func (g *Gateway) AnswerAutocomplete(ctx context.Context, actionID string, choices []transport.Choice) error {
	interaction := g.takeInteraction(actionID)
	if interaction == nil {
		return nil
	}
	options := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(choices))
	for _, choice := range choices {
		options = append(options, &discordgo.ApplicationCommandOptionChoice{Name: choice.Name, Value: choice.Value})
	}
	return g.session.InteractionRespond(interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: options},
	}, discordgo.WithContext(ctx))
}

func (g *Gateway) Probe(ctx context.Context) error {
	if _, err := g.session.User("@me", discordgo.WithContext(ctx)); err != nil {
		return err
	}
	if err := g.session.Open(); err != nil {
		return err
	}
	return g.session.Close()
}

func (g *Gateway) Health() transport.Health {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.health
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
	if event == nil {
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
		g.rejectInteraction(event, "You are not authorized to use Agent Relay.")
		return
	}
	private := event.GuildID == ""
	if g.config.PrivateChannelsOnly && !private {
		g.rejectInteraction(event, "Use Agent Relay in a direct message.")
		return
	}
	switch event.Type {
	case discordgo.InteractionApplicationCommandAutocomplete:
		g.handleAutocomplete(ctx, sink, event, user, private)
		return
	case discordgo.InteractionApplicationCommand:
		g.handleApplicationCommand(ctx, sink, event, user, private)
		return
	case discordgo.InteractionMessageComponent:
	default:
		return
	}
	if err := g.session.InteractionRespond(event.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}); err != nil {
		g.log.Warn("acknowledge Discord interaction", "error_type", fmt.Sprintf("%T", err))
	}
	g.storeInteraction(event.ID, event.Interaction)
	sequence, err := eventSequence(event.ID)
	if err != nil {
		return
	}
	if err := sink(ctx, transport.Inbound{
		EventID:    transport.Discord + ":" + event.ID,
		ResponseID: event.ID,
		Sequence:   sequence,
		Address:    transport.Address{Transport: transport.Discord, ConversationID: event.ChannelID},
		UserID:     user.ID,
		Private:    private,
		Action:     &transport.Action{ID: event.ID, Data: event.MessageComponentData().CustomID},
	}); err != nil && ctx.Err() == nil {
		g.log.Error("handle Discord interaction", "error_type", fmt.Sprintf("%T", err))
		g.failInteraction(event.ID, event.Interaction)
	}
}

func (g *Gateway) handleApplicationCommand(ctx context.Context, sink transport.Sink, event *discordgo.InteractionCreate, user *discordgo.User, private bool) {
	if err := g.session.InteractionRespond(event.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		g.log.Warn("acknowledge Discord command", "error_type", fmt.Sprintf("%T", err))
		return
	}
	g.storeInteraction(event.ID, event.Interaction)
	data := event.ApplicationCommandData()
	text := "/" + data.Name
	if len(data.Options) > 0 {
		text += " " + data.Options[0].StringValue()
	}
	sequence, err := eventSequence(event.ID)
	if err != nil {
		return
	}
	if err := sink(ctx, transport.Inbound{
		EventID:    transport.Discord + ":" + event.ID,
		ResponseID: event.ID,
		Sequence:   sequence,
		Address:    transport.Address{Transport: transport.Discord, ConversationID: event.ChannelID},
		UserID:     user.ID,
		Private:    private,
		Text:       text,
	}); err != nil && ctx.Err() == nil {
		g.log.Error("handle Discord command", "error_type", fmt.Sprintf("%T", err))
		g.failInteraction(event.ID, event.Interaction)
	}
}

func (g *Gateway) handleAutocomplete(ctx context.Context, sink transport.Sink, event *discordgo.InteractionCreate, user *discordgo.User, private bool) {
	data := event.ApplicationCommandData()
	if len(data.Options) == 0 {
		return
	}
	option := data.Options[0]
	g.storeInteraction(event.ID, event.Interaction)
	if err := sink(ctx, transport.Inbound{
		EventID: transport.Discord + ":" + event.ID,
		Address: transport.Address{Transport: transport.Discord, ConversationID: event.ChannelID},
		UserID:  user.ID,
		Private: private,
		Autocomplete: &transport.Autocomplete{
			ID: event.ID, Command: data.Name, Option: option.Name, Query: option.StringValue(),
		},
	}); err != nil && ctx.Err() == nil {
		g.log.Error("handle Discord autocomplete", "error_type", fmt.Sprintf("%T", err))
		g.takeInteraction(event.ID)
		g.rejectInteraction(event, "")
	}
}

func eventSequence(id string) (int64, error) {
	value, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, err
	}
	return -value, nil
}

func (g *Gateway) storeInteraction(key string, interaction *discordgo.Interaction) {
	g.mu.Lock()
	g.interactions[key] = interaction
	g.mu.Unlock()
}

func (g *Gateway) takeInteraction(key string) *discordgo.Interaction {
	g.mu.Lock()
	defer g.mu.Unlock()
	interaction := g.interactions[key]
	delete(g.interactions, key)
	return interaction
}

func (g *Gateway) setHealth(state, detail string) {
	g.mu.Lock()
	g.health = transport.Health{State: state, Detail: detail}
	g.mu.Unlock()
}

func (g *Gateway) failInteraction(responseID string, interaction *discordgo.Interaction) {
	g.takeInteraction(responseID)
	message := "Agent Relay could not process that request."
	components := []discordgo.MessageComponent{}
	if _, err := g.session.InteractionResponseEdit(interaction, &discordgo.WebhookEdit{
		Content: &message, Components: &components,
	}); err != nil {
		g.log.Warn("edit failed Discord interaction", "error_type", fmt.Sprintf("%T", err))
	}
}

func (g *Gateway) rejectInteraction(event *discordgo.InteractionCreate, message string) {
	if event.Type == discordgo.InteractionApplicationCommandAutocomplete {
		_ = g.session.InteractionRespond(event.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{Choices: []*discordgo.ApplicationCommandOptionChoice{}},
		})
		return
	}
	_ = g.session.InteractionRespond(event.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: message,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func discordComponents(rows [][]transport.Button) []discordgo.MessageComponent {
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
			style := discordgo.SecondaryButton
			if strings.HasPrefix(button.Data, "cancel") {
				style = discordgo.DangerButton
			}
			buttons = append(buttons, discordgo.Button{
				Label: button.Text, Style: style, CustomID: button.Data,
			})
		}
		if len(buttons) > 0 {
			components = append(components, discordgo.ActionsRow{Components: buttons})
		}
	}
	return components
}

func applicationCommands() []*discordgo.ApplicationCommand {
	simple := []struct{ name, description string }{
		{"help", "Show Agent Relay commands"},
		{"agents", "List available coding agents"},
		{"sessions", "List saved agent sessions"},
		{"queue", "Show the selected task queue"},
		{"clearqueue", "Clear queued and interrupted tasks"},
		{"new", "Start a fresh agent context"},
		{"last", "Resend the last completed response"},
		{"status", "Show relay, transport, project, and task status"},
		{"cancel", "Cancel the current task"},
		{"cancelall", "Cancel all tasks for this conversation"},
		{"refresh", "Rescan configured project roots"},
	}
	commands := make([]*discordgo.ApplicationCommand, 0, len(simple)+3)
	for _, item := range simple {
		commands = append(commands, &discordgo.ApplicationCommand{Name: item.name, Description: item.description})
	}
	commands = append(commands, &discordgo.ApplicationCommand{
		Name: "projects", Description: "Browse directories beneath the configured project roots",
		Options: []*discordgo.ApplicationCommandOption{{
			Type: discordgo.ApplicationCommandOptionString, Name: "path",
			Description: "Optional relative directory path", Required: false,
		}},
	})
	for _, item := range []struct{ name, description, option string }{
		{"project", "Select a project directory", "path"},
		{"agent", "Select a coding agent", "agent"},
		{"retry", "Retry an interrupted task", "job-id"},
	} {
		commands = append(commands, &discordgo.ApplicationCommand{
			Name: item.name, Description: item.description,
			Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionString, Name: item.option,
				Description: item.description, Required: true, Autocomplete: item.name != "retry",
			}},
		})
	}
	return commands
}
