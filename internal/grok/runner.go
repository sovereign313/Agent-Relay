package grok

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
	version, err := agent.Version(ctx, r.Command, append(append([]string(nil), r.Args...), "version"), r.RemoveEnv)
	if err != nil {
		return "", fmt.Errorf("run grok version: %w", err)
	}
	help, err := agent.Help(ctx, r.Command, append(append([]string(nil), r.Args...), "--help"), r.RemoveEnv)
	if err != nil {
		return "", fmt.Errorf("inspect grok help: %w", err)
	}
	for _, required := range []string{"--session-id", "--resume", "--output-format", "--always-approve"} {
		if !strings.Contains(help, required) {
			return "", fmt.Errorf("grok does not advertise required capability %s", required)
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
				return agent.Result{}, fmt.Errorf("persist Grok session: %w", err)
			}
		}
	}

	args := append([]string(nil), r.Args...)
	args = append(args, "--no-auto-update", "--no-alt-screen", "--output-format", "json", "--cwd", request.ProjectPath)
	if r.FullAccess {
		args = append(args, "--always-approve")
	}
	if request.ThreadID == "" {
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", sessionID)
	}
	args = append(args, "--single", request.Prompt)

	var response map[string]any
	stderr, err := agent.RunCommand(ctx, agent.Command{
		Path:      r.Command,
		Args:      args,
		Dir:       request.ProjectPath,
		RemoveEnv: r.RemoveEnv,
	}, func(output io.Reader) error {
		decoder := json.NewDecoder(io.LimitReader(output, 16*1024*1024))
		if err := decoder.Decode(&response); err != nil {
			return fmt.Errorf("decode Grok response: %w", err)
		}
		return nil
	})
	if err != nil {
		return agent.Result{ThreadID: sessionID}, commandError("Grok", err, stderr)
	}
	if failed, _ := response["is_error"].(bool); failed {
		return agent.Result{ThreadID: sessionID}, errors.New(firstText(response, "error", "message", "result"))
	}
	message := strings.TrimSpace(firstText(response, "result", "response", "message", "text", "content"))
	if message == "" {
		return agent.Result{ThreadID: sessionID}, errors.New("Grok completed without a final message")
	}
	return agent.Result{ThreadID: sessionID, FinalMessage: message}, nil
}

func firstText(object map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := object[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return value
			}
		case map[string]any:
			if text := firstText(value, "text", "content", "message", "result"); text != "" {
				return text
			}
		case []any:
			var parts []string
			for _, item := range value {
				if nested, ok := item.(map[string]any); ok {
					if text := firstText(nested, "text", "content", "message"); text != "" {
						parts = append(parts, text)
					}
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "")
			}
		}
	}
	return ""
}

func commandError(name string, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if detail != "" {
		return fmt.Errorf("%s failed: %w: %s", name, err, detail)
	}
	return fmt.Errorf("%s failed: %w", name, err)
}
