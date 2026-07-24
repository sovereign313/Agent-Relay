package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "state.json")
	stateStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.SetSelectedProject(42, "harness-studio"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	conversation := Conversation{
		ChatID:       42,
		ProjectID:    "harness-studio",
		ProjectPath:  "/projects/HarnessStudio",
		AgentName:    "codex",
		ThreadID:     "thread-123",
		State:        StateIdle,
		CreatedAt:    now,
		LastActivity: now,
	}
	if err := stateStore.PutConversation(conversation); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %o, want 600", got)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.SelectedProject(42); got != "harness-studio" {
		t.Fatalf("selected project = %q", got)
	}
	got, ok := reopened.Conversation(42, "harness-studio", "codex")
	if !ok || got.ThreadID != "thread-123" {
		t.Fatalf("conversation = %#v, %v", got, ok)
	}
}

func TestStoreReconcilesWorkingJobWithoutReplayingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stateStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := Conversation{
		ChatID:       42,
		ProjectID:    "alpha",
		ProjectPath:  "/projects/alpha",
		AgentName:    "codex",
		State:        StateQueued,
		CreatedAt:    now,
		LastActivity: now,
	}
	job := PendingJob{
		ID:          100,
		ChatID:      42,
		ProjectID:   "alpha",
		ProjectPath: "/projects/alpha",
		AgentName:   "codex",
		Prompt:      "change something",
		CreatedAt:   now,
	}
	if accepted, err := stateStore.AcceptUpdate(100, &job, &conversation); err != nil || !accepted {
		t.Fatalf("AcceptUpdate = %v, %v", accepted, err)
	}
	conversation.State = StateWorking
	if err := stateStore.StartJob(100, conversation); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := reopened.ReconcileStartup()
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted) != 1 || interrupted[0].State != JobInterrupted {
		t.Fatalf("interrupted jobs = %#v", interrupted)
	}
	got, _ := reopened.Conversation(42, "alpha", "codex")
	if got.State != StateStopped {
		t.Fatalf("conversation state = %q, want stopped", got.State)
	}
}

func TestCompleteJobAtomicallyCreatesOutboxMessage(t *testing.T) {
	stateStore, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := Conversation{ChatID: 42, ProjectID: "alpha", AgentName: "codex", CreatedAt: now}
	job := PendingJob{ID: 7, ChatID: 42, ProjectID: "alpha", AgentName: "codex", Prompt: "task", CreatedAt: now}
	if _, err := stateStore.AcceptUpdate(7, &job, &conversation); err != nil {
		t.Fatal(err)
	}
	message := OutboxMessage{ID: "job:7", ChatID: 42, Text: "done", CreatedAt: now}
	if err := stateStore.CompleteJob(7, conversation, []OutboxMessage{message}); err != nil {
		t.Fatal(err)
	}
	if _, ok := stateStore.Job(7); ok {
		t.Fatal("completed job remains in inbox")
	}
	outbox := stateStore.Outbox()
	if len(outbox) != 1 || outbox[0].Text != "done" {
		t.Fatalf("outbox = %#v", outbox)
	}
}

func TestOpenMigratesVersionTwoSessionsToCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := map[string]any{
		"version":           2,
		"selected_projects": map[string]string{"42": "alpha"},
		"conversations": map[string]any{
			"42:alpha": map[string]any{
				"chat_id": 42, "project_id": "alpha", "thread_id": "thread-old",
			},
		},
		"jobs":   map[string]any{},
		"outbox": map[string]any{},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	stateStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	conversation, ok := stateStore.Conversation(42, "alpha", "codex")
	if !ok || conversation.AgentName != "codex" || conversation.ThreadID != "thread-old" {
		t.Fatalf("migrated conversation = %#v, %v", conversation, ok)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(persisted, &result); err != nil {
		t.Fatal(err)
	}
	if result["version"] != float64(3) {
		t.Fatalf("persisted version = %#v", result["version"])
	}
}
