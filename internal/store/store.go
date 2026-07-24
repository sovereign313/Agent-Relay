package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

type SessionState string
type JobState string

const (
	StateIdle    SessionState = "idle"
	StateQueued  SessionState = "queued"
	StateWorking SessionState = "working"
	StateStopped SessionState = "stopped"
	StateFailed  SessionState = "failed"
)

const (
	JobPending     JobState = "pending"
	JobEnqueued    JobState = "enqueued"
	JobWorking     JobState = "working"
	JobInterrupted JobState = "interrupted"
)

type Conversation struct {
	ChatID       int64        `json:"chat_id"`
	ProjectID    string       `json:"project_id"`
	ProjectPath  string       `json:"project_path"`
	ThreadID     string       `json:"thread_id,omitempty"`
	State        SessionState `json:"state"`
	CreatedAt    time.Time    `json:"created_at"`
	LastActivity time.Time    `json:"last_activity"`
	LastError    string       `json:"last_error,omitempty"`
	LastResponse string       `json:"last_response,omitempty"`
}

type PendingJob struct {
	ID          int64     `json:"id"`
	ChatID      int64     `json:"chat_id"`
	ProjectID   string    `json:"project_id"`
	ProjectPath string    `json:"project_path"`
	Prompt      string    `json:"prompt"`
	State       JobState  `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
}

type OutboxMessage struct {
	ID        string    `json:"id"`
	ChatID    int64     `json:"chat_id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	Attempts  int       `json:"attempts"`
}

type state struct {
	Version       int                      `json:"version"`
	UpdateOffset  int64                    `json:"update_offset"`
	Selected      map[string]string        `json:"selected_projects"`
	Conversations map[string]Conversation  `json:"conversations"`
	Jobs          map[string]PendingJob    `json:"jobs"`
	Outbox        map[string]OutboxMessage `json:"outbox"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	data state
}

func Open(path string) (*Store, error) {
	store := &Store{
		path: path,
		data: state{
			Version:       2,
			Selected:      make(map[string]string),
			Conversations: make(map[string]Conversation),
			Jobs:          make(map[string]PendingJob),
			Outbox:        make(map[string]OutboxMessage),
		},
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &store.data); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if store.data.Selected == nil {
		store.data.Selected = make(map[string]string)
	}
	if store.data.Conversations == nil {
		store.data.Conversations = make(map[string]Conversation)
	}
	if store.data.Jobs == nil {
		store.data.Jobs = make(map[string]PendingJob)
	}
	if store.data.Outbox == nil {
		store.data.Outbox = make(map[string]OutboxMessage)
	}
	if store.data.Version < 2 {
		store.data.Version = 2
	}
	return store, nil
}

func (s *Store) UpdateOffset() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.UpdateOffset
}

func (s *Store) SetUpdateOffset(offset int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset <= s.data.UpdateOffset {
		return nil
	}
	s.data.UpdateOffset = offset
	return s.persistLocked()
}

func (s *Store) AcceptUpdate(updateID int64, job *PendingJob, conversation *Conversation) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if updateID+1 <= s.data.UpdateOffset {
		return false, nil
	}
	if job != nil {
		job.State = JobPending
		s.data.Jobs[jobKey(job.ID)] = *job
	}
	if conversation != nil {
		s.data.Conversations[conversationKey(conversation.ChatID, conversation.ProjectID)] = *conversation
	}
	s.data.UpdateOffset = updateID + 1
	return true, s.persistLocked()
}

func (s *Store) SelectedProject(chatID int64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Selected[strconv.FormatInt(chatID, 10)]
}

func (s *Store) SetSelectedProject(chatID int64, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Selected[strconv.FormatInt(chatID, 10)] = projectID
	return s.persistLocked()
}

func (s *Store) Conversation(chatID int64, projectID string) (Conversation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	conversation, ok := s.data.Conversations[conversationKey(chatID, projectID)]
	return conversation, ok
}

func (s *Store) PutConversation(conversation Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Conversations[conversationKey(conversation.ChatID, conversation.ProjectID)] = conversation
	return s.persistLocked()
}

func (s *Store) DeleteConversation(chatID int64, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Conversations, conversationKey(chatID, projectID))
	return s.persistLocked()
}

func (s *Store) Conversations(chatID int64) []Conversation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Conversation, 0)
	for _, conversation := range s.data.Conversations {
		if conversation.ChatID == chatID {
			result = append(result, conversation)
		}
	}
	return result
}

func (s *Store) Jobs() []PendingJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]PendingJob, 0, len(s.data.Jobs))
	for _, job := range s.data.Jobs {
		result = append(result, job)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result
}

func (s *Store) Job(id int64) (PendingJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.data.Jobs[jobKey(id)]
	return job, ok
}

func (s *Store) SetJobState(id int64, state JobState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.data.Jobs[jobKey(id)]
	if !ok {
		return fmt.Errorf("job %d not found", id)
	}
	job.State = state
	s.data.Jobs[jobKey(id)] = job
	return s.persistLocked()
}

func (s *Store) StartJob(id int64, conversation Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.data.Jobs[jobKey(id)]
	if !ok {
		return fmt.Errorf("job %d not found", id)
	}
	job.State = JobWorking
	s.data.Jobs[jobKey(id)] = job
	s.data.Conversations[conversationKey(conversation.ChatID, conversation.ProjectID)] = conversation
	return s.persistLocked()
}

func (s *Store) RetryJob(id int64) (PendingJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.data.Jobs[jobKey(id)]
	if !ok {
		return PendingJob{}, fmt.Errorf("job %d not found", id)
	}
	if job.State != JobInterrupted {
		return PendingJob{}, fmt.Errorf("job %d is %s, not interrupted", id, job.State)
	}
	job.State = JobPending
	s.data.Jobs[jobKey(id)] = job
	return job, s.persistLocked()
}

func (s *Store) DropJob(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Jobs, jobKey(id))
	return s.persistLocked()
}

func (s *Store) CompleteJob(id int64, conversation Conversation, messages []OutboxMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Jobs, jobKey(id))
	s.data.Conversations[conversationKey(conversation.ChatID, conversation.ProjectID)] = conversation
	for _, message := range messages {
		if message.ID != "" {
			s.data.Outbox[message.ID] = message
		}
	}
	return s.persistLocked()
}

func (s *Store) ClearJobs(chatID int64, projectID string, excludeID int64) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []int64
	for key, job := range s.data.Jobs {
		if job.ChatID != chatID || job.ProjectID != projectID || job.State == JobWorking || job.ID == excludeID {
			continue
		}
		removed = append(removed, job.ID)
		delete(s.data.Jobs, key)
	}
	if len(removed) == 0 {
		return nil, nil
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })
	return removed, s.persistLocked()
}

func (s *Store) Outbox() []OutboxMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]OutboxMessage, 0, len(s.data.Outbox))
	for _, message := range s.data.Outbox {
		result = append(result, message)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

func (s *Store) PutOutbox(message OutboxMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Outbox[message.ID] = message
	return s.persistLocked()
}

func (s *Store) RemoveOutbox(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Outbox, id)
	return s.persistLocked()
}

func (s *Store) IncrementOutboxAttempts(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	message, ok := s.data.Outbox[id]
	if !ok {
		return nil
	}
	message.Attempts++
	s.data.Outbox[id] = message
	return s.persistLocked()
}

func (s *Store) ReconcileStartup() ([]PendingJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for key, conversation := range s.data.Conversations {
		if conversation.State != StateWorking && conversation.State != StateQueued {
			continue
		}
		conversation.State = StateStopped
		conversation.LastError = "agent relay restarted"
		conversation.LastActivity = time.Now().UTC()
		s.data.Conversations[key] = conversation
		changed = true
	}
	var interrupted []PendingJob
	for key, job := range s.data.Jobs {
		switch job.State {
		case JobWorking:
			job.State = JobInterrupted
			s.data.Jobs[key] = job
			interrupted = append(interrupted, job)
			changed = true
		case JobEnqueued:
			job.State = JobPending
			s.data.Jobs[key] = job
			changed = true
		}
	}
	if !changed {
		return interrupted, nil
	}
	return interrupted, s.persistLocked()
}

func (s *Store) persistLocked() error {
	parent := filepath.Dir(s.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	temp, err := os.CreateTemp(parent, ".state-*.json")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return os.Chmod(s.path, 0o600)
}

func conversationKey(chatID int64, projectID string) string {
	return strconv.FormatInt(chatID, 10) + ":" + projectID
}

func jobKey(id int64) string {
	return strconv.FormatInt(id, 10)
}
