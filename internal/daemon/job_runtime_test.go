package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/scheduler"
	"github.com/frux/promptd/internal/store"
	"github.com/frux/promptd/internal/supervisor"
)

func TestJobRuntimeRecordsSuccessfulRunAndLog(t *testing.T) {
	state, runtime, fake := newTestRuntime(t, "skip", false)
	event := scheduler.Event{JobID: "report", ScheduledAt: time.Now().UTC()}
	runtime.Trigger(event)

	runs := waitForRuns(t, state, func(runs []store.Run) bool {
		return len(runs) == 1 && runs[0].Status == store.RunSucceeded
	})
	run := runs[0]
	if run.ExitCode == nil || *run.ExitCode != 0 || run.LogPath == "" {
		t.Fatalf("run = %#v", run)
	}
	contents, err := os.ReadFile(run.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "runner output") || !strings.Contains(string(contents), "status=succeeded") {
		t.Fatalf("log = %q", contents)
	}
	info, err := os.Stat(run.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("log mode = %o, want 600", got)
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("runner calls = %d, want 1", fake.calls.Load())
	}
}

func TestJobRuntimeSkipsOverlappingRun(t *testing.T) {
	state, runtime, fake := newTestRuntime(t, "skip", true)
	first := scheduler.Event{JobID: "report", ScheduledAt: time.Now().UTC()}
	runtime.Trigger(first)
	waitForStart(t, fake)
	runtime.Trigger(scheduler.Event{JobID: "report", ScheduledAt: first.ScheduledAt.Add(time.Minute)})

	waitForRuns(t, state, func(runs []store.Run) bool {
		return countStatus(runs, store.RunRunning) == 1 && countStatus(runs, store.RunSkipped) == 1
	})
	close(fake.release)
	runs := waitForRuns(t, state, func(runs []store.Run) bool {
		return countStatus(runs, store.RunSucceeded) == 1 && countStatus(runs, store.RunSkipped) == 1
	})
	if len(runs) != 2 || fake.calls.Load() != 1 {
		t.Fatalf("runs = %#v, calls = %d", runs, fake.calls.Load())
	}
}

func TestJobRuntimeQueuesOnlyOneOverlappingRun(t *testing.T) {
	state, runtime, fake := newTestRuntime(t, "queue_one", true)
	first := scheduler.Event{JobID: "report", ScheduledAt: time.Now().UTC()}
	runtime.Trigger(first)
	waitForStart(t, fake)
	runtime.Trigger(scheduler.Event{JobID: "report", ScheduledAt: first.ScheduledAt.Add(time.Minute)})
	runtime.Trigger(scheduler.Event{JobID: "report", ScheduledAt: first.ScheduledAt.Add(2 * time.Minute)})
	close(fake.release)

	runs := waitForRuns(t, state, func(runs []store.Run) bool {
		return countStatus(runs, store.RunSucceeded) == 2 && countStatus(runs, store.RunSkipped) == 1
	})
	if len(runs) != 3 || fake.calls.Load() != 2 {
		t.Fatalf("runs = %#v, calls = %d", runs, fake.calls.Load())
	}
}

func TestJobRuntimePersistsCancellationDuringShutdown(t *testing.T) {
	state, runtime, fake := newTestRuntime(t, "skip", true)
	runtime.Trigger(scheduler.Event{JobID: "report", ScheduledAt: time.Now().UTC()})
	waitForStart(t, fake)
	runtime.Shutdown()

	runs, err := state.ListRuns(context.Background(), store.RunFilter{JobID: "report"})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != store.RunCanceled {
		t.Fatalf("runs = %#v, want one canceled run", runs)
	}
}

func TestClassifyResult(t *testing.T) {
	tests := []struct {
		result supervisor.Result
		err    error
		want   store.RunStatus
	}{
		{result: supervisor.Result{ExitCode: 0}, want: store.RunSucceeded},
		{result: supervisor.Result{ExitCode: 2}, want: store.RunFailed},
		{result: supervisor.Result{ExitCode: -1, TimedOut: true}, want: store.RunTimedOut},
		{result: supervisor.Result{ExitCode: -1, Canceled: true}, want: store.RunCanceled},
	}
	for _, test := range tests {
		got, _ := classifyResult(test.result, test.err)
		if got != test.want {
			t.Fatalf("classifyResult(%#v) = %q, want %q", test.result, got, test.want)
		}
	}
}

type fakeRuntimeRunner struct {
	block   bool
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (r *fakeRuntimeRunner) Run(ctx context.Context, _ config.Job, output io.Writer) (supervisor.Result, error) {
	r.calls.Add(1)
	_, _ = io.WriteString(output, "runner output\n")
	r.started <- struct{}{}
	if r.block {
		select {
		case <-r.release:
		case <-ctx.Done():
			return supervisor.Result{ExitCode: -1, Canceled: true, FinishedAt: time.Now().UTC()}, nil
		}
	}
	return supervisor.Result{ExitCode: 0, FinishedAt: time.Now().UTC()}, nil
}

func newTestRuntime(t *testing.T, overlap string, block bool) (*store.Store, *jobRuntime, *fakeRuntimeRunner) {
	t.Helper()
	state, err := store.Open(context.Background(), t.TempDir()+"/promptd.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	if err := state.ReconcileJobs(context.Background(), []store.JobSpec{{
		ID:           "report",
		ConfigHash:   "config-report",
		ScheduleHash: "schedule-report",
		ConfigJSON:   []byte(`{"id":"report"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRuntimeRunner{
		block:   block,
		started: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
	job := config.Job{
		Agent: config.Agent{Type: "command", Command: []string{"true"}},
		Run: config.Run{
			Timeout: config.Duration(time.Minute),
			Overlap: overlap,
			Misfire: "skip",
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime, err := newJobRuntime(context.Background(), state, fake, t.TempDir()+"/logs", logger, map[string]config.Job{"report": job})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Shutdown)
	return state, runtime, fake
}

func waitForStart(t *testing.T, fake *fakeRuntimeRunner) {
	t.Helper()
	select {
	case <-fake.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}
}

func waitForRuns(t *testing.T, state *store.Store, ready func([]store.Run) bool) []store.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := state.ListRuns(context.Background(), store.RunFilter{JobID: "report"})
		if err != nil {
			t.Fatal(err)
		}
		if ready(runs) {
			return runs
		}
		time.Sleep(10 * time.Millisecond)
	}
	runs, _ := state.ListRuns(context.Background(), store.RunFilter{JobID: "report"})
	t.Fatalf("runs did not reach expected state: %#v", runs)
	return nil
}

func countStatus(runs []store.Run, status store.RunStatus) int {
	count := 0
	for _, run := range runs {
		if run.Status == status {
			count++
		}
	}
	return count
}
