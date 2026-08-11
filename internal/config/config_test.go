package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadValidConfigAndDefaults(t *testing.T) {
	path := writeConfig(t, `
version: 1
jobs:
  report:
    schedule:
      cron: "0 9 * * 1-5"
      timezone: Asia/Yekaterinburg
    agent:
      type: codex
      prompt: Prepare a report.
    run:
      working_directory: .
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	job := cfg.Jobs["report"]
	if job.Run.Timeout.Value() != 30*time.Minute {
		t.Fatalf("timeout = %s, want 30m", job.Run.Timeout)
	}
	if job.Run.Overlap != "skip" {
		t.Fatalf("overlap = %q, want skip", job.Run.Overlap)
	}
	if job.Run.Misfire != "skip" {
		t.Fatalf("misfire = %q, want skip", job.Run.Misfire)
	}
	if !filepath.IsAbs(job.Run.WorkingDirectory) {
		t.Fatalf("working directory = %q, want absolute path", job.Run.WorkingDirectory)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `
version: 1
unexpected: true
jobs: {}
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("Load() error = %v, want unknown field error", err)
	}
}

func TestLoadRejectsInvalidCron(t *testing.T) {
	path := writeConfig(t, `
version: 1
jobs:
  broken:
    schedule:
      cron: "not a schedule"
    agent:
      type: codex
      prompt: Hello.
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "jobs.broken.schedule.cron") {
		t.Fatalf("Load() error = %v, want cron validation error", err)
	}
}

func TestLoadRejectsMultipleScheduleKinds(t *testing.T) {
	path := writeConfig(t, `
version: 1
jobs:
  broken:
    schedule:
      cron: "0 * * * *"
      every: 5m
    agent:
      type: codex
      prompt: Hello.
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "set exactly one") {
		t.Fatalf("Load() error = %v, want exclusive schedule error", err)
	}
}

func TestLoadResolvesRelativeFiles(t *testing.T) {
	path := writeConfig(t, `
version: 1
jobs:
  report:
    schedule:
      every: 15m
    agent:
      type: codex
      prompt_file: prompts/report.md
    run:
      environment_file: secrets/report.env
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	base := filepath.Dir(path)
	job := cfg.Jobs["report"]
	if job.Agent.PromptFile != filepath.Join(base, "prompts", "report.md") {
		t.Fatalf("prompt file = %q", job.Agent.PromptFile)
	}
	if job.Run.EnvironmentFile != filepath.Join(base, "secrets", "report.env") {
		t.Fatalf("environment file = %q", job.Run.EnvironmentFile)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
