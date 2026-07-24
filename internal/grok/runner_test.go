package grok

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
	t.Setenv("GO_WANT_GROK_HELPER", "1")
	t.Setenv("GROK_HELPER_ARGS", argsFile)
	runner := &Runner{
		Command:    os.Args[0],
		Args:       []string{"-test.run=TestGrokHelperProcess", "--"},
		FullAccess: true,
	}
	if version, err := runner.Validate(context.Background()); err != nil || version != "grok test" {
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
	if persisted == "" || first.ThreadID != persisted || first.FinalMessage != "final from Grok" {
		t.Fatalf("first result = %#v, persisted %q", first, persisted)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--session-id") || !strings.Contains(string(args), "--always-approve") {
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
	if !strings.Contains(string(args), "--resume "+first.ThreadID) {
		t.Fatalf("resume args = %s", args)
	}
}

func TestGrokHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_GROK_HELPER") != "1" {
		return
	}
	args := helperArgs(os.Args)
	if len(args) == 1 && args[0] == "version" {
		fmt.Println("grok test")
		os.Exit(0)
	}
	if len(args) == 1 && args[0] == "--help" {
		fmt.Println("--session-id --resume --output-format --always-approve")
		os.Exit(0)
	}
	_ = os.WriteFile(os.Getenv("GROK_HELPER_ARGS"), []byte(strings.Join(args, " ")), 0o600)
	fmt.Println(`{"result":{"content":[{"type":"text","text":"final from Grok"}]}}`)
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
