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
	if job.Agent.Sandbox != "read-only" {
		t.Fatalf("sandbox = %q, want read-only", job.Agent.Sandbox)
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

func TestLoadRejectsInvalidAgentSpecificFields(t *testing.T) {
	path := writeConfig(t, `
version: 1
jobs:
  broken:
    schedule:
      every: 5m
    agent:
      type: command
      command: ["true"]
      prompt: ignored
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "not allowed for command") {
		t.Fatalf("Load() error = %v, want agent-specific field error", err)
	}
}

func TestLoadRejectsInvalidCodexSandbox(t *testing.T) {
	path := writeConfig(t, `
version: 1
jobs:
  broken:
    schedule:
      every: 5m
    agent:
      type: codex
      prompt: Hello
      sandbox: root
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "agent.sandbox") {
		t.Fatalf("Load() error = %v, want sandbox error", err)
	}
}

func TestLoadClaudeDefaults(t *testing.T) {
	path := writeConfig(t, `
version: 1
jobs:
  review:
    schedule:
      every: 1h
    agent:
      type: claude
      prompt: Review the repository.
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	agent := cfg.Jobs["review"].Agent
	if agent.PermissionMode != "dontAsk" {
		t.Fatalf("permission mode = %q, want dontAsk", agent.PermissionMode)
	}
}

func TestLoadRejectsInvalidClaudeOptions(t *testing.T) {
	path := writeConfig(t, `
version: 1
jobs:
  broken:
    schedule:
      every: 5m
    agent:
      type: claude
      prompt: Hello
      permission_mode: default
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "permission_mode") {
		t.Fatalf("Load() error = %v, want permission mode error", err)
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
