package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sovereign313/Agent-Relay/internal/transport"
)

var telegramAddress = transport.Address{Transport: transport.Telegram, ConversationID: "42"}

func TestStoreRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "state.json")
	stateStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.SetSelectedProject(telegramAddress, "harness-studio"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	conversation := Conversation{
		Transport:      transport.Telegram,
		ConversationID: "42",
		ProjectID:      "harness-studio",
		ProjectPath:    "/projects/HarnessStudio",
		AgentName:      "codex",
		ThreadID:       "thread-123",
		State:          StateIdle,
		CreatedAt:      now,
		LastActivity:   now,
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
	if got := reopened.SelectedProject(telegramAddress); got != "harness-studio" {
		t.Fatalf("selected project = %q", got)
	}
	got, ok := reopened.Conversation(telegramAddress, "harness-studio", "codex")
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
		Transport:      transport.Telegram,
		ConversationID: "42",
		ProjectID:      "alpha",
		ProjectPath:    "/projects/alpha",
		AgentName:      "codex",
		State:          StateQueued,
		CreatedAt:      now,
		LastActivity:   now,
	}
	job := PendingJob{
		ID:              100,
		Transport:       transport.Telegram,
		ConversationID:  "42",
		ProjectID:       "alpha",
		ProjectPath:     "/projects/alpha",
		AgentName:       "codex",
		Prompt:          "change something",
		StatusMessageID: "status-100",
		CreatedAt:       now,
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
	if interrupted[0].StatusMessageID != "status-100" {
		t.Fatalf("status message ID = %q", interrupted[0].StatusMessageID)
	}
	got, _ := reopened.Conversation(telegramAddress, "alpha", "codex")
	if got.State != StateStopped {
		t.Fatalf("conversation state = %q, want stopped", got.State)
	}
}

func TestOpenMigratesVersionFourState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	data, err := json.Marshal(map[string]any{
		"version":           4,
		"selected_projects": map[string]string{},
		"selected_agents":   map[string]string{},
		"conversations":     map[string]any{},
		"jobs":              map[string]any{},
		"outbox":            map[string]any{},
		"processed_events":  map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(persisted, &state); err != nil {
		t.Fatal(err)
	}
	if state["version"] != float64(5) {
		t.Fatalf("version = %#v, want 5", state["version"])
	}
}

func TestCompleteJobAtomicallyCreatesOutboxMessage(t *testing.T) {
	stateStore, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := Conversation{Transport: transport.Telegram, ConversationID: "42", ProjectID: "alpha", AgentName: "codex", CreatedAt: now}
	job := PendingJob{ID: 7, Transport: transport.Telegram, ConversationID: "42", ProjectID: "alpha", AgentName: "codex", Prompt: "task", CreatedAt: now}
	if _, err := stateStore.AcceptUpdate(7, &job, &conversation); err != nil {
		t.Fatal(err)
	}
	message := OutboxMessage{ID: "job:7", Transport: transport.Telegram, ConversationID: "42", Text: "done", CreatedAt: now}
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

func TestStoreSeparatesTransportsAndDeduplicatesEvents(t *testing.T) {
	stateStore, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	discordAddress := transport.Address{Transport: transport.Discord, ConversationID: "42"}
	if err := stateStore.SetSelectedProject(telegramAddress, "telegram-project"); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.SetSelectedProject(discordAddress, "discord-project"); err != nil {
		t.Fatal(err)
	}
	if got := stateStore.SelectedProject(telegramAddress); got != "telegram-project" {
		t.Fatalf("Telegram project = %q", got)
	}
	if got := stateStore.SelectedProject(discordAddress); got != "discord-project" {
		t.Fatalf("Discord project = %q", got)
	}
	if accepted, err := stateStore.AcceptEvent("discord:123", nil, nil); err != nil || !accepted {
		t.Fatalf("first AcceptEvent = %v, %v", accepted, err)
	}
	if accepted, err := stateStore.AcceptEvent("discord:123", nil, nil); err != nil || accepted {
		t.Fatalf("duplicate AcceptEvent = %v, %v", accepted, err)
	}
}
