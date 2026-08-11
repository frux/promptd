package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("control API unavailable")

type Client struct {
	http *http.Client
}

func NewClient(socketPath string) *Client {
	dialer := &net.Dialer{Timeout: time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives: true,
	}
	return &Client{http: &http.Client{Transport: transport, Timeout: 5 * time.Second}}
}

func (c *Client) Status(ctx context.Context) (StatusResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://promptd/v1/status", nil)
	if err != nil {
		return StatusResponse{}, fmt.Errorf("build control request: %w", err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return StatusResponse{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return StatusResponse{}, fmt.Errorf("control API returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	var status StatusResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&status); err != nil {
		return StatusResponse{}, fmt.Errorf("decode control response: %w", err)
	}
	if status.Version != APIVersion {
		return StatusResponse{}, fmt.Errorf("unsupported control API version %d", status.Version)
	}
	return status, nil
}

func DefaultSocketPath(statePath string) string {
	extension := filepath.Ext(statePath)
	if extension == "" {
		return statePath + ".sock"
	}
	return strings.TrimSuffix(statePath, extension) + ".sock"
}
