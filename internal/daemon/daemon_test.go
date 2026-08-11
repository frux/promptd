package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
