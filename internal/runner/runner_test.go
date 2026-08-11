package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/supervisor"
)

func TestCommandRunnerBuildsDirectInvocation(t *testing.T) {
	executor := &recordingExecutor{result: supervisor.Result{ExitCode: 0}}
	output := &strings.Builder{}
	job := config.Job{
		Agent: config.Agent{Type: "command", Command: []string{"printf", "%s", "hello world"}},
		Run: config.Run{
			WorkingDirectory: "/srv/project",
			Timeout:          config.Duration(2 * time.Minute),
		},
	}

	result, err := New(executor).Run(context.Background(), job, output)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(executor.spec.Command, job.Agent.Command) {
		t.Fatalf("command = %#v", executor.spec.Command)
	}
	if executor.spec.Directory != "/srv/project" || executor.spec.Timeout != 2*time.Minute {
		t.Fatalf("spec = %#v", executor.spec)
	}
	if executor.spec.Output != output || executor.spec.Stdin != nil || executor.spec.Environment != nil {
		t.Fatalf("spec I/O = %#v", executor.spec)
	}
}

func TestCommandRunnerExecutesThroughSupervisor(t *testing.T) {
	t.Setenv("PROMPTD_RUNNER_HELPER", "1")
	var output bytes.Buffer
	job := config.Job{
		Agent: config.Agent{
			Type:    "command",
			Command: []string{os.Args[0], "-test.run=TestRunnerHelperProcess"},
		},
		Run: config.Run{Timeout: config.Duration(5 * time.Second)},
	}

	result, err := New(supervisor.New()).Run(context.Background(), job, &output)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 || output.String() != "runner-output\n" {
		t.Fatalf("result = %#v, output = %q", result, output.String())
	}
}

func TestCodexRunnerUsesNonInteractiveStdin(t *testing.T) {
	executor := &recordingExecutor{}
	job := config.Job{
		Agent: config.Agent{
			Type:    "codex",
			Prompt:  "Review this repository.",
			Model:   "gpt-test",
			Sandbox: "workspace-write",
		},
		Run: config.Run{Timeout: config.Duration(time.Minute)},
	}

	_, err := New(executor, WithCodexBinary("/opt/codex")).Run(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantCommand := []string{
		"/opt/codex", "--ask-for-approval", "never", "exec", "--color", "never",
		"--ephemeral", "--sandbox", "workspace-write", "--model", "gpt-test", "-",
	}
	if !reflect.DeepEqual(executor.spec.Command, wantCommand) {
		t.Fatalf("command = %#v, want %#v", executor.spec.Command, wantCommand)
	}
	prompt, err := io.ReadAll(executor.spec.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if string(prompt) != job.Agent.Prompt {
		t.Fatalf("stdin = %q", prompt)
	}
}

func TestCodexRunnerReadsPromptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(path, []byte("Prompt from a file.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{}
	job := config.Job{
		Agent: config.Agent{Type: "codex", PromptFile: path, Sandbox: "read-only"},
		Run:   config.Run{Timeout: config.Duration(time.Minute)},
	}
	if _, err := New(executor).Run(context.Background(), job, nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	prompt, err := io.ReadAll(executor.spec.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if string(prompt) != "Prompt from a file.\n" {
		t.Fatalf("stdin = %q", prompt)
	}
}

func TestClaudeRunnerUsesHeadlessMode(t *testing.T) {
	executor := &recordingExecutor{}
	job := config.Job{
		Agent: config.Agent{
			Type:           "claude",
			Prompt:         "Fix the tests.",
			Model:          "test-model",
			PermissionMode: "acceptEdits",
			AllowedTools:   []string{"Read", "Edit", "Bash(go test *)"},
			Bare:           true,
		},
		Run: config.Run{Timeout: config.Duration(time.Minute)},
	}

	_, err := New(executor, WithClaudeBinary("/opt/claude")).Run(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantCommand := []string{
		"/opt/claude", "--bare", "--print", "--no-session-persistence",
		"--output-format", "text", "--permission-mode", "acceptEdits",
		"--allowedTools", "Read", "Edit", "Bash(go test *)", "--model", "test-model",
		"Fix the tests.",
	}
	if !reflect.DeepEqual(executor.spec.Command, wantCommand) {
		t.Fatalf("command = %#v, want %#v", executor.spec.Command, wantCommand)
	}
	if executor.spec.Stdin != nil {
		t.Fatalf("stdin = %#v, want nil", executor.spec.Stdin)
	}
}

func TestClaudeRunnerDefaultsToDontAsk(t *testing.T) {
	executor := &recordingExecutor{}
	job := config.Job{
		Agent: config.Agent{Type: "claude", Prompt: "Review this."},
		Run:   config.Run{Timeout: config.Duration(time.Minute)},
	}
	if _, err := New(executor).Run(context.Background(), job, nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := executor.spec.Command; !reflect.DeepEqual(got, []string{
		"claude", "--print", "--no-session-persistence", "--output-format", "text",
		"--permission-mode", "dontAsk", "Review this.",
	}) {
		t.Fatalf("command = %#v", got)
	}
}

func TestGeminiRunnerUsesHeadlessApprovalContract(t *testing.T) {
	executor := &recordingExecutor{}
	job := config.Job{
		Agent: config.Agent{
			Type:         "gemini",
			Prompt:       "Fix the tests.",
			Model:        "gemini-test",
			ApprovalMode: "auto_edit",
			Sandbox:      "enabled",
			AllowedTools: []string{"read_file", "replace"},
		},
		Run: config.Run{Timeout: config.Duration(time.Minute)},
	}

	_, err := New(executor, WithGeminiBinary("/opt/gemini")).Run(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantCommand := []string{
		"/opt/gemini", "--skip-trust", "--output-format", "text",
		"--approval-mode", "auto_edit", "--sandbox",
		"--allowed-tools", "read_file,replace", "--model", "gemini-test",
		"--prompt", "Fix the tests.",
	}
	if !reflect.DeepEqual(executor.spec.Command, wantCommand) {
		t.Fatalf("command = %#v, want %#v", executor.spec.Command, wantCommand)
	}
	if executor.spec.Stdin != nil {
		t.Fatalf("stdin = %#v, want nil", executor.spec.Stdin)
	}
}

func TestGeminiRunnerDefaultsToPlan(t *testing.T) {
	executor := &recordingExecutor{}
	job := config.Job{
		Agent: config.Agent{Type: "gemini", Prompt: "Review this."},
		Run:   config.Run{Timeout: config.Duration(time.Minute)},
	}
	if _, err := New(executor).Run(context.Background(), job, nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := []string{
		"gemini", "--skip-trust", "--output-format", "text",
		"--approval-mode", "plan", "--prompt", "Review this.",
	}
	if !reflect.DeepEqual(executor.spec.Command, want) {
		t.Fatalf("command = %#v, want %#v", executor.spec.Command, want)
	}
}

func TestRunnerOverlaysEnvironmentFile(t *testing.T) {
	t.Setenv("PROMPTD_EXISTING", "old")
	path := filepath.Join(t.TempDir(), "job.env")
	contents := "# job settings\nexport PROMPTD_EXISTING=new\nPROMPTD_QUOTED='hello world'\nPROMPTD_ESCAPED=\"line\\nvalue\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{}
	job := config.Job{
		Agent: config.Agent{Type: "command", Command: []string{"true"}},
		Run: config.Run{
			Timeout:         config.Duration(time.Minute),
			EnvironmentFile: path,
		},
	}
	if _, err := New(executor).Run(context.Background(), job, nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := environmentValue(executor.spec.Environment, "PROMPTD_EXISTING"); got != "new" {
		t.Fatalf("existing value = %q", got)
	}
	if got := environmentValue(executor.spec.Environment, "PROMPTD_QUOTED"); got != "hello world" {
		t.Fatalf("quoted value = %q", got)
	}
	if got := environmentValue(executor.spec.Environment, "PROMPTD_ESCAPED"); got != "line\nvalue" {
		t.Fatalf("escaped value = %q", got)
	}
}

func TestRunnerRejectsInvalidEnvironmentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.env")
	if err := os.WriteFile(path, []byte("NOT VALID=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{}
	job := config.Job{
		Agent: config.Agent{Type: "command", Command: []string{"true"}},
		Run: config.Run{
			Timeout:         config.Duration(time.Minute),
			EnvironmentFile: path,
		},
	}
	if _, err := New(executor).Run(context.Background(), job, nil); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("Run() error = %v", err)
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", executor.calls)
	}
}

func TestRunnerRejectsInvalidInputBeforeExecution(t *testing.T) {
	executor := &recordingExecutor{}
	tests := []config.Agent{
		{Type: "command"},
		{Type: "codex", Prompt: "Hello", PromptFile: "also.md"},
		{Type: "codex", Prompt: "Hello", Sandbox: "root"},
		{Type: "claude", Prompt: "Hello", PermissionMode: "interactive"},
		{Type: "gemini", Prompt: "Hello", ApprovalMode: "default"},
		{Type: "gemini", Prompt: "Hello", ApprovalMode: "plan", Sandbox: "disabled"},
		{Type: "gemini", Prompt: "Hello", ApprovalMode: "plan", AllowedTools: []string{"tool,other"}},
		{Type: "unknown"},
	}
	for _, agent := range tests {
		job := config.Job{Agent: agent, Run: config.Run{Timeout: config.Duration(time.Minute)}}
		if _, err := New(executor).Run(context.Background(), job, nil); err == nil {
			t.Fatalf("agent %#v: error = nil", agent)
		}
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", executor.calls)
	}
}

func TestRunnerWrapsExecutorError(t *testing.T) {
	executor := &recordingExecutor{err: errors.New("boom")}
	job := config.Job{
		Agent: config.Agent{Type: "command", Command: []string{"true"}},
		Run:   config.Run{Timeout: config.Duration(time.Minute)},
	}
	if _, err := New(executor).Run(context.Background(), job, nil); err == nil || !strings.Contains(err.Error(), "run command agent") {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunnerHelperProcess(t *testing.T) {
	if os.Getenv("PROMPTD_RUNNER_HELPER") != "1" {
		return
	}
	_, _ = os.Stdout.WriteString("runner-output\n")
	os.Exit(0)
}

type recordingExecutor struct {
	spec   supervisor.Spec
	result supervisor.Result
	err    error
	calls  int
}

func (e *recordingExecutor) Run(_ context.Context, spec supervisor.Spec) (supervisor.Result, error) {
	e.calls++
	e.spec = spec
	return e.result, e.err
}

func environmentValue(environment []string, key string) string {
	prefix := key + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}
