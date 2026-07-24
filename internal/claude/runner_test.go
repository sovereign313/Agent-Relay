package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sovereign313/Agent-Relay/internal/agent"
)

func TestRunnerStartsAndResumesSession(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("GO_WANT_CLAUDE_HELPER", "1")
	t.Setenv("CLAUDE_HELPER_ARGS", argsFile)
	runner := &Runner{
		Command:    os.Args[0],
		Args:       []string{"-test.run=TestClaudeHelperProcess", "--"},
		FullAccess: true,
	}
	if version, err := runner.Validate(context.Background()); err != nil || version != "claude test" {
		t.Fatalf("Validate = %q, %v", version, err)
	}

	var persisted string
	first, err := runner.Run(context.Background(), agent.Request{
		ProjectPath: t.TempDir(),
		Prompt:      "first",
		OnThread: func(id string) error {
			persisted = id
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if persisted == "" || first.ThreadID != persisted || first.FinalMessage != "final from Claude" {
		t.Fatalf("first result = %#v, persisted %q", first, persisted)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--session-id") || !strings.Contains(string(args), "--dangerously-skip-permissions") {
		t.Fatalf("new-session args = %s", args)
	}

	second, err := runner.Run(context.Background(), agent.Request{
		ProjectPath: t.TempDir(),
		ThreadID:    first.ThreadID,
		Prompt:      "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ThreadID != first.ThreadID {
		t.Fatalf("resumed session = %q", second.ThreadID)
	}
	args, _ = os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--resume "+first.ThreadID) {
		t.Fatalf("resume args = %s", args)
	}
}

func TestClaudeHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CLAUDE_HELPER") != "1" {
		return
	}
	args := helperArgs(os.Args)
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("claude test")
		os.Exit(0)
	}
	if len(args) == 1 && args[0] == "--help" {
		fmt.Println("--print --output-format --session-id --resume --dangerously-skip-permissions")
		os.Exit(0)
	}
	_ = os.WriteFile(os.Getenv("CLAUDE_HELPER_ARGS"), []byte(strings.Join(args, " ")), 0o600)
	sessionID := valueAfter(args, "--session-id")
	if sessionID == "" {
		sessionID = valueAfter(args, "--resume")
	}
	fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":"final from Claude","session_id":%q}`+"\n", sessionID)
	os.Exit(0)
}

func helperArgs(args []string) []string {
	for index, value := range args {
		if value == "--" {
			return args[index+1:]
		}
	}
	return nil
}

func valueAfter(args []string, target string) string {
	for index, value := range args {
		if value == target && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}
