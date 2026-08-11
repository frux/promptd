package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/store"
)

func TestJobSpecsAreSortedAndStable(t *testing.T) {
	cfg := &config.Config{
		Version: config.CurrentVersion,
		Jobs: map[string]config.Job{
			"zeta":  {Agent: config.Agent{Type: "command", Command: []string{"true"}}},
			"alpha": {Agent: config.Agent{Type: "codex", Prompt: "Hello"}},
		},
	}

	first, err := jobSpecs(cfg)
	if err != nil {
		t.Fatalf("jobSpecs() error = %v", err)
	}
	second, err := jobSpecs(cfg)
	if err != nil {
		t.Fatalf("second jobSpecs() error = %v", err)
	}
	if len(first) != 2 || first[0].ID != "alpha" || first[1].ID != "zeta" {
		t.Fatalf("job specs = %#v, want alpha then zeta", first)
	}
	for index := range first {
		if first[index].ConfigHash != second[index].ConfigHash {
			t.Fatalf("hash changed for %q", first[index].ID)
		}
		if len(first[index].ConfigHash) != 64 {
			t.Fatalf("hash length = %d, want 64", len(first[index].ConfigHash))
		}
		if len(first[index].ScheduleHash) != 64 {
			t.Fatalf("schedule hash length = %d, want 64", len(first[index].ScheduleHash))
		}
	}
}

func TestReconcileConfigPersistsConfiguredJobs(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	statePath := filepath.Join(dir, "state", "promptd.db")
	contents := []byte(`version: 1
jobs:
  heartbeat:
    schedule:
      every: 1h
    agent:
      type: command
      command: ["true"]
`)
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	state, err := store.Open(context.Background(), statePath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer state.Close()
	if err := reconcileConfig(context.Background(), state, cfg); err != nil {
		t.Fatalf("reconcileConfig() error = %v", err)
	}
	jobs, err := state.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "heartbeat" || !jobs[0].Enabled {
		t.Fatalf("jobs = %#v", jobs)
	}
}

func TestDaemonRunsScheduledJob(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	t.Setenv("PROMPTD_DAEMON_HELPER", "1")
	t.Setenv("PROMPTD_DAEMON_MARKER", marker)
	command, err := json.Marshal([]string{os.Args[0], "-test.run=TestDaemonHelperProcess"})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	contents := fmt.Sprintf(`version: 1
jobs:
  heartbeat:
    schedule:
      every: 100ms
    agent:
      type: command
      command: %s
`, command)
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state", "promptd.db")
	logDir := filepath.Join(dir, "logs")
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { finished <- Run(ctx, configPath, statePath, logDir, logger) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("scheduled command did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		logs, _ := filepath.Glob(filepath.Join(logDir, "heartbeat", "*.log"))
		completed := false
		for _, path := range logs {
			contents, _ := os.ReadFile(path)
			if strings.Contains(string(contents), "status=succeeded") {
				completed = true
				break
			}
		}
		if completed {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("scheduled command did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	state, err := store.Open(context.Background(), statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	runs, err := state.ListRuns(context.Background(), store.RunFilter{JobID: "heartbeat"})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 || countStatus(runs, store.RunSucceeded) == 0 {
		t.Fatalf("runs = %#v", runs)
	}
}

func TestDaemonHelperProcess(t *testing.T) {
	if os.Getenv("PROMPTD_DAEMON_HELPER") != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv("PROMPTD_DAEMON_MARKER"), []byte("done"), 0o600); err != nil {
		os.Exit(1)
	}
	_, _ = os.Stdout.WriteString("scheduled helper ran\n")
	os.Exit(0)
}
