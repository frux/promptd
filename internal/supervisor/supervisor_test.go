package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const helperEnvironment = "PROMPTD_SUPERVISOR_HELPER"

func TestRunCapturesOutput(t *testing.T) {
	output := &safeBuffer{}
	result, err := New().Run(context.Background(), helperSpec("success", output))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 || result.TimedOut || result.Canceled {
		t.Fatalf("result = %#v", result)
	}
	if got := output.String(); !strings.Contains(got, "stdout") || !strings.Contains(got, "stderr") {
		t.Fatalf("output = %q", got)
	}
	if result.StartedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) {
		t.Fatalf("timestamps = %v to %v", result.StartedAt, result.FinishedAt)
	}
}

func TestRunReturnsNonZeroExitCodeWithoutError(t *testing.T) {
	result, err := New().Run(context.Background(), helperSpec("exit-seven", nil))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", result.ExitCode)
	}
}

func TestRunPassesDirectoryEnvironmentAndStdin(t *testing.T) {
	output := &safeBuffer{}
	spec := helperSpec("inspect", output)
	spec.Directory = t.TempDir()
	spec.Environment = append(spec.Environment, "PROMPTD_TEST_VALUE=present")
	spec.Stdin = strings.NewReader("from-stdin")

	result, err := New().Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	got := output.String()
	if !strings.Contains(got, "present|from-stdin") {
		t.Fatalf("output = %q", got)
	}
}

func TestRunTimesOutAndEscalates(t *testing.T) {
	grace := 25 * time.Millisecond
	spec := helperSpec("block", nil)
	spec.Timeout = 25 * time.Millisecond
	started := time.Now()
	result, err := New(WithGracePeriod(grace)).Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.TimedOut || result.Canceled {
		t.Fatalf("result = %#v", result)
	}
	if result.ExitCode != -1 {
		t.Fatalf("exit code = %d, want -1 for signaled process", result.ExitCode)
	}
	if elapsed := time.Since(started); elapsed < spec.Timeout+grace {
		t.Fatalf("elapsed = %s, want at least timeout plus grace", elapsed)
	}
}

func TestRunRespondsToCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	output := newNotifyingBuffer()
	spec := helperSpec("block", output)
	finished := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := New(WithGracePeriod(20*time.Millisecond)).Run(ctx, spec)
		finished <- struct {
			result Result
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-output.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("helper process did not start")
	}
	cancel()

	select {
	case outcome := <-finished:
		if outcome.err != nil {
			t.Fatalf("Run() error = %v", outcome.err)
		}
		if !outcome.result.Canceled || outcome.result.TimedOut {
			t.Fatalf("result = %#v", outcome.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop canceled process")
	}
}

func TestRunValidatesInput(t *testing.T) {
	if _, err := New().Run(context.Background(), Spec{}); err == nil {
		t.Fatal("empty command error = nil")
	}
	if _, err := New().Run(context.Background(), Spec{Command: []string{"true"}, Timeout: -1}); err == nil {
		t.Fatal("negative timeout error = nil")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Run(canceled, Spec{Command: []string{"true"}}); err == nil {
		t.Fatal("canceled context error = nil")
	}
}

func TestSupervisorHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvironment) != "1" {
		return
	}
	args := helperArguments()
	if len(args) == 0 {
		os.Exit(98)
	}
	mode := args[0]
	switch mode {
	case "success":
		fmt.Fprint(os.Stdout, "stdout\n")
		fmt.Fprint(os.Stderr, "stderr\n")
	case "exit-seven":
		os.Exit(7)
	case "inspect":
		stdin, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(91)
		}
		fmt.Fprintf(os.Stdout, "%s|%s", os.Getenv("PROMPTD_TEST_VALUE"), stdin)
	case "block":
		fmt.Fprint(os.Stdout, "ready\n")
		blockIgnoringTermination()
	case "spawn-child":
		if len(args) != 2 {
			os.Exit(97)
		}
		child := exec.Command(os.Args[0], "-test.run=TestSupervisorHelperProcess", "--", "heartbeat", args[1])
		child.Env = append(os.Environ(), helperEnvironment+"=1")
		child.Stdout = io.Discard
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			os.Exit(96)
		}
		waitForHeartbeat(args[1])
		fmt.Fprint(os.Stdout, "child-ready\n")
		blockIgnoringTermination()
	case "heartbeat":
		if len(args) != 2 {
			os.Exit(95)
		}
		writeHeartbeats(args[1])
	default:
		os.Exit(99)
	}
}

func helperArguments() []string {
	for index, argument := range os.Args {
		if argument == "--" {
			return os.Args[index+1:]
		}
	}
	return nil
}

func waitForHeartbeat(path string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	os.Exit(94)
}

func writeHeartbeats(path string) {
	ignoreTermination()
	file, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(93)
	}
	defer file.Close()
	for {
		if _, err := file.WriteString("beat\n"); err != nil {
			os.Exit(92)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func helperSpec(mode string, output *safeBuffer) Spec {
	spec := Spec{
		Command:     []string{os.Args[0], "-test.run=TestSupervisorHelperProcess", "--", mode},
		Environment: append(os.Environ(), helperEnvironment+"=1"),
	}
	if output != nil {
		spec.Output = output
	}
	return spec
}

type safeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	wrote  chan struct{}
	once   sync.Once
}

func newNotifyingBuffer() *safeBuffer {
	return &safeBuffer{wrote: make(chan struct{})}
}

func (b *safeBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.wrote != nil {
		b.once.Do(func() { close(b.wrote) })
	}
	return b.buffer.Write(value)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
