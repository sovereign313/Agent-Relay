package relay

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sovereign313/Agent-Relay/internal/agent"
	"github.com/sovereign313/Agent-Relay/internal/config"
	"github.com/sovereign313/Agent-Relay/internal/project"
	"github.com/sovereign313/Agent-Relay/internal/store"
	"github.com/sovereign313/Agent-Relay/internal/telegram"
	"github.com/sovereign313/Agent-Relay/internal/transport"
)

var testAddress = transport.Address{Transport: transport.Telegram, ConversationID: "42"}

func TestServiceKeepsSeparateDurableProjectThread(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "Alpha")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.toml")
	configData := `telegram_token = "test-token"
allowed_user_ids = [42]
project_roots = ["` + root + `"]
queue_size = 2
task_timeout = "1m"
state_file = "./state.json"
`
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := project.Discover([]string{root}, nil, cfg.ProjectDiscoveryDepth)
	if err != nil {
		t.Fatal(err)
	}
	stateStore, err := store.Open(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	fakeTelegram := &recordingTelegram{}
	fakeRunner := &recordingRunner{called: make(chan struct{}, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := New(
		ctx,
		cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		fakeTelegram,
		stateStore,
		catalog,
		map[string]agent.Runner{"codex": fakeRunner},
		"test",
		map[string]string{"codex": "codex-test"},
	)
	defer service.sessions.Close()

	if err := service.handleUpdate(ctx, textUpdate(1, "/project alpha")); err != nil {
		t.Fatal(err)
	}
	if err := service.handleUpdate(ctx, textUpdate(2, "first task")); err != nil {
		t.Fatal(err)
	}
	waitCall(t, fakeRunner.called)
	waitForState(t, stateStore, 42, "alpha", store.StateIdle)

	if err := service.handleUpdate(ctx, textUpdate(3, "second task")); err != nil {
		t.Fatal(err)
	}
	waitCall(t, fakeRunner.called)
	waitForState(t, stateStore, 42, "alpha", store.StateIdle)

	if err := service.handleUpdate(ctx, textUpdate(3, "duplicate delivery")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	requests := fakeRunner.Requests()
	if len(requests) != 2 {
		t.Fatalf("runner received %d requests after duplicate update", len(requests))
	}
	if requests[0].ThreadID != "" {
		t.Fatalf("first request thread = %q", requests[0].ThreadID)
	}
	if requests[1].ThreadID != "thread-alpha" {
		t.Fatalf("second request thread = %q, want thread-alpha", requests[1].ThreadID)
	}
	if conversation, ok := stateStore.Conversation(testAddress, "alpha", "codex"); !ok || conversation.ThreadID != "thread-alpha" {
		t.Fatalf("persisted conversation = %#v, %v", conversation, ok)
	}

	conversation, _ := stateStore.Conversation(testAddress, "alpha", "codex")
	conversation.ProjectPath = "/old/project/path"
	if err := stateStore.PutConversation(conversation); err != nil {
		t.Fatal(err)
	}
	if err := service.handleUpdate(ctx, textUpdate(4, "task after path change")); err != nil {
		t.Fatal(err)
	}
	waitCall(t, fakeRunner.called)
	waitForState(t, stateStore, 42, "alpha", store.StateIdle)
	requests = fakeRunner.Requests()
	if len(requests) != 3 || requests[2].ThreadID != "" {
		t.Fatalf("path-changed request reused thread: %#v", requests)
	}

	service.deliverOutbox(ctx)
	if outbox := stateStore.Outbox(); len(outbox) != 0 {
		t.Fatalf("delivered outbox still contains %#v", outbox)
	}
}

func TestServiceKeepsAgentSessionsSeparate(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "Alpha")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.toml")
	configData := `telegram_token = "test-token"
allowed_user_ids = [42]
project_roots = ["` + root + `"]
state_file = "./state.json"

[agents.codex]
type = "codex"

[agents.claude]
type = "claude"
`
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := project.Discover([]string{root}, nil, cfg.ProjectDiscoveryDepth)
	if err != nil {
		t.Fatal(err)
	}
	stateStore, err := store.Open(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	codexRunner := &recordingRunner{called: make(chan struct{}, 2), newThread: "codex-thread"}
	claudeRunner := &recordingRunner{called: make(chan struct{}, 1), newThread: "claude-thread"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := New(
		ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), &recordingTelegram{},
		stateStore, catalog,
		map[string]agent.Runner{"codex": codexRunner, "claude": claudeRunner},
		"test", map[string]string{"codex": "test", "claude": "test"},
	)
	defer service.sessions.Close()

	updates := []string{"/project alpha", "codex one", "/agent claude", "claude one", "/agent codex", "codex two"}
	for index, message := range updates {
		if err := service.handleUpdate(ctx, textUpdate(int64(index+1), message)); err != nil {
			t.Fatal(err)
		}
		if message == "codex one" || message == "codex two" {
			waitCall(t, codexRunner.called)
			waitForState(t, stateStore, 42, "alpha", "codex", store.StateIdle)
		}
		if message == "claude one" {
			waitCall(t, claudeRunner.called)
			waitForState(t, stateStore, 42, "alpha", "claude", store.StateIdle)
		}
	}

	codexRequests := codexRunner.Requests()
	claudeRequests := claudeRunner.Requests()
	if len(codexRequests) != 2 || codexRequests[0].ThreadID != "" || codexRequests[1].ThreadID != "codex-thread" {
		t.Fatalf("Codex requests = %#v", codexRequests)
	}
	if len(claudeRequests) != 1 || claudeRequests[0].ThreadID != "" {
		t.Fatalf("Claude requests = %#v", claudeRequests)
	}
}

func TestServiceKeepsDiscordSessionAndDeduplicatesEvents(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "Alpha")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.toml")
	configData := `telegram_token = "test-token"
allowed_user_ids = [42]
project_roots = ["` + root + `"]
state_file = "./state.json"
`
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := project.Discover([]string{root}, nil, cfg.ProjectDiscoveryDepth)
	if err != nil {
		t.Fatal(err)
	}
	stateStore, err := store.Open(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{called: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := New(
		ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), &recordingTelegram{},
		stateStore, catalog, map[string]agent.Runner{"codex": runner},
		"test", map[string]string{"codex": "test"},
	)
	defer service.sessions.Close()
	discordSender := &recordingSender{name: transport.Discord, maxLength: 2000}
	service.senders[transport.Discord] = discordSender
	address := transport.Address{Transport: transport.Discord, ConversationID: "dm-123"}

	if err := service.handleInbound(ctx, transport.Inbound{
		EventID: "discord:1", Sequence: -1, Address: address, Text: "/project alpha",
	}); err != nil {
		t.Fatal(err)
	}
	task := transport.Inbound{EventID: "discord:2", Sequence: -2, Address: address, Text: "change it"}
	if err := service.handleInbound(ctx, task); err != nil {
		t.Fatal(err)
	}
	waitCall(t, runner.called)
	waitForAddressState(t, stateStore, address, "alpha", "codex", store.StateIdle)
	if err := service.handleInbound(ctx, task); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if requests := runner.Requests(); len(requests) != 1 {
		t.Fatalf("runner received %d Discord requests after duplicate event", len(requests))
	}
	service.deliverOutbox(ctx)
	if outbox := stateStore.Outbox(); len(outbox) != 0 {
		t.Fatalf("delivered Discord outbox still contains %#v", outbox)
	}
}

type recordingTelegram struct {
	mu       sync.Mutex
	messages []string
}

type recordingSender struct {
	recordingTelegram
	name      string
	maxLength int
}

func (s *recordingSender) Name() string {
	return s.name
}

func (s *recordingSender) MaxMessageLength() int {
	return s.maxLength
}

func (t *recordingTelegram) GetUpdates(context.Context, int64) ([]telegram.Update, error) {
	return nil, nil
}

func (t *recordingTelegram) Name() string {
	return transport.Telegram
}

func (t *recordingTelegram) MaxMessageLength() int {
	return telegram.MaxMessageLength
}

func (t *recordingTelegram) Send(_ context.Context, _ string, message string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.messages = append(t.messages, message)
	return nil
}

func (t *recordingTelegram) SendKeyboard(_ context.Context, _ string, message string, _ [][]transport.Button) error {
	return t.Send(context.Background(), "0", message)
}

func (t *recordingTelegram) AnswerAction(context.Context, string, string) error {
	return nil
}

type recordingRunner struct {
	mu        sync.Mutex
	requests  []agent.Request
	called    chan struct{}
	newThread string
}

func (r *recordingRunner) Validate(context.Context) (string, error) {
	return "test", nil
}

func (r *recordingRunner) Run(_ context.Context, request agent.Request) (agent.Result, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	threadID := request.ThreadID
	if threadID == "" {
		threadID = r.newThread
		if threadID == "" {
			threadID = "thread-alpha"
		}
		if request.OnThread != nil {
			if err := request.OnThread(threadID); err != nil {
				return agent.Result{}, err
			}
		}
	}
	r.called <- struct{}{}
	return agent.Result{ThreadID: threadID, FinalMessage: "done"}, nil
}

func (r *recordingRunner) Requests() []agent.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agent.Request(nil), r.requests...)
}

func textUpdate(id int64, text string) telegram.Update {
	return telegram.Update{
		UpdateID: id,
		Message: &telegram.Message{
			From: telegram.User{ID: 42},
			Chat: telegram.Chat{ID: 42, Type: "private"},
			Text: text,
		},
	}
}

func waitCall(t *testing.T, called <-chan struct{}) {
	t.Helper()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Codex runner")
	}
}

func waitForState(t *testing.T, stateStore *store.Store, chatID int64, projectID string, arguments ...any) {
	t.Helper()
	agentName := "codex"
	var want store.SessionState
	switch len(arguments) {
	case 1:
		want = arguments[0].(store.SessionState)
	case 2:
		agentName = arguments[0].(string)
		want = arguments[1].(store.SessionState)
	default:
		t.Fatal("waitForState requires state or agent and state")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		address := transport.Address{Transport: transport.Telegram, ConversationID: strconv.FormatInt(chatID, 10)}
		conversation, ok := stateStore.Conversation(address, projectID, agentName)
		if ok && conversation.State == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	address := transport.Address{Transport: transport.Telegram, ConversationID: strconv.FormatInt(chatID, 10)}
	conversation, _ := stateStore.Conversation(address, projectID, agentName)
	t.Fatalf("conversation state = %q, want %q", conversation.State, want)
}

func waitForAddressState(t *testing.T, stateStore *store.Store, address transport.Address, projectID, agentName string, want store.SessionState) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conversation, ok := stateStore.Conversation(address, projectID, agentName)
		if ok && conversation.State == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	conversation, _ := stateStore.Conversation(address, projectID, agentName)
	t.Fatalf("conversation state = %q, want %q", conversation.State, want)
}

func TestMakeOutboxMessagesPersistsChunksIndependently(t *testing.T) {
	message := strings.Repeat("x", telegram.MaxMessageLength+100)
	fake := &recordingTelegram{}
	service := &Service{senders: map[string]transport.Sender{transport.Telegram: fake}}
	parts := service.makeOutboxMessages("job:1", testAddress, message)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	if parts[0].ID == parts[1].ID {
		t.Fatal("outbox chunk IDs are not unique")
	}
	for _, part := range parts {
		if len([]rune(part.Text)) > telegram.MaxMessageLength {
			t.Fatalf("part exceeds Telegram limit: %d", len([]rune(part.Text)))
		}
	}
}
