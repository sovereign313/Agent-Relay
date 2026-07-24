package telegram

import (
	"context"
	"strconv"
	"sync"

	relaytransport "github.com/sovereign313/Agent-Relay/internal/transport"
)

type RelayTransport struct {
	client *Client
	mu     sync.RWMutex
	health relaytransport.Health
}

func NewRelayTransport(client *Client) *RelayTransport {
	return &RelayTransport{
		client: client,
		health: relaytransport.Health{State: "starting"},
	}
}

func (t *RelayTransport) Name() string {
	return relaytransport.Telegram
}

func (t *RelayTransport) MaxMessageLength() int {
	return MaxMessageLength
}

func (t *RelayTransport) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	updates, err := t.client.GetUpdates(ctx, offset)
	if err != nil {
		t.setHealth("degraded", "long polling failed")
		return nil, err
	}
	t.setHealth("connected", "long polling")
	return updates, nil
}

func (t *RelayTransport) Send(ctx context.Context, conversationID, message string) error {
	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		return err
	}
	return t.client.Send(ctx, chatID, message)
}

func (t *RelayTransport) SendKeyboard(ctx context.Context, conversationID, message string, rows [][]relaytransport.Button) error {
	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		return err
	}
	keyboard := make([][]Button, len(rows))
	for rowIndex, row := range rows {
		keyboard[rowIndex] = make([]Button, len(row))
		for buttonIndex, button := range row {
			keyboard[rowIndex][buttonIndex] = Button{Text: button.Text, Data: button.Data}
		}
	}
	return t.client.SendKeyboard(ctx, chatID, message, keyboard)
}

func (t *RelayTransport) AnswerAction(ctx context.Context, actionID, message string) error {
	return t.client.AnswerCallback(ctx, actionID, message)
}

func (t *RelayTransport) CreateStatus(ctx context.Context, conversationID, message string, rows [][]relaytransport.Button) (string, error) {
	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		return "", err
	}
	messageID, err := t.client.CreateStatus(ctx, chatID, message, telegramButtons(rows))
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(messageID, 10), nil
}

func (t *RelayTransport) EditStatus(ctx context.Context, conversationID, messageID, message string, rows [][]relaytransport.Button) error {
	chatID, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil {
		return err
	}
	parsedMessageID, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		return err
	}
	return t.client.EditStatus(ctx, chatID, parsedMessageID, message, telegramButtons(rows))
}

func (t *RelayTransport) Probe(ctx context.Context) error {
	_, err := t.client.GetMe(ctx)
	return err
}

func (t *RelayTransport) Health() relaytransport.Health {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.health
}

func (t *RelayTransport) setHealth(state, detail string) {
	t.mu.Lock()
	t.health = relaytransport.Health{State: state, Detail: detail}
	t.mu.Unlock()
}

func telegramButtons(rows [][]relaytransport.Button) [][]Button {
	keyboard := make([][]Button, len(rows))
	for rowIndex, row := range rows {
		keyboard[rowIndex] = make([]Button, len(row))
		for buttonIndex, button := range row {
			keyboard[rowIndex][buttonIndex] = Button{Text: button.Text, Data: button.Data}
		}
	}
	return keyboard
}
