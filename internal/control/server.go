package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/frux/promptd/internal/store"
)

type statusProvider interface {
	ListJobStatuses(context.Context) ([]store.JobStatus, error)
}

type Server struct {
	path string
	http *http.Server
	done chan error
}

func Start(path string, provider statusProvider, runJob func(string) (RunResponse, error), logger *slog.Logger) (*Server, error) {
	if path == "" {
		return nil, fmt.Errorf("control socket path is empty")
	}
	if provider == nil {
		return nil, fmt.Errorf("control status provider is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	listener, err := listen(path)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs/{id}/run", func(writer http.ResponseWriter, request *http.Request) {
		if runJob == nil {
			writeError(writer, http.StatusServiceUnavailable, "manual runs are unavailable")
			return
		}
		response, err := runJob(request.PathValue("id"))
		if err != nil {
			switch {
			case errors.Is(err, ErrUnknownJob):
				writeError(writer, http.StatusNotFound, err.Error())
			case errors.Is(err, ErrStopping):
				writeError(writer, http.StatusServiceUnavailable, err.Error())
			default:
				logger.Error("control run failed", "error", err)
				writeError(writer, http.StatusInternalServerError, "cannot run job")
			}
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			logger.Error("control response failed", "error", err)
		}
	})
	mux.HandleFunc("GET /v1/status", func(writer http.ResponseWriter, request *http.Request) {
		statuses, err := provider.ListJobStatuses(request.Context())
		if err != nil {
			logger.Error("control status failed", "error", err)
			writeError(writer, http.StatusInternalServerError, "cannot read job status")
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(writer).Encode(StatusFromStore(statuses)); err != nil {
			logger.Error("control response failed", "error", err)
		}
	})

	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	server := &Server{
		path: path,
		http: httpServer,
		done: make(chan error, 1),
	}
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		server.done <- err
		close(server.done)
	}()
	return server, nil
}

func (s *Server) Done() <-chan error {
	return s.done
}

func (s *Server) Shutdown(ctx context.Context) error {
	err := s.http.Shutdown(ctx)
	removeErr := os.Remove(s.path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		removeErr = fmt.Errorf("remove control socket: %w", removeErr)
	} else {
		removeErr = nil
	}
	return errors.Join(err, removeErr)
}

func listen(path string) (net.Listener, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create control socket directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket %q: %w", path, err)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(true)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("secure control socket: %w", err)
	}
	return listener, nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect control socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("control socket path %q exists and is not a socket", path)
	}

	connection, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
	if dialErr == nil {
		connection.Close()
		return fmt.Errorf("control socket %q is already in use", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale control socket: %w", err)
	}
	return nil
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}
