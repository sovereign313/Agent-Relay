package discord

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/sovereign313/Agent-Relay/internal/transport"
)

func TestHandleMessageAuthorizesAndTranslatesCommands(t *testing.T) {
	gateway, err := New(Config{
		Token:               "test",
		AllowedUserIDs:      map[string]struct{}{"123": {}},
		PrivateChannelsOnly: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan transport.Inbound, 1)
	gateway.handleMessage(context.Background(), func(_ context.Context, inbound transport.Inbound) error {
		received <- inbound
		return nil
	}, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:        "1000",
		ChannelID: "dm-channel",
		Content:   "!projects",
		Author:    &discordgo.User{ID: "123"},
	}})

	inbound := <-received
	if inbound.Text != "/projects" {
		t.Fatalf("text = %q, want /projects", inbound.Text)
	}
	if inbound.Sequence != -1000 {
		t.Fatalf("sequence = %d, want -1000", inbound.Sequence)
	}
	if inbound.Address != (transport.Address{Transport: transport.Discord, ConversationID: "dm-channel"}) {
		t.Fatalf("address = %#v", inbound.Address)
	}
	if !inbound.Private {
		t.Fatal("direct message was not marked private")
	}
}

func TestHandleMessageRejectsUnauthorizedAndGuildMessages(t *testing.T) {
	gateway, err := New(Config{
		Token:               "test",
		AllowedUserIDs:      map[string]struct{}{"123": {}},
		PrivateChannelsOnly: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan transport.Inbound, 1)
	sink := func(_ context.Context, inbound transport.Inbound) error {
		received <- inbound
		return nil
	}
	gateway.handleMessage(context.Background(), sink, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "1001", ChannelID: "dm", Content: "hello", Author: &discordgo.User{ID: "999"},
	}})
	gateway.handleMessage(context.Background(), sink, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "1002", ChannelID: "guild", GuildID: "guild-id", Content: "hello", Author: &discordgo.User{ID: "123"},
	}})
	select {
	case inbound := <-received:
		t.Fatalf("rejected message reached sink: %#v", inbound)
	default:
	}
}

func TestApplicationCommandsIncludeNativeProjectAutocomplete(t *testing.T) {
	commands := applicationCommands()
	byName := make(map[string]*discordgo.ApplicationCommand, len(commands))
	for _, command := range commands {
		byName[command.Name] = command
	}
	for _, name := range []string{"project", "agent", "status", "cancel", "retry"} {
		if byName[name] == nil {
			t.Fatalf("command %q is not registered", name)
		}
	}
	project := byName["project"]
	if len(project.Options) != 1 || !project.Options[0].Autocomplete || !project.Options[0].Required {
		t.Fatalf("project command options = %#v", project.Options)
	}
}

func TestDiscordComponentsEnforceLimitsAndDangerStyle(t *testing.T) {
	rows := make([][]transport.Button, 6)
	for index := range rows {
		rows[index] = []transport.Button{{Text: "Cancel", Data: "canceljob:1"}}
	}
	components := discordComponents(rows)
	if len(components) != 5 {
		t.Fatalf("component rows = %d, want 5", len(components))
	}
	row := components[0].(discordgo.ActionsRow)
	button := row.Components[0].(discordgo.Button)
	if button.Style != discordgo.DangerButton {
		t.Fatalf("cancel button style = %v, want danger", button.Style)
	}
}
