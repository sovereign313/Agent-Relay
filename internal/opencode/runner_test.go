package opencode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sovereign313/Agent-Relay/internal/agent"
)

func TestRunnerReturnsLastStepAndResumesSession(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("GO_WANT_OPENCODE_HELPER", "1")
	t.Setenv("OPENCODE_HELPER_ARGS", argsFile)
	runner := &Runner{
		Command:    os.Args[0],
		Args:       []string{"-test.run=TestOpenCodeHelperProcess", "--"},
		FullAccess: true,
	}
	if version, err := runner.Validate(context.Background()); err != nil || version != "opencode test" {
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
	if persisted != "ses_test" || first.ThreadID != "ses_test" {
		t.Fatalf("session IDs = %q, %q", persisted, first.ThreadID)
	}
	if first.FinalMessage != "final from OpenCode" {
		t.Fatalf("final message = %q", first.FinalMessage)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--dangerously-skip-permissions") {
		t.Fatalf("new-session args = %s", args)
	}

	if _, err := runner.Run(context.Background(), agent.Request{
		ProjectPath: t.TempDir(),
		ThreadID:    first.ThreadID,
		Prompt:      "second",
	}); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--session ses_test") {
		t.Fatalf("resume args = %s", args)
	}
}

func TestOpenCodeHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_OPENCODE_HELPER") != "1" {
		return
	}
	args := helperArgs(os.Args)
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("opencode test")
		os.Exit(0)
	}
	if len(args) == 2 && args[0] == "run" && args[1] == "--help" {
		fmt.Println("--session --format json --dir --dangerously-skip-permissions")
		os.Exit(0)
	}
	_ = os.WriteFile(os.Getenv("OPENCODE_HELPER_ARGS"), []byte(strings.Join(args, " ")), 0o600)
	fmt.Println(`{"type":"step_start","sessionID":"ses_test","part":{"type":"step-start"}}`)
	fmt.Println(`{"type":"text","sessionID":"ses_test","part":{"type":"text","text":"working..."}}`)
	fmt.Println(`{"type":"step_start","sessionID":"ses_test","part":{"type":"step-start"}}`)
	fmt.Println(`{"type":"text","sessionID":"ses_test","part":{"type":"text","text":"final from OpenCode"}}`)
	fmt.Println(`{"type":"step_finish","sessionID":"ses_test","part":{"type":"step-finish"}}`)
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
