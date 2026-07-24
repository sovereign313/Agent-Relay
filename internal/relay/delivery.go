package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sovereign313/Agent-Relay/internal/store"
	"github.com/sovereign313/Agent-Relay/internal/telegram"
	"github.com/sovereign313/Agent-Relay/internal/transport"
)

func (s *Service) send(address transport.Address, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sender := s.sender(address)
	if sender == nil {
		s.log.Error("send message", "address", address, "error", "transport unavailable")
		return
	}
	if err := sender.Send(ctx, address.ConversationID, message); err != nil {
		s.log.Error("send message", "address", address, "error_type", fmt.Sprintf("%T", err))
	}
}

func (s *Service) answerAction(transportName, actionID, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sender := s.senders[transportName]
	if sender == nil {
		return
	}
	if err := sender.AnswerAction(ctx, actionID, message); err != nil && !errors.Is(err, transport.ErrUnsupported) {
		s.log.Error("answer transport action", "transport", transportName, "error_type", fmt.Sprintf("%T", err))
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
		sender := s.sender(message.Address())
		if sender == nil {
			_ = s.store.IncrementOutboxAttempts(message.ID)
			s.log.Error("deliver persisted response", "outbox_id", message.ID, "transport", message.Transport, "error", "transport unavailable")
			continue
		}
		sendContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := sender.Send(sendContext, message.ConversationID, message.Text)
		cancel()
		if err != nil {
			_ = s.store.IncrementOutboxAttempts(message.ID)
			s.log.Error("deliver persisted response", "outbox_id", message.ID, "transport", message.Transport, "error_type", fmt.Sprintf("%T", err))
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

func (s *Service) makeOutboxMessages(idPrefix string, address transport.Address, message string) []store.OutboxMessage {
	limit := telegram.MaxMessageLength
	if sender := s.sender(address); sender != nil {
		limit = sender.MaxMessageLength()
	}
	parts := telegram.Split(telegram.Sanitize(message), limit)
	now := time.Now().UTC()
	messages := make([]store.OutboxMessage, 0, len(parts))
	for index, part := range parts {
		messages = append(messages, store.OutboxMessage{
			ID:             fmt.Sprintf("%s:%d", idPrefix, index),
			Transport:      address.Transport,
			ConversationID: address.ConversationID,
			Text:           part,
			CreatedAt:      now.Add(time.Duration(index) * time.Nanosecond),
		})
	}
	return messages
}

func (s *Service) sender(address transport.Address) transport.Sender {
	return s.senders[address.Transport]
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

On Discord, use ! in place of / (for example, !projects).
Send any other text to the selected agent in the selected project.`
