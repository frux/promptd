package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frux/promptd/internal/control"
	"github.com/frux/promptd/internal/store"
)

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "promptd dev") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
jobs:
  heartbeat:
    schedule:
      every: 1h
    agent:
      type: command
      command: ["true"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--config", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "config valid: 1 job(s)\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nope"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("Run() code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "nope"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSubcommandHelpExitsSuccessfully(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "Usage of validate") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestStatusShowsAllRegisteredJobs(t *testing.T) {
	path := seedStatusStore(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", "--state", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	output := stdout.String()
	for _, expected := range []string{"JOB", "alpha", "enabled", "beta", "disabled", "failed"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("status output %q does not contain %q", output, expected)
		}
	}
}

func TestStatusJSON(t *testing.T) {
	path := seedStatusStore(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", "--state", path, "--format", "json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	var rows []control.JobStatus
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode status JSON: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "alpha" || !rows[0].Enabled || rows[0].NextRun == nil {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[1].ID != "beta" || rows[1].Enabled || rows[1].NextRun != nil || rows[1].LastRun == nil || rows[1].LastRun.Status != store.RunFailed {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestStatusMissingDatabaseDoesNotCreateIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", "--state", path}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "status failed") {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("database was created: %v", err)
	}
}

func TestStatusPrefersRunningDaemon(t *testing.T) {
	offlinePath := seedStatusStore(t)
	directory := t.TempDir()
	live, err := store.Open(context.Background(), filepath.Join(directory, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := live.ReconcileJobs(context.Background(), []store.JobSpec{{
		ID: "live-job", ConfigHash: "live", ScheduleHash: "live", ConfigJSON: []byte(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}
	socketDirectory, err := os.MkdirTemp("/tmp", "promptd-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "p.sock")
	server, err := control.Start(socketPath, live, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
		if err := <-server.Done(); err != nil {
			t.Errorf("Serve() error = %v", err)
		}
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", "--state", offlinePath, "--socket", socketPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if output := stdout.String(); !strings.Contains(output, "live-job") || strings.Contains(output, "alpha") {
		t.Fatalf("status output = %q", output)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--state", offlinePath, "--socket", socketPath, "--offline"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("offline Run() code = %d, stderr = %s", code, stderr.String())
	}
	if output := stdout.String(); !strings.Contains(output, "alpha") || strings.Contains(output, "live-job") {
		t.Fatalf("offline status output = %q", output)
	}
}

func seedStatusStore(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "promptd.db")
	state, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	jobs := []store.JobSpec{
		{ID: "alpha", ConfigHash: "config-a", ScheduleHash: "schedule-a", ConfigJSON: []byte(`{"id":"alpha"}`)},
		{ID: "beta", ConfigHash: "config-b", ScheduleHash: "schedule-b", ConfigJSON: []byte(`{"id":"beta"}`)},
	}
	if err := state.ReconcileJobs(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	next := time.Date(2026, 8, 12, 4, 0, 0, 0, time.UTC)
	if err := state.SetSchedulerState(ctx, "alpha", &next, nil); err != nil {
		t.Fatal(err)
	}
	run, err := state.CreateRun(ctx, store.NewRun{JobID: "beta", Trigger: "scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	started := next.Add(-time.Hour)
	if _, err := state.StartRun(ctx, run.ID, started, "/tmp/beta.log"); err != nil {
		t.Fatal(err)
	}
	exitCode := 2
	if _, err := state.FinishRun(ctx, run.ID, started.Add(time.Minute), store.RunResult{
		Status: store.RunFailed, ExitCode: &exitCode, Error: "failed", LogPath: "/tmp/beta.log",
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.ReconcileJobs(ctx, jobs[:1]); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
