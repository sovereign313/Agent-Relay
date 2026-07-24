package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		return "", fmt.Errorf("run claude --version: %w", err)
	}
	help, err := agent.Help(ctx, r.Command, append(append([]string(nil), r.Args...), "--help"), r.RemoveEnv)
	if err != nil {
		return "", fmt.Errorf("inspect claude help: %w", err)
	}
	for _, required := range []string{"--print", "--output-format", "--session-id", "--resume", "--dangerously-skip-permissions"} {
		if !strings.Contains(help, required) {
			return "", fmt.Errorf("claude does not advertise required capability %s", required)
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

	sessionID := request.ThreadID
	if sessionID == "" {
		var err error
		sessionID, err = agent.NewSessionID()
		if err != nil {
			return agent.Result{}, err
		}
		if request.OnThread != nil {
			if err := request.OnThread(sessionID); err != nil {
				return agent.Result{}, fmt.Errorf("persist Claude session: %w", err)
			}
		}
	}

	args := append([]string(nil), r.Args...)
	args = append(args, "--print", "--output-format", "json")
	if r.FullAccess {
		args = append(args, "--dangerously-skip-permissions")
	}
	if request.ThreadID == "" {
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", sessionID)
	}

	var response wireResponse
	stderr, err := agent.RunCommand(ctx, agent.Command{
		Path:      r.Command,
		Args:      args,
		Dir:       request.ProjectPath,
		Stdin:     strings.NewReader(request.Prompt),
		RemoveEnv: r.RemoveEnv,
	}, func(output io.Reader) error {
		decoder := json.NewDecoder(io.LimitReader(output, 16*1024*1024))
		if err := decoder.Decode(&response); err != nil {
			return fmt.Errorf("decode Claude response: %w", err)
		}
		return nil
	})
	if err != nil {
		return agent.Result{ThreadID: sessionID}, commandError("Claude", err, stderr)
	}
	if response.SessionID != "" && response.SessionID != sessionID {
		return agent.Result{ThreadID: sessionID}, errors.New("Claude returned an unexpected session ID")
	}
	message := strings.TrimSpace(response.Result)
	if response.IsError {
		if message == "" {
			message = "Claude reported an unsuccessful result"
		}
		return agent.Result{ThreadID: sessionID}, errors.New(message)
	}
	if message == "" {
		return agent.Result{ThreadID: sessionID}, errors.New("Claude completed without a final message")
	}
	return agent.Result{ThreadID: sessionID, FinalMessage: message}, nil
}

type wireResponse struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
}

func commandError(name string, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if detail != "" {
		return fmt.Errorf("%s failed: %w: %s", name, err, detail)
	}
	return fmt.Errorf("%s failed: %w", name, err)
}
