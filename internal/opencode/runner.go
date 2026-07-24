package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/sovereign313/Agent-Relay/internal/agent"
)

type Runner struct {
	Command    string
	Args       []string
	FullAccess bool
	RemoveEnv  []string
}

func (r *Runner) Validate(ctx context.Context) (string, error) {
	version, err := agent.Version(ctx, r.Command, append(append([]string(nil), r.Args...), "--version"), r.RemoveEnv)
	if err != nil {
		return "", fmt.Errorf("run opencode --version: %w", err)
	}
	helpArgs := append([]string(nil), r.Args...)
	helpArgs = append(helpArgs, "run", "--help")
	help, err := agent.Help(ctx, r.Command, helpArgs, r.RemoveEnv)
	if err != nil {
		return "", fmt.Errorf("inspect opencode run help: %w", err)
	}
	for _, required := range []string{"--session", "--format", "json", "--dir", "--dangerously-skip-permissions"} {
		if !strings.Contains(help, required) {
			return "", fmt.Errorf("opencode run does not advertise required capability %s", required)
		}
	}
	return version, nil
}

func (r *Runner) Run(ctx context.Context, request agent.Request) (agent.Result, error) {
	if request.ProjectPath == "" {
		return agent.Result{}, errors.New("project path is required")
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return agent.Result{}, errors.New("prompt is required")
	}

	args := append([]string(nil), r.Args...)
	args = append(args, "run", "--format", "json", "--dir", request.ProjectPath)
	if r.FullAccess {
		args = append(args, "--dangerously-skip-permissions")
	}
	if request.ThreadID != "" {
		args = append(args, "--session", request.ThreadID)
	}
	args = append(args, "--", request.Prompt)

	threadID := request.ThreadID
	var final strings.Builder
	stderr, err := agent.RunCommand(ctx, agent.Command{
		Path:      r.Command,
		Args:      args,
		Dir:       request.ProjectPath,
		RemoveEnv: r.RemoveEnv,
	}, func(output io.Reader) error {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			var event wireEvent
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				return fmt.Errorf("decode OpenCode event: %w", err)
			}
			if event.SessionID != "" && threadID == "" {
				threadID = event.SessionID
				if request.OnThread != nil {
					if err := request.OnThread(threadID); err != nil {
						return fmt.Errorf("persist OpenCode session: %w", err)
					}
				}
			}
			if event.Type == "step_start" {
				final.Reset()
			}
			if event.Type == "text" && event.Part.Type == "text" {
				final.WriteString(event.Part.Text)
			}
			if request.OnEvent != nil {
				request.OnEvent(agent.Event{Type: event.Type})
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read OpenCode events: %w", err)
		}
		return nil
	})
	if err != nil {
		return agent.Result{ThreadID: threadID}, commandError("OpenCode", err, stderr)
	}
	if threadID == "" {
		return agent.Result{}, errors.New("OpenCode did not report a session ID")
	}
	message := strings.TrimSpace(final.String())
	if message == "" {
		return agent.Result{ThreadID: threadID}, errors.New("OpenCode completed without a final message")
	}
	return agent.Result{ThreadID: threadID, FinalMessage: message}, nil
}

func (r *Runner) Reset(ctx context.Context, threadID string) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("session ID is required")
	}
	args := append([]string(nil), r.Args...)
	args = append(args, "session", "delete", threadID)
	command := exec.CommandContext(ctx, r.Command, args...)
	command.Env = agent.Environment(r.RemoveEnv)
	output, err := command.CombinedOutput()
	if err != nil {
		return commandError("delete OpenCode session", err, string(output))
	}
	return nil
}

type wireEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
}

func commandError(name string, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if detail != "" {
		return fmt.Errorf("%s failed: %w: %s", name, err, detail)
	}
	return fmt.Errorf("%s failed: %w", name, err)
}
