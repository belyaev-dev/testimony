package generate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const maxStderrLength = 2048

type CommandSpec struct {
	Path    string
	Args    []string
	Dir     string
	Env     []string
	Timeout time.Duration
}

type CommandResult struct {
	Path     string
	Args     []string
	Dir      string
	Timeout  time.Duration
	Duration time.Duration
	Stderr   string
}

type CommandError struct {
	Command  CommandResult
	ExitCode int
	TimedOut bool
	Cause    error
}

func (e *CommandError) Error() string {
	if e == nil {
		return "command failed: <nil>"
	}

	parts := []string{fmt.Sprintf("command %q failed", e.Command.Path)}
	if len(e.Command.Args) > 0 {
		parts = append(parts, fmt.Sprintf("args=%q", strings.Join(e.Command.Args, " ")))
	}
	if e.TimedOut {
		parts = append(parts, fmt.Sprintf("timed_out_after=%s", e.Command.Timeout))
	} else if e.ExitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit_code=%d", e.ExitCode))
	}
	if snippet := strings.TrimSpace(e.Command.Stderr); snippet != "" {
		parts = append(parts, fmt.Sprintf("stderr=%q", snippet))
	}
	if e.Cause != nil {
		parts = append(parts, fmt.Sprintf("cause=%v", e.Cause))
	}
	return strings.Join(parts, " ")
}

func (e *CommandError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type Executor interface {
	Run(ctx context.Context, spec CommandSpec) (CommandResult, error)
}

type CommandExecutor struct{}

func NewCommandExecutor() *CommandExecutor {
	return &CommandExecutor{}
}

func (e *CommandExecutor) Run(ctx context.Context, spec CommandSpec) (CommandResult, error) {
	path := strings.TrimSpace(spec.Path)
	if path == "" {
		return CommandResult{}, fmt.Errorf("run command: empty path")
	}
	if spec.Timeout <= 0 {
		return CommandResult{}, fmt.Errorf("run command %q: timeout must be greater than zero", path)
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, path, spec.Args...)
	if dir := strings.TrimSpace(spec.Dir); dir != "" {
		cmd.Dir = dir
	}
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	startedAt := time.Now()
	err := cmd.Run()
	duration := time.Since(startedAt)

	result := CommandResult{
		Path:     path,
		Args:     append([]string(nil), spec.Args...),
		Dir:      cmd.Dir,
		Timeout:  spec.Timeout,
		Duration: duration,
		Stderr:   trimStderr(stderr.String()),
	}
	if err == nil {
		return result, nil
	}

	cmdErr := &CommandError{
		Command: result,
		Cause:   err,
	}

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		cmdErr.TimedOut = true
		return result, cmdErr
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		cmdErr.ExitCode = exitErr.ExitCode()
	}

	return result, cmdErr
}

func trimStderr(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if len(trimmed) <= maxStderrLength {
		return trimmed
	}
	return strings.TrimSpace(trimmed[:maxStderrLength]) + "…"
}
