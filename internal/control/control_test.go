package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frux/promptd/internal/store"
)

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
	server, err := Start(socketPath, state, discardLogger())
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
	server, err := Start(socketPath, state, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownServer(t, server)

	if _, err := Start(socketPath, state, discardLogger()); err == nil || !strings.Contains(err.Error(), "already in use") {
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
	if _, err := Start(path, state, discardLogger()); err == nil || !strings.Contains(err.Error(), "is not a socket") {
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

	server, err := Start(path, state, discardLogger())
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
