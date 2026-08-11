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
	executor     Executor
	codexBinary  string
	claudeBinary string
	geminiBinary string
}

type Option func(*Service)

func WithCodexBinary(path string) Option {
	return func(service *Service) {
		service.codexBinary = path
	}
}

func WithClaudeBinary(path string) Option {
	return func(service *Service) {
		service.claudeBinary = path
	}
}

func WithGeminiBinary(path string) Option {
	return func(service *Service) {
		service.geminiBinary = path
	}
}

func New(executor Executor, options ...Option) *Service {
	if executor == nil {
		executor = supervisor.New()
	}
	service := &Service{
		executor:     executor,
		codexBinary:  "codex",
		claudeBinary: "claude",
		geminiBinary: "gemini",
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
	case "claude":
		return s.claudeInvocation(agent)
	case "gemini":
		return s.geminiInvocation(agent)
	default:
		return nil, nil, fmt.Errorf("unsupported agent type %q", agent.Type)
	}
}

func (s *Service) geminiInvocation(agent config.Agent) ([]string, io.Reader, error) {
	if strings.TrimSpace(s.geminiBinary) == "" {
		return nil, nil, fmt.Errorf("Gemini binary is empty")
	}
	prompt, err := promptForAgent(agent)
	if err != nil {
		return nil, nil, err
	}
	approvalMode := agent.ApprovalMode
	if approvalMode == "" {
		approvalMode = "plan"
	}
	switch approvalMode {
	case "plan", "auto_edit", "yolo":
	default:
		return nil, nil, fmt.Errorf("unsupported Gemini approval mode %q", approvalMode)
	}

	command := []string{
		s.geminiBinary,
		"--skip-trust",
		"--output-format", "text",
		"--approval-mode", approvalMode,
	}
	if agent.Sandbox == "enabled" {
		command = append(command, "--sandbox")
	} else if agent.Sandbox != "" {
		return nil, nil, fmt.Errorf("unsupported Gemini sandbox %q", agent.Sandbox)
	}
	if len(agent.AllowedTools) != 0 {
		for _, tool := range agent.AllowedTools {
			if strings.Contains(tool, ",") {
				return nil, nil, fmt.Errorf("Gemini allowed tool %q contains a comma", tool)
			}
		}
		command = append(command, "--allowed-tools", strings.Join(agent.AllowedTools, ","))
	}
	if agent.Model != "" {
		command = append(command, "--model", agent.Model)
	}
	command = append(command, "--prompt", prompt)
	return command, nil, nil
}

func (s *Service) claudeInvocation(agent config.Agent) ([]string, io.Reader, error) {
	if strings.TrimSpace(s.claudeBinary) == "" {
		return nil, nil, fmt.Errorf("Claude binary is empty")
	}
	prompt, err := promptForAgent(agent)
	if err != nil {
		return nil, nil, err
	}
	permissionMode := agent.PermissionMode
	if permissionMode == "" {
		permissionMode = "dontAsk"
	}
	switch permissionMode {
	case "dontAsk", "acceptEdits", "auto", "plan", "bypassPermissions":
	default:
		return nil, nil, fmt.Errorf("unsupported Claude permission mode %q", permissionMode)
	}

	command := []string{s.claudeBinary}
	if agent.Bare {
		command = append(command, "--bare")
	}
	command = append(command,
		"--print",
		"--no-session-persistence",
		"--output-format", "text",
		"--permission-mode", permissionMode,
	)
	if len(agent.AllowedTools) > 0 {
		command = append(command, "--allowedTools")
		command = append(command, agent.AllowedTools...)
	}
	if agent.Model != "" {
		command = append(command, "--model", agent.Model)
	}
	command = append(command, prompt)
	return command, nil, nil
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
		return "", fmt.Errorf("%s agent requires exactly one of prompt or prompt_file", agent.Type)
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
