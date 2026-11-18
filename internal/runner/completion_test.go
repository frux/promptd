package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/supervisor"
)

type completionExecutor struct {
	answer string
	result supervisor.Result
	path   string
}

func (e *completionExecutor) Run(_ context.Context, spec supervisor.Spec) (supervisor.Result, error) {
	for i, arg := range spec.Command {
		if arg == "--output-last-message" {
			e.path = spec.Command[i+1]
		}
	}
	prompt, _ := io.ReadAll(spec.Stdin)
	if !strings.Contains(string(prompt), "Completion contract:") {
		return e.result, os.ErrInvalid
	}
	// A marker in progress output must never satisfy the final-result check.
	if spec.Output != nil {
		_, _ = io.WriteString(spec.Output, "DELIVERED\n")
	}
	return e.result, os.WriteFile(e.path, []byte(e.answer), 0o600)
}

func TestCodexCompletionContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		result supervisor.Result
		failed bool
	}{
		{name: "delivery confirmed", answer: "DELIVERED\nSent the complete brief."},
		{name: "workflow stopped despite exit zero", answer: "Stopped: PR status unavailable.", failed: true},
		{name: "missing final result", failed: true},
		{name: "marker only mentioned later", answer: "Failed.\nDELIVERED", failed: true},
		{name: "partial marker", answer: "DELIVERED_PARTIALLY", failed: true},
		{name: "nonzero preserved", result: supervisor.Result{ExitCode: 7}},
		{name: "timeout preserved", result: supervisor.Result{TimedOut: true}},
		{name: "cancel preserved", result: supervisor.Result{Canceled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &completionExecutor{answer: tc.answer, result: tc.result}
			job := config.Job{Agent: config.Agent{Type: "codex", Prompt: "Deliver a report.", SuccessMarker: "DELIVERED"}}
			var output strings.Builder
			result, err := New(executor).Run(context.Background(), job, &output)
			if (err != nil) != tc.failed {
				t.Fatalf("Run() error = %v, want failure %v", err, tc.failed)
			}
			if result != tc.result {
				t.Fatalf("process result changed: %#v", result)
			}
			if _, err := os.Stat(executor.path); !os.IsNotExist(err) {
				t.Fatalf("temporary final-result file was not removed: %v", err)
			}
		})
	}
}

func TestCodexResultPathResolvesTempDirectorySymlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, link)
	}

	executor := &completionExecutor{answer: "DELIVERED\nDone."}
	job := config.Job{Agent: config.Agent{Type: "codex", Prompt: "Deliver a report.", SuccessMarker: "DELIVERED"}}
	if _, err := New(executor).Run(context.Background(), job, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Dir(executor.path); got != target {
		t.Fatalf("Codex result directory = %q, want resolved directory %q", got, target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary result files remain: %v", entries)
	}
}
