package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecRunnerStartsAndResumesThread(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("GO_WANT_CODEX_HELPER", "1")
	t.Setenv("CODEX_HELPER_ARGS", argsFile)
	runner := &ExecRunner{
		Command:    os.Args[0],
		Args:       []string{"-test.run=TestCodexHelperProcess", "--"},
		FullAccess: true,
		TempDir:    t.TempDir(),
	}
	version, err := runner.Validate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "codex-cli test" {
		t.Fatalf("version = %q", version)
	}

	var capturedThread string
	first, err := runner.Run(context.Background(), Request{
		ProjectPath: t.TempDir(),
		Prompt:      "first prompt",
		OnThread: func(threadID string) error {
			capturedThread = threadID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if capturedThread != "thread-123" || first.ThreadID != "thread-123" {
		t.Fatalf("thread IDs = %q, %q", capturedThread, first.ThreadID)
	}
	if first.FinalMessage != "final from helper" {
		t.Fatalf("final message = %q", first.FinalMessage)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "exec") || !strings.Contains(string(args), "-C") {
		t.Fatalf("new-session args = %s", args)
	}

	second, err := runner.Run(context.Background(), Request{
		ProjectPath: t.TempDir(),
		ThreadID:    first.ThreadID,
		Prompt:      "second prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ThreadID != "thread-123" {
		t.Fatalf("resumed thread = %q", second.ThreadID)
	}
	args, err = os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "resume") || !strings.Contains(string(args), "thread-123") {
		t.Fatalf("resume args = %s", args)
	}
	if err := runner.Archive(context.Background(), "thread-123"); err != nil {
		t.Fatal(err)
	}
}

func TestExecRunnerRemovesSensitiveEnvironmentVariables(t *testing.T) {
	t.Setenv("AGENT_RELAY_SECRET", "do-not-inherit")
	runner := &ExecRunner{RemoveEnv: []string{"AGENT_RELAY_SECRET"}}
	for _, entry := range runner.environment() {
		if strings.HasPrefix(entry, "AGENT_RELAY_SECRET=") {
			t.Fatalf("sensitive environment variable was inherited: %s", entry)
		}
	}
}

func TestCodexHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_HELPER") != "1" {
		return
	}
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		os.Exit(2)
	}
	args := os.Args[separator+1:]
	if err := os.WriteFile(os.Getenv("CODEX_HELPER_ARGS"), []byte(strings.Join(args, " ")), 0o600); err != nil {
		os.Exit(2)
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli test")
		os.Exit(0)
	}
	if len(args) >= 3 && args[0] == "exec" && args[1] == "resume" && args[2] == "--help" {
		fmt.Println("SESSION_ID --json --output-last-message --dangerously-bypass-approvals-and-sandbox")
		os.Exit(0)
	}
	if len(args) == 2 && args[0] == "archive" {
		os.Exit(0)
	}
	outputPath := ""
	for index, arg := range args {
		if arg == "--output-last-message" && index+1 < len(args) {
			outputPath = args[index+1]
			break
		}
	}
	if outputPath == "" {
		os.Exit(2)
	}
	if err := os.WriteFile(outputPath, []byte("final from helper\n"), 0o600); err != nil {
		os.Exit(2)
	}
	fmt.Println(`{"type":"thread.started","thread_id":"thread-123"}`)
	fmt.Println(`{"type":"turn.completed"}`)
	os.Exit(0)
}
