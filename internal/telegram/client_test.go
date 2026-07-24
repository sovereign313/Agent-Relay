package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSplitPreservesUnicodeWithinLimit(t *testing.T) {
	message := strings.Repeat("hello 🌍\n", 20)
	parts := Split(message, 30)
	if len(parts) < 2 {
		t.Fatal("message was not split")
	}
	for _, part := range parts {
		if !utf8.ValidString(part) {
			t.Fatalf("invalid UTF-8 part: %q", part)
		}
		if utf8.RuneCountInString(part) > 30 {
			t.Fatalf("part has %d runes, limit 30", utf8.RuneCountInString(part))
		}
	}
	if strings.Join(parts, "\n") == "" {
		t.Fatal("split returned no content")
	}
}

func TestSplitEmptyMessage(t *testing.T) {
	parts := Split("  ", MaxMessageLength)
	if len(parts) != 1 || parts[0] != "(empty response)" {
		t.Fatalf("Split empty = %#v", parts)
	}
}

func TestSanitizeRemovesControlCharacters(t *testing.T) {
	got := Sanitize("hello\x00\nworld\x1b")
	if got != "hello\nworld" {
		t.Fatalf("Sanitize = %q", got)
	}
}

func TestClientRedactsTokenFromTransportErrors(t *testing.T) {
	const token = "secret-bot-token"
	client := New(token, "https://example.invalid", &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return nil, errors.New(request.URL.String())
		}),
	})
	_, err := client.GetUpdates(context.Background(), 0)
	if err == nil {
		t.Fatal("GetUpdates succeeded")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("transport error leaked token: %v", err)
	}
}

func TestSendRetriesTransientTelegramFailures(t *testing.T) {
	var attempts atomic.Int32
	httpClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		if attempts.Add(1) < 3 {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader(`{"ok":false,"description":"temporary"}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)),
		}, nil
	})}

	client := New("token", "https://example.invalid", httpClient)
	client.retryDelay = time.Millisecond
	if err := client.Send(context.Background(), 42, "hello"); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
