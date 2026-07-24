package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxDiagnosticBytes = 64 * 1024

type Request struct {
	ProjectPath string
	ThreadID    string
	Prompt      string
	OnThread    func(string) error
	OnEvent     func(Event)
}

type Event struct {
	Type    string
	Message string
}

type Result struct {
	ThreadID     string
	FinalMessage string
}

type Runner interface {
	Run(context.Context, Request) (Result, error)
}

type ExecRunner struct {
	Command    string
	Args       []string
	FullAccess bool
	TempDir    string
	RemoveEnv  []string
}

func (r *ExecRunner) Validate(ctx context.Context) (string, error) {
	versionCommand := exec.CommandContext(ctx, r.Command, append(append([]string(nil), r.Args...), "--version")...)
	versionCommand.Env = r.environment()
	versionOutput, err := versionCommand.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run codex --version: %w", err)
	}
	version := lastNonEmptyLine(string(versionOutput))

	helpArgs := append([]string(nil), r.Args...)
	helpArgs = append(helpArgs, "exec", "resume", "--help")
	helpCommand := exec.CommandContext(ctx, r.Command, helpArgs...)
	helpCommand.Env = r.environment()
	helpOutput, err := helpCommand.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect codex exec resume: %w", err)
	}
	help := string(helpOutput)
	for _, required := range []string{"SESSION_ID", "--json", "--output-last-message", "--dangerously-bypass-approvals-and-sandbox"} {
		if !strings.Contains(help, required) {
			return "", fmt.Errorf("codex exec resume does not advertise required capability %s", required)
		}
	}
	return version, nil
}

func lastNonEmptyLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if value := strings.TrimSpace(lines[index]); value != "" {
			return value
		}
	}
	return "unknown"
}

func (r *ExecRunner) Archive(ctx context.Context, threadID string) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("thread ID is required")
	}
	args := append([]string(nil), r.Args...)
	args = append(args, "archive", threadID)
	command := exec.CommandContext(ctx, r.Command, args...)
	command.Env = r.environment()
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("archive Codex thread: %w: %s", err, detail)
		}
		return fmt.Errorf("archive Codex thread: %w", err)
	}
	return nil
}

func (r *ExecRunner) Run(ctx context.Context, request Request) (Result, error) {
	if request.ProjectPath == "" {
		return Result{}, errors.New("project path is required")
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return Result{}, errors.New("prompt is required")
	}

	output, err := os.CreateTemp(r.TempDir, "agent-relay-final-*.txt")
	if err != nil {
		return Result{}, fmt.Errorf("create final-message file: %w", err)
	}
	outputPath := output.Name()
	if err := output.Chmod(0o600); err != nil {
		output.Close()
		os.Remove(outputPath)
		return Result{}, fmt.Errorf("secure final-message file: %w", err)
	}
	if err := output.Close(); err != nil {
		os.Remove(outputPath)
		return Result{}, fmt.Errorf("close final-message file: %w", err)
	}
	defer os.Remove(outputPath)

	args := append([]string(nil), r.Args...)
	args = append(args, "exec")
	if request.ThreadID != "" {
		args = append(args, "resume")
	}
	if r.FullAccess {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	args = append(args, "--json", "--output-last-message", outputPath)
	if request.ThreadID == "" {
		args = append(args, "-C", request.ProjectPath)
	} else {
		args = append(args, request.ThreadID)
	}
	args = append(args, "-")

	runContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	command := exec.CommandContext(runContext, r.Command, args...)
	command.Dir = request.ProjectPath
	command.Env = r.environment()
	command.Stdin = strings.NewReader(request.Prompt)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	const interruptGrace = 5 * time.Second
	command.WaitDelay = interruptGrace + 2*time.Second
	processDone := make(chan struct{})
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGINT)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err == nil {
			go func(pid int) {
				timer := time.NewTimer(interruptGrace)
				defer timer.Stop()
				select {
				case <-processDone:
				case <-timer.C:
					_ = syscall.Kill(-pid, syscall.SIGKILL)
				}
			}(command.Process.Pid)
		}
		return err
	}

	stdout, err := command.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("capture codex output: %w", err)
	}
	var stderr limitedBuffer
	command.Stderr = &stderr

	if err := command.Start(); err != nil {
		return Result{}, fmt.Errorf("start codex: %w", err)
	}

	threadID := request.ThreadID
	parseErr := parseEvents(stdout, func(event wireEvent) error {
		if event.ThreadID != "" && threadID == "" {
			threadID = event.ThreadID
			if request.OnThread != nil {
				if err := request.OnThread(threadID); err != nil {
					return fmt.Errorf("persist codex thread: %w", err)
				}
			}
		}
		if request.OnEvent != nil {
			request.OnEvent(Event{Type: event.Type, Message: event.Message()})
		}
		return nil
	})
	if parseErr != nil {
		cancelRun()
	}
	waitErr := command.Wait()
	close(processDone)
	if parseErr != nil {
		return Result{ThreadID: threadID}, parseErr
	}
	if waitErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return Result{ThreadID: threadID}, fmt.Errorf("codex failed: %w: %s", waitErr, detail)
		}
		return Result{ThreadID: threadID}, fmt.Errorf("codex failed: %w", waitErr)
	}

	final, err := os.ReadFile(outputPath)
	if err != nil {
		return Result{ThreadID: threadID}, fmt.Errorf("read final Codex message: %w", err)
	}
	message := strings.TrimSpace(string(final))
	if message == "" {
		return Result{ThreadID: threadID}, errors.New("codex completed without a final message")
	}
	if threadID == "" {
		return Result{}, errors.New("codex did not report a thread ID")
	}
	return Result{ThreadID: threadID, FinalMessage: message}, nil
}

func (r *ExecRunner) environment() []string {
	if len(r.RemoveEnv) == 0 {
		return os.Environ()
	}
	removed := make(map[string]bool, len(r.RemoveEnv))
	for _, key := range r.RemoveEnv {
		removed[key] = true
	}
	environment := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !removed[key] {
			environment = append(environment, entry)
		}
	}
	return environment
}

type wireEvent struct {
	Type       string          `json:"type"`
	ThreadID   string          `json:"thread_id"`
	Item       json.RawMessage `json:"item"`
	Error      json.RawMessage `json:"error"`
	MessageRaw json.RawMessage `json:"message"`
}

func (e wireEvent) Message() string {
	for _, raw := range []json.RawMessage{e.MessageRaw, e.Error, e.Item} {
		if len(raw) == 0 {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return text
		}
		var object map[string]any
		if json.Unmarshal(raw, &object) != nil {
			continue
		}
		for _, key := range []string{"message", "text", "error"} {
			if value, ok := object[key].(string); ok {
				return value
			}
		}
	}
	return ""
}

func parseEvents(reader io.Reader, handle func(wireEvent) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event wireEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("decode Codex JSON event: %w", err)
		}
		if err := handle(event); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Codex JSON events: %w", err)
	}
	return nil
}

type limitedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := maxDiagnosticBytes - b.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			_, _ = b.buffer.Write(data[:remaining])
		} else {
			_, _ = b.buffer.Write(data)
		}
	}
	return len(data), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
