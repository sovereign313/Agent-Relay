package telegram

import (
	"context"
	"strconv"

	relaytransport "github.com/sovereign313/Agent-Relay/internal/transport"
)

type RelayTransport struct {
	client *Client
}

func NewRelayTransport(client *Client) *RelayTransport {
	return &RelayTransport{client: client}
}

func (t *RelayTransport) Name() string {
	return relaytransport.Telegram
}

func (t *RelayTransport) MaxMessageLength() int {
	return MaxMessageLength
}

func (t *RelayTransport) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	return t.client.GetUpdates(ctx, offset)
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
