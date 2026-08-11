package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/supervisor"
)

type Executor interface {
	Run(context.Context, supervisor.Spec) (supervisor.Result, error)
}

type Service struct {
	executor    Executor
	codexBinary string
}

type Option func(*Service)

func WithCodexBinary(path string) Option {
	return func(service *Service) {
		service.codexBinary = path
	}
}

func New(executor Executor, options ...Option) *Service {
	if executor == nil {
		executor = supervisor.New()
	}
	service := &Service{
		executor:    executor,
		codexBinary: "codex",
	}
	for _, option := range options {
		option(service)
	}
	return service
}

// Run translates a validated job into a direct process invocation.
func (s *Service) Run(ctx context.Context, job config.Job, output io.Writer) (supervisor.Result, error) {
	command, stdin, err := s.invocation(job.Agent)
	if err != nil {
		return supervisor.Result{ExitCode: -1}, err
	}
	environment, err := environmentForJob(job.Run.EnvironmentFile)
	if err != nil {
		return supervisor.Result{ExitCode: -1}, err
	}

	result, err := s.executor.Run(ctx, supervisor.Spec{
		Command:     command,
		Directory:   job.Run.WorkingDirectory,
		Environment: environment,
		Stdin:       stdin,
		Output:      output,
		Timeout:     job.Run.Timeout.Value(),
	})
	if err != nil {
		return result, fmt.Errorf("run %s agent: %w", job.Agent.Type, err)
	}
	return result, nil
}

func (s *Service) invocation(agent config.Agent) ([]string, io.Reader, error) {
	switch agent.Type {
	case "command":
		if len(agent.Command) == 0 || strings.TrimSpace(agent.Command[0]) == "" {
			return nil, nil, fmt.Errorf("command agent has no command")
		}
		return append([]string(nil), agent.Command...), nil, nil
	case "codex":
		return s.codexInvocation(agent)
	default:
		return nil, nil, fmt.Errorf("unsupported agent type %q", agent.Type)
	}
}

func (s *Service) codexInvocation(agent config.Agent) ([]string, io.Reader, error) {
	if strings.TrimSpace(s.codexBinary) == "" {
		return nil, nil, fmt.Errorf("codex binary is empty")
	}
	prompt, err := promptForAgent(agent)
	if err != nil {
		return nil, nil, err
	}
	sandbox := agent.Sandbox
	if sandbox == "" {
		sandbox = "read-only"
	}
	switch sandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return nil, nil, fmt.Errorf("unsupported Codex sandbox %q", sandbox)
	}

	command := []string{
		s.codexBinary,
		"--ask-for-approval", "never",
		"exec",
		"--color", "never",
		"--ephemeral",
		"--sandbox", sandbox,
	}
	if agent.Model != "" {
		command = append(command, "--model", agent.Model)
	}
	command = append(command, "-")
	return command, strings.NewReader(prompt), nil
}

func promptForAgent(agent config.Agent) (string, error) {
	hasInline := strings.TrimSpace(agent.Prompt) != ""
	hasFile := agent.PromptFile != ""
	if hasInline == hasFile {
		return "", fmt.Errorf("Codex agent requires exactly one of prompt or prompt_file")
	}
	if hasInline {
		return agent.Prompt, nil
	}
	contents, err := os.ReadFile(agent.PromptFile)
	if err != nil {
		return "", fmt.Errorf("read prompt file: %w", err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		return "", fmt.Errorf("prompt file is empty")
	}
	return string(contents), nil
}
