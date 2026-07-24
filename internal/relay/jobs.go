package relay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sovereign313/Agent-Relay/internal/agent"
	"github.com/sovereign313/Agent-Relay/internal/session"
	"github.com/sovereign313/Agent-Relay/internal/store"
	"github.com/sovereign313/Agent-Relay/internal/transport"
)

func (s *Service) acceptJob(inbound transport.Inbound) error {
	updateID := inbound.Sequence
	address := inbound.Address
	prompt := inbound.Text
	projectID := s.store.SelectedProject(address)
	if projectID == "" {
		if _, err := s.acceptInbound(inbound, nil, nil); err != nil {
			return err
		}
		s.send(address, "Select a project first with /projects and /project <relative-path>.")
		return nil
	}
	selected, ok := s.getProject(projectID)
	if !ok {
		if _, err := s.acceptInbound(inbound, nil, nil); err != nil {
			return err
		}
		s.send(address, "The selected project is no longer available. Run /projects and select another.")
		return nil
	}
	agentName := s.selectedAgent(address)
	if _, ok := s.runners[agentName]; !ok {
		if _, err := s.acceptInbound(inbound, nil, nil); err != nil {
			return err
		}
		s.send(address, "The selected agent is unavailable. Run /agents and select another.")
		return nil
	}
	status := s.sessions.Status(session.Key{Address: address, ProjectID: selected.ID, AgentName: agentName})
	if status.Queued >= s.cfg.QueueSize {
		if _, err := s.acceptInbound(inbound, nil, nil); err != nil {
			return err
		}
		s.send(address, "The queue for this project is full.")
		return nil
	}

	now := time.Now().UTC()
	conversation, exists := s.store.Conversation(address, selected.ID, agentName)
	if !exists {
		conversation = store.Conversation{
			Transport:      address.Transport,
			ConversationID: address.ConversationID,
			ProjectID:      selected.ID,
			ProjectPath:    selected.Path,
			AgentName:      agentName,
			CreatedAt:      now,
		}
	} else if conversation.ProjectPath != "" && conversation.ProjectPath != selected.Path {
		s.log.Warn(
			"project path changed; resetting agent thread",
			"agent", agentName,
			"chat_id", address,
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
		ID:             updateID,
		Transport:      address.Transport,
		ConversationID: address.ConversationID,
		ProjectID:      selected.ID,
		ProjectPath:    selected.Path,
		AgentName:      agentName,
		Prompt:         prompt,
		CreatedAt:      now,
	}
	accepted, err := s.acceptInbound(inbound, &job, &conversation)
	if err != nil {
		return fmt.Errorf("persist transport job: %w", err)
	}
	if !accepted {
		return nil
	}
	queuePosition := 0
	if status.Working || status.Queued > 0 {
		queuePosition = status.Queued + 1
	}
	if queuePosition > 0 {
		s.createJobStatus(job, fmt.Sprintf("Queued #%d for %s in %s.\nJob: %s", queuePosition, agentName, selected.ID, jobReference(job.ID)), clearQueueButtons())
	} else {
		s.createJobStatus(job, fmt.Sprintf("Starting %s in %s...\nJob: %s", agentName, selected.ID, jobReference(job.ID)), cancelButtons(job.ID))
	}
	_, err = s.dispatchJob(job)
	if errors.Is(err, session.ErrQueueFull) {
		if stateErr := s.store.SetJobState(job.ID, store.JobPending); stateErr != nil {
			return stateErr
		}
		if persisted, ok := s.store.Job(job.ID); ok {
			s.editJobStatus(persisted, "The task was saved and will run when queue space is available.\nJob: "+jobReference(job.ID), clearQueueButtons())
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("queue agent task: %w", err)
	}
	return nil
}

func (s *Service) process(parent context.Context, job session.Job) {
	timeoutContext, cancel := context.WithTimeout(parent, s.cfg.TaskDuration())
	defer cancel()

	conversation, exists := s.store.Conversation(job.Key.Address, job.Key.ProjectID, job.Key.AgentName)
	if !exists {
		s.completeWithoutConversation(job, "Agent session state was lost before the task started.")
		return
	}
	conversation.State = store.StateWorking
	conversation.LastActivity = time.Now().UTC()
	if err := s.store.StartJob(job.ID, conversation); err != nil {
		s.log.Error("persist working job", "job_id", job.ID, "error_type", fmt.Sprintf("%T", err))
		s.completeWithoutConversation(job, "Could not persist the agent session.")
		return
	}
	if persisted, ok := s.store.Job(job.ID); ok {
		s.editJobStatus(persisted, fmt.Sprintf("%s is working in %s...\nJob: %s", job.Key.AgentName, job.Key.ProjectID, jobReference(job.ID)), cancelButtons(job.ID))
	}

	var deliveryText string
	runner, ok := s.runners[job.Key.AgentName]
	if !ok {
		s.completeWithoutConversation(job, "The configured agent is no longer available.")
		return
	}
	result, err := runner.Run(timeoutContext, agent.Request{
		ProjectPath: job.ProjectPath,
		ThreadID:    conversation.ThreadID,
		Prompt:      job.Prompt,
		OnThread: func(threadID string) error {
			conversation.ThreadID = threadID
			conversation.LastActivity = time.Now().UTC()
			return s.store.PutConversation(conversation)
		},
		OnEvent: func(event agent.Event) {
			switch event.Type {
			case "turn.started", "turn.completed", "turn.failed", "error":
				s.log.Debug("agent event", "agent", job.Key.AgentName, "chat_id", job.Key.Address, "project", job.Key.ProjectID, "type", event.Type)
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
			deliveryText = job.Key.AgentName + " task cancelled."
		} else if errors.Is(timeoutContext.Err(), context.DeadlineExceeded) {
			conversation.State = store.StateFailed
			conversation.LastError = "task timeout"
			deliveryText = job.Key.AgentName + " task exceeded the configured timeout."
		} else {
			conversation.State = store.StateFailed
			conversation.LastError = safeError(err)
			s.log.Error(
				"agent task failed",
				"agent", job.Key.AgentName,
				"chat_id", job.Key.Address,
				"project", job.Key.ProjectID,
				"job_id", job.ID,
				"error_type", fmt.Sprintf("%T", err),
			)
			deliveryText = job.Key.AgentName + " failed: " + safeError(err)
		}
	} else {
		conversation.State = store.StateIdle
		conversation.LastError = ""
		conversation.LastResponse = result.FinalMessage
		deliveryText = result.FinalMessage
	}
	persistedJob, _ := s.store.Job(job.ID)
	messages := s.makeOutboxMessages(
		fmt.Sprintf("job:%d", job.ID),
		job.Key.Address,
		deliveryText,
		deliveryEdit{MessageID: persistedJob.StatusMessageID},
	)
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
		Key:         session.Key{Address: job.Address(), ProjectID: job.ProjectID, AgentName: job.AgentName},
		ProjectPath: job.ProjectPath,
		AgentName:   job.AgentName,
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
		Transport:      job.Key.Address.Transport,
		ConversationID: job.Key.Address.ConversationID,
		ProjectID:      job.Key.ProjectID,
		ProjectPath:    job.ProjectPath,
		AgentName:      job.Key.AgentName,
		State:          store.StateFailed,
		CreatedAt:      now,
		LastActivity:   now,
		LastError:      text,
	}
	persistedJob, _ := s.store.Job(job.ID)
	messages := s.makeOutboxMessages(
		fmt.Sprintf("job:%d", job.ID),
		job.Key.Address,
		text,
		deliveryEdit{MessageID: persistedJob.StatusMessageID},
	)
	if err := s.store.CompleteJob(job.ID, conversation, messages); err != nil {
		s.log.Error("complete failed job", "job_id", job.ID, "error_type", fmt.Sprintf("%T", err))
	}
	s.markDispatched(job.ID, false)
	s.wakeOutbox()
}

func (s *Service) hasPersistedJobs(address transport.Address, projectID, agentName string) bool {
	for _, job := range s.store.Jobs() {
		if job.Address() == address && job.ProjectID == projectID && job.AgentName == agentName {
			return true
		}
	}
	return false
}
