package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const DefaultGracePeriod = 10 * time.Second

type Spec struct {
	Command     []string
	Directory   string
	Environment []string
	Stdin       io.Reader
	Output      io.Writer
	Timeout     time.Duration
}

type Result struct {
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	TimedOut   bool
	Canceled   bool
}

type Supervisor struct {
	gracePeriod time.Duration
}

type Option func(*Supervisor)

func WithGracePeriod(period time.Duration) Option {
	return func(supervisor *Supervisor) {
		supervisor.gracePeriod = period
	}
}

func New(options ...Option) *Supervisor {
	supervisor := &Supervisor{gracePeriod: DefaultGracePeriod}
	for _, option := range options {
		option(supervisor)
	}
	return supervisor
}

// Run executes a command directly, without an implicit shell. A non-zero exit
// code is a process result rather than an infrastructure error.
func (s *Supervisor) Run(ctx context.Context, spec Spec) (Result, error) {
	result := Result{ExitCode: -1}
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return result, fmt.Errorf("command is empty")
	}
	if spec.Timeout < 0 {
		return result, fmt.Errorf("timeout must not be negative")
	}
	if s.gracePeriod < 0 {
		return result, fmt.Errorf("grace period must not be negative")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	command := exec.Command(spec.Command[0], spec.Command[1:]...)
	command.Dir = spec.Directory
	if spec.Environment != nil {
		command.Env = spec.Environment
	}
	command.Stdin = spec.Stdin
	output := spec.Output
	if output == nil {
		output = io.Discard
	}
	command.Stdout = output
	command.Stderr = output
	configureProcessTree(command)

	if err := command.Start(); err != nil {
		return result, fmt.Errorf("start command %q: %w", spec.Command[0], err)
	}
	result.StartedAt = time.Now().UTC()

	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()

	var timeout *time.Timer
	var timeoutSignal <-chan time.Time
	if spec.Timeout > 0 {
		timeout = time.NewTimer(spec.Timeout)
		timeoutSignal = timeout.C
		defer timeout.Stop()
	}

	var waitErr, stopErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		result.Canceled = true
		waitErr, stopErr = s.stop(command, waited)
	case <-timeoutSignal:
		result.TimedOut = true
		waitErr, stopErr = s.stop(command, waited)
	}

	result.FinishedAt = time.Now().UTC()
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	return result, errors.Join(normalizeWaitError(waitErr), stopErr)
}

func (s *Supervisor) stop(command *exec.Cmd, waited <-chan error) (error, error) {
	terminateErr := terminateProcessTree(command)
	grace := time.NewTimer(s.gracePeriod)
	defer grace.Stop()

	select {
	case waitErr := <-waited:
		// The leader can exit while descendants remain alive. Always sweep the
		// process group before considering a canceled run complete.
		return waitErr, errors.Join(terminateErr, killProcessTree(command))
	case <-grace.C:
		killErr := killProcessTree(command)
		return <-waited, errors.Join(terminateErr, killErr)
	}
}

func normalizeWaitError(err error) error {
	if err == nil {
		return nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return nil
	}
	return fmt.Errorf("wait for command: %w", err)
}
