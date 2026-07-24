package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	longPollTimeout  = 50
	sendAttempts     = 4
	MaxMessageLength = 4096
)

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	From      User   `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

type inlineKeyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

type Client struct {
	token      string
	baseURL    string
	httpClient *http.Client
	retryDelay time.Duration
}

func New(token, baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 65 * time.Second}
	}
	return &Client{
		token:      token,
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
		retryDelay: time.Second,
	}
}

func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	values := url.Values{}
	values.Set("offset", strconv.FormatInt(offset, 10))
	values.Set("timeout", strconv.Itoa(longPollTimeout))
	values.Set("allowed_updates", `["message","callback_query"]`)

	var updates []Update
	if err := c.call(ctx, http.MethodPost, "getUpdates", values, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	var user User
	if err := c.call(ctx, http.MethodPost, "getMe", nil, &user); err != nil {
		return User{}, err
	}
	return user, nil
}

func (c *Client) Send(ctx context.Context, chatID int64, message string) error {
	return c.send(ctx, chatID, message, nil)
}

func (c *Client) SendKeyboard(ctx context.Context, chatID int64, message string, rows [][]Button) error {
	keyboard, err := json.Marshal(inlineKeyboard{Rows: rows})
	if err != nil {
		return err
	}
	return c.send(ctx, chatID, message, keyboard)
}

func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string) error {
	values := url.Values{}
	values.Set("callback_query_id", callbackID)
	if text != "" {
		values.Set("text", text)
	}
	return c.call(ctx, http.MethodPost, "answerCallbackQuery", values, nil)
}

func (c *Client) CreateStatus(ctx context.Context, chatID int64, message string, rows [][]Button) (int64, error) {
	if rows == nil {
		rows = make([][]Button, 0)
	}
	keyboard, err := json.Marshal(inlineKeyboard{Rows: rows})
	if err != nil {
		return 0, err
	}
	values := messageValues(chatID, message)
	values.Set("reply_markup", string(keyboard))
	var sent Message
	if err := c.sendWithRetry(ctx, "sendMessage", values, &sent); err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

func (c *Client) EditStatus(ctx context.Context, chatID, messageID int64, message string, rows [][]Button) error {
	if rows == nil {
		rows = make([][]Button, 0)
	}
	keyboard, err := json.Marshal(inlineKeyboard{Rows: rows})
	if err != nil {
		return err
	}
	values := messageValues(chatID, message)
	values.Set("message_id", strconv.FormatInt(messageID, 10))
	values.Set("reply_markup", string(keyboard))
	return c.sendWithRetry(ctx, "editMessageText", values, nil)
}

func (c *Client) send(ctx context.Context, chatID int64, message string, keyboard []byte) error {
	message = Sanitize(message)
	parts := Split(message, MaxMessageLength)
	for index, part := range parts {
		values := messageValues(chatID, part)
		if index == len(parts)-1 && len(keyboard) > 0 {
			values.Set("reply_markup", string(keyboard))
		}
		if err := c.sendWithRetry(ctx, "sendMessage", values, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) sendWithRetry(ctx context.Context, endpoint string, values url.Values, destination any) error {
	var lastErr error
	for attempt := 0; attempt < sendAttempts; attempt++ {
		lastErr = c.call(ctx, http.MethodPost, endpoint, values, destination)
		if lastErr == nil {
			return nil
		}
		var apiErr *APIError
		if errors.As(lastErr, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != http.StatusTooManyRequests {
			return lastErr
		}
		if attempt == sendAttempts-1 {
			break
		}
		delay := c.retryDelay << attempt
		if errors.As(lastErr, &apiErr) && apiErr.RetryAfter > 0 {
			delay = time.Duration(apiErr.RetryAfter) * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

func messageValues(chatID int64, message string) url.Values {
	values := url.Values{}
	values.Set("chat_id", strconv.FormatInt(chatID, 10))
	values.Set("text", Sanitize(message))
	values.Set("disable_web_page_preview", "true")
	return values
}

func (c *Client) call(ctx context.Context, method, endpoint string, values url.Values, destination any) error {
	apiURL := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, endpoint)
	request, err := http.NewRequestWithContext(ctx, method, apiURL, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("telegram %s: %s", endpoint, c.redact(err.Error()))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		return fmt.Errorf("telegram %s response: %w", endpoint, err)
	}

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("telegram %s returned HTTP %d with invalid JSON", endpoint, response.StatusCode)
	}
	if !envelope.OK {
		if envelope.Description == "" {
			envelope.Description = http.StatusText(response.StatusCode)
		}
		return &APIError{
			Endpoint:    endpoint,
			StatusCode:  response.StatusCode,
			Description: envelope.Description,
			RetryAfter:  envelope.Parameters.RetryAfter,
		}
	}
	if destination != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, destination); err != nil {
			return fmt.Errorf("telegram %s result: %w", endpoint, err)
		}
	}
	return nil
}

type APIError struct {
	Endpoint    string
	StatusCode  int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %s", e.Endpoint, e.Description)
}

func (c *Client) redact(message string) string {
	if c.token == "" {
		return message
	}
	return strings.ReplaceAll(message, c.token, "[REDACTED]")
}

func Sanitize(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n', r == '\t':
			return r
		case r >= 0x20 && r != 0x7f:
			return r
		default:
			return -1
		}
	}, message)
}

func Split(message string, limit int) []string {
	message = strings.TrimSpace(message)
	if message == "" {
		return []string{"(empty response)"}
	}
	if limit <= 0 {
		return []string{message}
	}

	var parts []string
	for utf8.RuneCountInString(message) > limit {
		cutByte := byteIndexAtRune(message, limit)
		prefix := message[:cutByte]
		if newline := strings.LastIndex(prefix, "\n"); newline > limit/2 {
			cutByte = newline
		} else if space := strings.LastIndex(prefix, " "); space > limit/2 {
			cutByte = space
		}
		part := strings.TrimSpace(message[:cutByte])
		if part == "" {
			cutByte = byteIndexAtRune(message, limit)
			part = message[:cutByte]
		}
		parts = append(parts, part)
		message = strings.TrimSpace(message[cutByte:])
	}
	if message != "" {
		parts = append(parts, message)
	}
	return parts
}

func byteIndexAtRune(value string, count int) int {
	if count <= 0 {
		return 0
	}
	index := 0
	for index < len(value) && count > 0 {
		_, size := utf8.DecodeRuneInString(value[index:])
		index += size
		count--
	}
	return index
}
