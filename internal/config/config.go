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
	defaultTimeout = 30 * time.Minute
	defaultOverlap = "skip"
	defaultMisfire = "skip"
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
	Type       string   `yaml:"type"`
	Prompt     string   `yaml:"prompt,omitempty"`
	PromptFile string   `yaml:"prompt_file,omitempty"`
	Command    []string `yaml:"command,omitempty"`
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
		hasPrompt := strings.TrimSpace(job.Agent.Prompt) != ""
		hasPromptFile := job.Agent.PromptFile != ""
		if hasPrompt == hasPromptFile {
			return fmt.Errorf("%s.agent: codex requires exactly one of prompt or prompt_file", prefix)
		}
		if len(job.Agent.Command) != 0 {
			return fmt.Errorf("%s.agent.command: not allowed for codex", prefix)
		}
	case "command":
		if len(job.Agent.Command) == 0 || strings.TrimSpace(job.Agent.Command[0]) == "" {
			return fmt.Errorf("%s.agent.command: command agent requires a non-empty command", prefix)
		}
	default:
		return fmt.Errorf("%s.agent.type: expected codex or command, got %q", prefix, job.Agent.Type)
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
