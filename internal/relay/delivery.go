package relay

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sovereign313/Agent-Relay/internal/store"
	"github.com/sovereign313/Agent-Relay/internal/telegram"
)

func (s *Service) send(chatID int64, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.telegram.Send(ctx, chatID, message); err != nil {
		s.log.Error("send Telegram message", "chat_id", chatID, "error", err)
	}
}

func (s *Service) answerCallback(callbackID, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.telegram.AnswerCallback(ctx, callbackID, message); err != nil {
		s.log.Error("answer Telegram callback", "error_type", fmt.Sprintf("%T", err))
	}
}

func (s *Service) runOutbox(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	s.deliverOutbox(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.deliverOutbox(ctx)
		case <-s.outboxWake:
			s.deliverOutbox(ctx)
		}
	}
}

func (s *Service) deliverOutbox(ctx context.Context) {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	for _, message := range s.store.Outbox() {
		sendContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := s.telegram.Send(sendContext, message.ChatID, message.Text)
		cancel()
		if err != nil {
			_ = s.store.IncrementOutboxAttempts(message.ID)
			s.log.Error("deliver persisted Telegram response", "outbox_id", message.ID, "error_type", fmt.Sprintf("%T", err))
			continue
		}
		if err := s.store.RemoveOutbox(message.ID); err != nil {
			s.log.Error("remove delivered response", "outbox_id", message.ID, "error_type", fmt.Sprintf("%T", err))
		}
	}
}

func (s *Service) wakeOutbox() {
	select {
	case s.outboxWake <- struct{}{}:
	default:
	}
}

func formatUptime(duration time.Duration) string {
	duration = duration.Round(time.Second)
	if duration < time.Minute {
		return duration.String()
	}
	hours := int(duration.Hours())
	minutes := int(duration.Minutes()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

func makeOutboxMessages(idPrefix string, chatID int64, message string) []store.OutboxMessage {
	parts := telegram.Split(telegram.Sanitize(message), telegram.MaxMessageLength)
	now := time.Now().UTC()
	messages := make([]store.OutboxMessage, 0, len(parts))
	for index, part := range parts {
		messages = append(messages, store.OutboxMessage{
			ID:        fmt.Sprintf("%s:%d", idPrefix, index),
			ChatID:    chatID,
			Text:      part,
			CreatedAt: now.Add(time.Duration(index) * time.Nanosecond),
		})
	}
	return messages
}

func safeError(err error) string {
	message := strings.ReplaceAll(err.Error(), "\x00", "")
	if len(message) > 1000 {
		message = message[:1000] + "..."
	}
	return message
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

const helpText = `Agent Relay commands:
/projects — list discovered Git projects
/project <id> — select a project
/agents — list configured coding agents
/agent <name> — select Codex, Claude, OpenCode, or Grok
/sessions — list saved agent contexts
/queue — list durable jobs for the selected project and agent
/clearqueue — discard queued or interrupted jobs
/retry <job-id> — explicitly retry an interrupted job
/new — start a fresh context for the selected project
/last — resend the last completed agent response
/status — show the current project, agent, and task state
/cancel — interrupt the current task
/cancelall — interrupt every running task for this chat
/refresh — rescan configured project roots
/help — show this message

Send any other text to the selected agent in the selected project.`
