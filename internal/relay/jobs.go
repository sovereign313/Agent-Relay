package relay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sovereign313/Agent-Relay/internal/codex"
	"github.com/sovereign313/Agent-Relay/internal/session"
	"github.com/sovereign313/Agent-Relay/internal/store"
)

func (s *Service) acceptJob(updateID, chatID int64, prompt string) error {
	projectID := s.store.SelectedProject(chatID)
	if projectID == "" {
		if _, err := s.store.AcceptUpdate(updateID, nil, nil); err != nil {
			return err
		}
		s.send(chatID, "Select a project first with /projects and /project <project-id>.")
		return nil
	}
	selected, ok := s.getProject(projectID)
	if !ok {
		if _, err := s.store.AcceptUpdate(updateID, nil, nil); err != nil {
			return err
		}
		s.send(chatID, "The selected project is no longer available. Run /projects and select another.")
		return nil
	}
	status := s.sessions.Status(session.Key{ChatID: chatID, ProjectID: selected.ID})
	if status.Queued >= s.cfg.QueueSize {
		if _, err := s.store.AcceptUpdate(updateID, nil, nil); err != nil {
			return err
		}
		s.send(chatID, "The queue for this project is full.")
		return nil
	}

	now := time.Now().UTC()
	conversation, exists := s.store.Conversation(chatID, selected.ID)
	if !exists {
		conversation = store.Conversation{
			ChatID:      chatID,
			ProjectID:   selected.ID,
			ProjectPath: selected.Path,
			CreatedAt:   now,
		}
	} else if conversation.ProjectPath != "" && conversation.ProjectPath != selected.Path {
		s.log.Warn(
			"project path changed; resetting Codex thread",
			"chat_id", chatID,
			"project", selected.ID,
			"old_path", conversation.ProjectPath,
			"new_path", selected.Path,
		)
		conversation.ThreadID = ""
		conversation.LastResponse = ""
		conversation.CreatedAt = now
	}
	conversation.ProjectPath = selected.Path
	conversation.State = store.StateQueued
	conversation.LastActivity = now
	conversation.LastError = ""
	job := store.PendingJob{
		ID:          updateID,
		ChatID:      chatID,
		ProjectID:   selected.ID,
		ProjectPath: selected.Path,
		Prompt:      prompt,
		CreatedAt:   now,
	}
	accepted, err := s.store.AcceptUpdate(updateID, &job, &conversation)
	if err != nil {
		return fmt.Errorf("persist Telegram job: %w", err)
	}
	if !accepted {
		return nil
	}
	queued, err := s.dispatchJob(job)
	if errors.Is(err, session.ErrQueueFull) {
		if stateErr := s.store.SetJobState(job.ID, store.JobPending); stateErr != nil {
			return stateErr
		}
		s.send(chatID, "The task was saved and will run when queue space is available.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("queue Codex task: %w", err)
	}
	if queued {
		s.send(chatID, "Queued behind the current task.")
	} else {
		s.send(chatID, "Codex is working in "+selected.ID+"...")
	}
	return nil
}

func (s *Service) process(parent context.Context, job session.Job) {
	timeoutContext, cancel := context.WithTimeout(parent, s.cfg.TaskDuration())
	defer cancel()

	conversation, exists := s.store.Conversation(job.Key.ChatID, job.Key.ProjectID)
	if !exists {
		s.completeWithoutConversation(job, "Codex session state was lost before the task started.")
		return
	}
	conversation.State = store.StateWorking
	conversation.LastActivity = time.Now().UTC()
	if err := s.store.StartJob(job.ID, conversation); err != nil {
		s.log.Error("persist working job", "job_id", job.ID, "error_type", fmt.Sprintf("%T", err))
		s.completeWithoutConversation(job, "Could not persist the Codex session.")
		return
	}

	var deliveryText string
	result, err := s.runner.Run(timeoutContext, codex.Request{
		ProjectPath: job.ProjectPath,
		ThreadID:    conversation.ThreadID,
		Prompt:      job.Prompt,
		OnThread: func(threadID string) error {
			conversation.ThreadID = threadID
			conversation.LastActivity = time.Now().UTC()
			return s.store.PutConversation(conversation)
		},
		OnEvent: func(event codex.Event) {
			switch event.Type {
			case "turn.started", "turn.completed", "turn.failed", "error":
				s.log.Debug("codex event", "chat_id", job.Key.ChatID, "project", job.Key.ProjectID, "type", event.Type)
			}
		},
	})
	conversation.LastActivity = time.Now().UTC()
	if result.ThreadID != "" {
		conversation.ThreadID = result.ThreadID
	}
	if err != nil {
		if parent.Err() != nil || errors.Is(err, context.Canceled) {
			conversation.State = store.StateStopped
			conversation.LastError = "cancelled"
			deliveryText = "Codex task cancelled."
		} else if errors.Is(timeoutContext.Err(), context.DeadlineExceeded) {
			conversation.State = store.StateFailed
			conversation.LastError = "task timeout"
			deliveryText = "Codex task exceeded the configured timeout."
		} else {
			conversation.State = store.StateFailed
			conversation.LastError = safeError(err)
			s.log.Error(
				"codex task failed",
				"chat_id", job.Key.ChatID,
				"project", job.Key.ProjectID,
				"job_id", job.ID,
				"error_type", fmt.Sprintf("%T", err),
			)
			deliveryText = "Codex failed: " + safeError(err)
		}
	} else {
		conversation.State = store.StateIdle
		conversation.LastError = ""
		conversation.LastResponse = result.FinalMessage
		deliveryText = result.FinalMessage
	}
	messages := makeOutboxMessages(fmt.Sprintf("job:%d", job.ID), job.Key.ChatID, deliveryText)
	if persistErr := s.store.CompleteJob(job.ID, conversation, messages); persistErr != nil {
		s.log.Error("persist completed conversation", "job_id", job.ID, "error_type", fmt.Sprintf("%T", persistErr))
	}
	s.markDispatched(job.ID, false)
	s.wakeOutbox()
	s.dispatchPending()
}

func (s *Service) dispatchJob(job store.PendingJob) (bool, error) {
	s.dispatchMu.Lock()
	if s.dispatched[job.ID] {
		s.dispatchMu.Unlock()
		return true, nil
	}
	s.dispatched[job.ID] = true
	s.dispatchMu.Unlock()

	if err := s.store.SetJobState(job.ID, store.JobEnqueued); err != nil {
		s.markDispatched(job.ID, false)
		return false, err
	}
	queued, err := s.sessions.Enqueue(session.Job{
		ID:          job.ID,
		Key:         session.Key{ChatID: job.ChatID, ProjectID: job.ProjectID},
		ProjectPath: job.ProjectPath,
		Prompt:      job.Prompt,
	})
	if err != nil {
		s.markDispatched(job.ID, false)
		if stateErr := s.store.SetJobState(job.ID, store.JobPending); stateErr != nil {
			return false, stateErr
		}
		return true, err
	}
	return queued, nil
}

func (s *Service) dispatchPending() {
	for _, job := range s.store.Jobs() {
		if job.State != store.JobPending {
			continue
		}
		if _, err := s.dispatchJob(job); err != nil && !errors.Is(err, session.ErrQueueFull) {
			s.log.Error("dispatch persisted job", "job_id", job.ID, "error_type", fmt.Sprintf("%T", err))
		}
	}
}

func (s *Service) markDispatched(jobID int64, value bool) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	if value {
		s.dispatched[jobID] = true
	} else {
		delete(s.dispatched, jobID)
	}
}

func (s *Service) completeWithoutConversation(job session.Job, text string) {
	now := time.Now().UTC()
	conversation := store.Conversation{
		ChatID:       job.Key.ChatID,
		ProjectID:    job.Key.ProjectID,
		ProjectPath:  job.ProjectPath,
		State:        store.StateFailed,
		CreatedAt:    now,
		LastActivity: now,
		LastError:    text,
	}
	messages := makeOutboxMessages(fmt.Sprintf("job:%d", job.ID), job.Key.ChatID, text)
	if err := s.store.CompleteJob(job.ID, conversation, messages); err != nil {
		s.log.Error("complete failed job", "job_id", job.ID, "error_type", fmt.Sprintf("%T", err))
	}
	s.markDispatched(job.ID, false)
	s.wakeOutbox()
}

func (s *Service) hasPersistedJobs(chatID int64, projectID string) bool {
	for _, job := range s.store.Jobs() {
		if job.ChatID == chatID && job.ProjectID == projectID {
			return true
		}
	}
	return false
}
