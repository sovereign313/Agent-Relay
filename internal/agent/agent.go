package agent

import (
	"bytes"
	"context"
	"crypto/rand"
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
	Validate(context.Context) (string, error)
	Run(context.Context, Request) (Result, error)
}

type Resetter interface {
	Reset(context.Context, string) error
}

type Command struct {
	Path      string
	Args      []string
	Dir       string
	Stdin     io.Reader
	RemoveEnv []string
}

func RunCommand(ctx context.Context, spec Command, consume func(io.Reader) error) (string, error) {
	runContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	command := exec.CommandContext(runContext, spec.Path, spec.Args...)
	command.Dir = spec.Dir
	command.Env = Environment(spec.RemoveEnv)
	command.Stdin = spec.Stdin
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
		return "", fmt.Errorf("capture agent output: %w", err)
	}
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("start agent: %w", err)
	}

	parseErr := consume(stdout)
	if parseErr != nil {
		cancelRun()
	}
	waitErr := command.Wait()
	close(processDone)
	if parseErr != nil {
		return stderr.String(), parseErr
	}
	if waitErr != nil {
		return stderr.String(), waitErr
	}
	return stderr.String(), nil
}

func Environment(remove []string) []string {
	if len(remove) == 0 {
		return os.Environ()
	}
	removed := make(map[string]bool, len(remove))
	for _, key := range remove {
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

func Version(ctx context.Context, command string, args, removeEnv []string) (string, error) {
	versionCommand := exec.CommandContext(ctx, command, args...)
	versionCommand.Env = Environment(removeEnv)
	output, err := versionCommand.CombinedOutput()
	if err != nil {
		return "", err
	}
	return LastNonEmptyLine(string(output)), nil
}

func Help(ctx context.Context, command string, args, removeEnv []string) (string, error) {
	helpCommand := exec.CommandContext(ctx, command, args...)
	helpCommand.Env = Environment(removeEnv)
	output, err := helpCommand.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func LastNonEmptyLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if value := strings.TrimSpace(lines[index]); value != "" {
			return value
		}
	}
	return "unknown"
}

func NewSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		value[0:4],
		value[4:6],
		value[6:8],
		value[8:10],
		value[10:16],
	), nil
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
