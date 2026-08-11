package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
)

const CurrentVersion = 1

const (
	defaultTimeout      = 30 * time.Minute
	defaultOverlap      = "skip"
	defaultMisfire      = "skip"
	defaultCodexSandbox = "read-only"
	defaultClaudeMode   = "dontAsk"
	defaultGeminiMode   = "plan"
)

var jobIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type Config struct {
	Version int            `yaml:"version"`
	Jobs    map[string]Job `yaml:"jobs"`
}

type Job struct {
	Schedule Schedule `yaml:"schedule"`
	Agent    Agent    `yaml:"agent"`
	Run      Run      `yaml:"run"`
}

type Schedule struct {
	Cron     string   `yaml:"cron,omitempty"`
	Every    Duration `yaml:"every,omitempty"`
	Timezone string   `yaml:"timezone,omitempty"`
}

type Agent struct {
	Type           string   `yaml:"type"`
	Prompt         string   `yaml:"prompt,omitempty"`
	PromptFile     string   `yaml:"prompt_file,omitempty"`
	Command        []string `yaml:"command,omitempty"`
	Model          string   `yaml:"model,omitempty"`
	Sandbox        string   `yaml:"sandbox,omitempty"`
	PermissionMode string   `yaml:"permission_mode,omitempty"`
	ApprovalMode   string   `yaml:"approval_mode,omitempty"`
	AllowedTools   []string `yaml:"allowed_tools,omitempty"`
	Bare           bool     `yaml:"bare,omitempty"`
}

type Run struct {
	WorkingDirectory string   `yaml:"working_directory,omitempty"`
	Timeout          Duration `yaml:"timeout,omitempty"`
	Overlap          string   `yaml:"overlap,omitempty"`
	Misfire          string   `yaml:"misfire,omitempty"`
	EnvironmentFile  string   `yaml:"environment_file,omitempty"`
}

// DefaultPath returns the default per-user configuration path.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(dir, "promptd", "config.yaml"), nil
}

// Load decodes, normalizes, and validates a configuration file.
func Load(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config is empty")
		}
		return nil, fmt.Errorf("decode config: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
		return nil, fmt.Errorf("config must contain exactly one YAML document")
	}

	cfg.applyDefaults()
	cfg.resolvePaths(filepath.Dir(path))
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	for id, job := range c.Jobs {
		if job.Schedule.Timezone == "" {
			job.Schedule.Timezone = "Local"
		}
		if job.Run.Timeout == 0 {
			job.Run.Timeout = Duration(defaultTimeout)
		}
		if job.Run.Overlap == "" {
			job.Run.Overlap = defaultOverlap
		}
		if job.Run.Misfire == "" {
			job.Run.Misfire = defaultMisfire
		}
		if job.Agent.Type == "codex" && job.Agent.Sandbox == "" {
			job.Agent.Sandbox = defaultCodexSandbox
		}
		if job.Agent.Type == "claude" && job.Agent.PermissionMode == "" {
			job.Agent.PermissionMode = defaultClaudeMode
		}
		if job.Agent.Type == "gemini" && job.Agent.ApprovalMode == "" {
			job.Agent.ApprovalMode = defaultGeminiMode
		}
		c.Jobs[id] = job
	}
}

func (c *Config) resolvePaths(base string) {
	for id, job := range c.Jobs {
		job.Agent.PromptFile = resolvePath(base, job.Agent.PromptFile)
		job.Run.WorkingDirectory = resolvePath(base, job.Run.WorkingDirectory)
		job.Run.EnvironmentFile = resolvePath(base, job.Run.EnvironmentFile)
		c.Jobs[id] = job
	}
}

func resolvePath(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}

// Validate verifies configuration semantics after decoding.
func (c *Config) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("version: expected %d, got %d", CurrentVersion, c.Version)
	}

	for id, job := range c.Jobs {
		if !jobIDPattern.MatchString(id) {
			return fmt.Errorf("jobs.%s: id must match %s", id, jobIDPattern.String())
		}
		if err := validateJob(id, job); err != nil {
			return err
		}
	}
	return nil
}

func validateJob(id string, job Job) error {
	prefix := "jobs." + id

	hasCron := strings.TrimSpace(job.Schedule.Cron) != ""
	hasEvery := job.Schedule.Every != 0
	if hasCron == hasEvery {
		return fmt.Errorf("%s.schedule: set exactly one of cron or every", prefix)
	}
	if hasCron {
		if _, err := cron.ParseStandard(job.Schedule.Cron); err != nil {
			return fmt.Errorf("%s.schedule.cron: %w", prefix, err)
		}
	}
	if hasEvery && job.Schedule.Every.Value() <= 0 {
		return fmt.Errorf("%s.schedule.every: must be positive", prefix)
	}
	if _, err := time.LoadLocation(job.Schedule.Timezone); err != nil {
		return fmt.Errorf("%s.schedule.timezone: %w", prefix, err)
	}

	switch job.Agent.Type {
	case "codex":
		if err := validatePromptAgent(prefix, "codex", job.Agent); err != nil {
			return err
		}
		if job.Agent.PermissionMode != "" || job.Agent.ApprovalMode != "" || len(job.Agent.AllowedTools) != 0 || job.Agent.Bare {
			return fmt.Errorf("%s.agent: permission_mode, approval_mode, allowed_tools, and bare are not allowed for codex", prefix)
		}
		if job.Agent.Model != strings.TrimSpace(job.Agent.Model) {
			return fmt.Errorf("%s.agent.model: must not have surrounding whitespace", prefix)
		}
		switch job.Agent.Sandbox {
		case "read-only", "workspace-write", "danger-full-access":
		default:
			return fmt.Errorf("%s.agent.sandbox: expected read-only, workspace-write, or danger-full-access", prefix)
		}
	case "claude":
		if err := validatePromptAgent(prefix, "claude", job.Agent); err != nil {
			return err
		}
		if job.Agent.Sandbox != "" {
			return fmt.Errorf("%s.agent.sandbox: not allowed for claude", prefix)
		}
		if job.Agent.ApprovalMode != "" {
			return fmt.Errorf("%s.agent.approval_mode: not allowed for claude", prefix)
		}
		if job.Agent.Model != strings.TrimSpace(job.Agent.Model) {
			return fmt.Errorf("%s.agent.model: must not have surrounding whitespace", prefix)
		}
		switch job.Agent.PermissionMode {
		case "dontAsk", "acceptEdits", "auto", "plan", "bypassPermissions":
		default:
			return fmt.Errorf("%s.agent.permission_mode: unsupported unattended mode %q", prefix, job.Agent.PermissionMode)
		}
		for index, tool := range job.Agent.AllowedTools {
			if strings.TrimSpace(tool) == "" || tool != strings.TrimSpace(tool) {
				return fmt.Errorf("%s.agent.allowed_tools[%d]: must be a non-empty value without surrounding whitespace", prefix, index)
			}
		}
	case "gemini":
		if err := validatePromptAgent(prefix, "gemini", job.Agent); err != nil {
			return err
		}
		if job.Agent.PermissionMode != "" || job.Agent.Bare {
			return fmt.Errorf("%s.agent: permission_mode and bare are not allowed for gemini", prefix)
		}
		if job.Agent.Model != strings.TrimSpace(job.Agent.Model) {
			return fmt.Errorf("%s.agent.model: must not have surrounding whitespace", prefix)
		}
		switch job.Agent.ApprovalMode {
		case "plan", "auto_edit", "yolo":
		default:
			return fmt.Errorf("%s.agent.approval_mode: expected plan, auto_edit, or yolo", prefix)
		}
		switch job.Agent.Sandbox {
		case "", "enabled":
		default:
			return fmt.Errorf("%s.agent.sandbox: expected enabled when set for gemini", prefix)
		}
		for index, tool := range job.Agent.AllowedTools {
			if strings.TrimSpace(tool) == "" || tool != strings.TrimSpace(tool) || strings.Contains(tool, ",") {
				return fmt.Errorf("%s.agent.allowed_tools[%d]: must be non-empty, comma-free, and have no surrounding whitespace", prefix, index)
			}
		}
	case "command":
		if len(job.Agent.Command) == 0 || strings.TrimSpace(job.Agent.Command[0]) == "" {
			return fmt.Errorf("%s.agent.command: command agent requires a non-empty command", prefix)
		}
		if strings.TrimSpace(job.Agent.Prompt) != "" || job.Agent.PromptFile != "" {
			return fmt.Errorf("%s.agent: prompt and prompt_file are not allowed for command", prefix)
		}
		if job.Agent.Model != "" || job.Agent.Sandbox != "" || job.Agent.PermissionMode != "" || job.Agent.ApprovalMode != "" || len(job.Agent.AllowedTools) != 0 || job.Agent.Bare {
			return fmt.Errorf("%s.agent: agent-specific options are not allowed for command", prefix)
		}
	default:
		return fmt.Errorf("%s.agent.type: expected codex, claude, gemini, or command, got %q", prefix, job.Agent.Type)
	}

	if job.Run.Timeout.Value() <= 0 {
		return fmt.Errorf("%s.run.timeout: must be positive", prefix)
	}
	if job.Run.Overlap != "skip" && job.Run.Overlap != "queue_one" {
		return fmt.Errorf("%s.run.overlap: expected skip or queue_one", prefix)
	}
	if job.Run.Misfire != "skip" && job.Run.Misfire != "run_once" {
		return fmt.Errorf("%s.run.misfire: expected skip or run_once", prefix)
	}

	return nil
}

func validatePromptAgent(prefix, agentType string, agent Agent) error {
	hasPrompt := strings.TrimSpace(agent.Prompt) != ""
	hasPromptFile := agent.PromptFile != ""
	if hasPrompt == hasPromptFile {
		return fmt.Errorf("%s.agent: %s requires exactly one of prompt or prompt_file", prefix, agentType)
	}
	if len(agent.Command) != 0 {
		return fmt.Errorf("%s.agent.command: not allowed for %s", prefix, agentType)
	}
	return nil
}
