package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frux/promptd/internal/store"
)

type emptyStatusProvider struct{}

func (emptyStatusProvider) ListJobStatuses(context.Context) ([]store.JobStatus, error) {
	return nil, nil
}

func TestServerAndClientRun(t *testing.T) {
	for _, status := range []string{RunAccepted, RunQueued, RunSkipped} {
		t.Run(status, func(t *testing.T) {
			path := shortSocketPath(t)
			requested := make(chan string, 1)
			server, err := Start(path, emptyStatusProvider{}, func(id string) (RunResponse, error) {
				requested <- id
				return RunResponse{Version: APIVersion, JobID: id, Status: status}, nil
			}, discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer shutdownServer(t, server)
			response, err := NewClient(path).Run(context.Background(), "daily-report")
			if err != nil || response.JobID != "daily-report" || response.Status != status || response.Version != APIVersion {
				t.Fatalf("Run() = %#v, %v", response, err)
			}
			if id := <-requested; id != "daily-report" {
				t.Fatalf("requested job = %q", id)
			}
		})
	}
}

func TestRunErrorsAndMethod(t *testing.T) {
	path := shortSocketPath(t)
	server, err := Start(path, emptyStatusProvider{}, func(id string) (RunResponse, error) {
		if id == "stopping" {
			return RunResponse{}, ErrStopping
		}
		return RunResponse{}, ErrUnknownJob
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownServer(t, server)
	client := NewClient(path)
	for id, want := range map[string]string{"missing": "404", "stopping": "503"} {
		if _, err := client.Run(context.Background(), id); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Run(%q) error = %v, want %s", id, err, want)
		}
	}
	response, err := client.http.Get("http://promptd/v1/jobs/report/run")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET run status = %d", response.StatusCode)
	}
	if _, err := NewClient(path+".missing").Run(context.Background(), "report"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable Run() error = %v", err)
	}
}

func TestServerAndClientStatus(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	state, err := store.Open(ctx, filepath.Join(directory, "promptd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.ReconcileJobs(ctx, []store.JobSpec{{
		ID: "report", ConfigHash: "config", ScheduleHash: "schedule", ConfigJSON: []byte(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}
	next := time.Date(2026, 8, 12, 4, 0, 0, 0, time.UTC)
	if err := state.SetSchedulerState(ctx, "report", &next, nil); err != nil {
		t.Fatal(err)
	}

	socketPath := shortSocketPath(t)
	server, err := Start(socketPath, state, nil, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownServer(t, server)
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want socket 0600", info.Mode())
	}

	response, err := NewClient(socketPath).Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if response.Version != APIVersion || len(response.Jobs) != 1 {
		t.Fatalf("response = %#v", response)
	}
	job := response.Jobs[0]
	if job.ID != "report" || !job.Enabled || job.NextRun == nil || !job.NextRun.Equal(next) {
		t.Fatalf("job = %#v", job)
	}
}

func TestStartRejectsActiveSocket(t *testing.T) {
	directory := t.TempDir()
	state, err := store.Open(context.Background(), filepath.Join(directory, "promptd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	socketPath := shortSocketPath(t)
	server, err := Start(socketPath, state, nil, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownServer(t, server)

	if _, err := Start(socketPath, state, nil, discardLogger()); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("second Start() error = %v", err)
	}
}

func TestStartDoesNotReplaceRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "promptd.sock")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(context.Background(), filepath.Join(directory, "promptd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if _, err := Start(path, state, nil, discardLogger()); err == nil || !strings.Contains(err.Error(), "is not a socket") {
		t.Fatalf("Start() error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "keep" {
		t.Fatalf("regular file changed: contents=%q error=%v", contents, err)
	}
}

func TestStartRemovesStaleSocket(t *testing.T) {
	directory := t.TempDir()
	state, err := store.Open(context.Background(), filepath.Join(directory, "promptd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	path := shortSocketPath(t)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket missing: %v", err)
	}

	server, err := Start(path, state, nil, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownServer(t, server)
	if _, err := NewClient(path).Status(context.Background()); err != nil {
		t.Fatalf("Status() after stale socket recovery error = %v", err)
	}
}

func TestClientReportsUnavailableSocket(t *testing.T) {
	_, err := NewClient(filepath.Join(t.TempDir(), "missing.sock")).Status(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Status() error = %v", err)
	}
}

func TestDefaultSocketPath(t *testing.T) {
	for input, expected := range map[string]string{
		"/state/promptd.db": "/state/promptd.sock",
		"/state/promptd":    "/state/promptd.sock",
	} {
		if actual := DefaultSocketPath(input); actual != expected {
			t.Errorf("DefaultSocketPath(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func shutdownServer(t *testing.T, server *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown() error = %v", err)
	}
	if err := <-server.Done(); err != nil {
		t.Errorf("Serve() error = %v", err)
	}
	if _, err := os.Lstat(server.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still exists after shutdown: %v", err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "promptd-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "p.sock")
}
