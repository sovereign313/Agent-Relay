package relay

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
	if responseID := s.takeResponse(address); responseID != "" {
		if responder, ok := sender.(transport.ResponseSender); ok {
			if err := responder.SendResponse(ctx, responseID, message, nil); err == nil {
				return
			} else {
				s.log.Warn("send interaction response failed; sending separately", "address", address, "error_type", fmt.Sprintf("%T", err))
			}
		}
	}
	if err := sender.Send(ctx, address.ConversationID, message); err != nil {
		s.log.Error("send message", "address", address, "error_type", fmt.Sprintf("%T", err))
	}
}

func (s *Service) sendKeyboard(address transport.Address, message string, rows [][]transport.Button) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sender := s.sender(address)
	if sender == nil {
		s.log.Error("send keyboard", "address", address, "error", "transport unavailable")
		return
	}
	if responseID := s.takeResponse(address); responseID != "" {
		if responder, ok := sender.(transport.ResponseSender); ok {
			if err := responder.SendResponse(ctx, responseID, message, rows); err == nil {
				return
			} else {
				s.log.Warn("send interaction keyboard failed; sending separately", "address", address, "error_type", fmt.Sprintf("%T", err))
			}
		}
	}
	if err := sender.SendKeyboard(ctx, address.ConversationID, message, rows); err != nil {
		s.log.Error("send keyboard", "address", address, "error_type", fmt.Sprintf("%T", err))
	}
}

func (s *Service) takeResponse(address transport.Address) string {
	s.replyMu.Lock()
	defer s.replyMu.Unlock()
	responseID := s.replies[address]
	delete(s.replies, address)
	return responseID
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
		var err error
		if message.EditMessageID != "" {
			if editor, ok := sender.(transport.StatusEditor); ok {
				err = editor.EditStatus(sendContext, message.ConversationID, message.EditMessageID, message.Text, message.Buttons)
				if err != nil {
					s.log.Warn("edit persisted status failed; sending response separately", "outbox_id", message.ID, "error_type", fmt.Sprintf("%T", err))
					err = sender.Send(sendContext, message.ConversationID, message.Text)
				}
			} else {
				err = sender.Send(sendContext, message.ConversationID, message.Text)
			}
		} else {
			err = sender.Send(sendContext, message.ConversationID, message.Text)
		}
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

type deliveryEdit struct {
	MessageID string
	Buttons   [][]transport.Button
}

func (s *Service) makeOutboxMessages(idPrefix string, address transport.Address, message string, edits ...deliveryEdit) []store.OutboxMessage {
	limit := telegram.MaxMessageLength
	if sender := s.sender(address); sender != nil {
		limit = sender.MaxMessageLength()
	}
	parts := telegram.Split(telegram.Sanitize(message), limit)
	now := time.Now().UTC()
	messages := make([]store.OutboxMessage, 0, len(parts))
	for index, part := range parts {
		outbox := store.OutboxMessage{
			ID:             fmt.Sprintf("%s:%d", idPrefix, index),
			Transport:      address.Transport,
			ConversationID: address.ConversationID,
			Text:           part,
			CreatedAt:      now.Add(time.Duration(index) * time.Nanosecond),
		}
		if index == 0 && len(edits) > 0 {
			outbox.EditMessageID = edits[0].MessageID
			outbox.Buttons = edits[0].Buttons
		}
		messages = append(messages, outbox)
	}
	return messages
}

func (s *Service) createJobStatus(job store.PendingJob, message string, buttons [][]transport.Button) {
	sender := s.sender(job.Address())
	editor, ok := sender.(transport.StatusEditor)
	if !ok {
		s.send(job.Address(), message)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	messageID, err := editor.CreateStatus(ctx, job.ConversationID, message, buttons)
	if err != nil {
		s.log.Warn("create task status failed", "job_id", job.ID, "transport", job.Transport, "error_type", fmt.Sprintf("%T", err))
		s.send(job.Address(), message)
		return
	}
	if err := s.store.SetJobStatusMessage(job.ID, messageID); err != nil {
		s.log.Error("persist task status message", "job_id", job.ID, "error_type", fmt.Sprintf("%T", err))
	}
}

func (s *Service) editJobStatus(job store.PendingJob, message string, buttons [][]transport.Button) {
	if job.StatusMessageID == "" {
		return
	}
	sender := s.sender(job.Address())
	editor, ok := sender.(transport.StatusEditor)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := editor.EditStatus(ctx, job.ConversationID, job.StatusMessageID, message, buttons); err != nil {
		s.log.Warn("edit task status failed", "job_id", job.ID, "transport", job.Transport, "error_type", fmt.Sprintf("%T", err))
	}
}

func cancelButtons(jobID int64) [][]transport.Button {
	return [][]transport.Button{{{Text: "Cancel task", Data: fmt.Sprintf("canceljob:%d", jobID)}}}
}

func retryButtons(jobID int64) [][]transport.Button {
	return [][]transport.Button{{{Text: "Retry task", Data: fmt.Sprintf("retryjob:%d", jobID)}}}
}

func clearQueueButtons() [][]transport.Button {
	return [][]transport.Button{{{Text: "Clear queue", Data: "clearqueue"}}}
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

func jobReference(id int64) string {
	if id < 0 {
		return "d-" + strconv.FormatUint(uint64(-id), 36)
	}
	return "t-" + strconv.FormatUint(uint64(id), 36)
}

func parseJobReference(value string) (int64, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	sign := int64(1)
	switch {
	case strings.HasPrefix(value, "d-"):
		sign = -1
		value = strings.TrimPrefix(value, "d-")
	case strings.HasPrefix(value, "t-"):
		value = strings.TrimPrefix(value, "t-")
	default:
		return strconv.ParseInt(value, 10, 64)
	}
	id, err := strconv.ParseUint(value, 36, 63)
	if err != nil {
		return 0, err
	}
	return int64(id) * sign, nil
}

const helpText = `Agent Relay commands:
/projects [path] — list immediate subdirectories beneath a project root
/project <path> — select a directory beneath a project root
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

Discord supports native slash commands; ! aliases remain available.
Send any other text to the selected agent in the selected project.`
