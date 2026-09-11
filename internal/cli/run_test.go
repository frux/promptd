package cli

import (
	"bytes"
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

	"github.com/frux/promptd/internal/control"
	"github.com/frux/promptd/internal/daemon"
	"github.com/frux/promptd/internal/store"
)

func TestRunArguments(t *testing.T) {
	for _, args := range [][]string{
		{"run"}, {"run", ""}, {"run", "", "report"}, {"run", " "},
		{"run", "one", "two"}, {"run", "--socket"}, {"run", "report", "--unknown"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 {
			t.Errorf("Run(%q) = %d, stderr=%q", args, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "<task-name>") {
		t.Fatalf("help = %d, %q", code, stderr.String())
	}
}

func TestRunNeedsDaemonWithoutCreatingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "report", "--state", path}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "require a running promptd daemon") {
		t.Fatalf("Run() = %d, stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("run created state: %v", err)
	}
}

func TestRunReportsDaemonResponse(t *testing.T) {
	state, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "promptd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	dir, err := os.MkdirTemp("/tmp", "promptd-cli-run-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "p.sock")
	server, err := control.Start(path, state, func(id string) (control.RunResponse, error) {
		if id == "missing" {
			return control.RunResponse{}, control.ErrUnknownJob
		}
		return control.RunResponse{Version: control.APIVersion, JobID: id, Status: id}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	for _, test := range []struct {
		name    string
		code    int
		message string
	}{
		{control.RunAccepted, 0, "accepted for execution"},
		{control.RunQueued, 0, "queued after the active run"},
		{control.RunSkipped, 1, "skipped: previous run is still active"},
		{"missing", 1, "not in the active configuration"},
	} {
		for _, args := range [][]string{
			{"run", test.name, "--socket", path},
			{"run", "--socket", path, test.name},
			{"run", test.name, "--state", filepath.Join(dir, "p.db")},
		} {
			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr)
			output := stdout.String()
			if test.code != 0 {
				output = stderr.String()
			}
			if code != test.code || !strings.Contains(output, test.message) {
				t.Errorf("Run(%q) = %d, stdout=%q, stderr=%q", args, code, stdout.String(), stderr.String())
			}
		}
	}
}

func TestRunThroughDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTD_MANUAL_HELPER", "1")
	command, err := json.Marshal([]string{os.Args[0], "-test.run=TestManualRunHelperProcess"})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	contents := fmt.Sprintf("version: 1\njobs:\n  report:\n    schedule:\n      every: 24h\n    agent:\n      type: command\n      command: %s\n", command)
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "promptd-run-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "p.sock")
	statePath := filepath.Join(dir, "promptd.db")
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		finished <- daemon.Run(ctx, configPath, statePath, filepath.Join(dir, "logs"), socket, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("daemon failed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	}()
	client := control.NewClient(socket)
	var before control.StatusResponse
	deadline := time.Now().Add(10 * time.Second)
	for {
		before, err = client.Status(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(before.Jobs) != 1 || before.Jobs[0].LastRun != nil || before.Jobs[0].NextRun == nil {
		t.Fatalf("initial status = %#v", before)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "report", "--socket", socket}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr=%q", code, stderr.String())
	}
	for {
		after, err := client.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		job := after.Jobs[0]
		if job.LastRun != nil && job.LastRun.Status == store.RunSucceeded {
			if job.NextRun == nil || !job.NextRun.Equal(*before.Jobs[0].NextRun) {
				t.Fatalf("manual run changed next occurrence: %#v", job)
			}
			log, err := os.ReadFile(job.LastRun.LogPath)
			if err != nil || !strings.Contains(string(log), "manual helper ran") {
				t.Fatalf("run log = %q, %v", log, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manual job did not succeed: %#v", job.LastRun)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManualRunHelperProcess(t *testing.T) {
	if os.Getenv("PROMPTD_MANUAL_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "manual helper ran")
	os.Exit(0)
}
