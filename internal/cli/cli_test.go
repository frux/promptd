package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "promptd dev") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
jobs:
  heartbeat:
    schedule:
      every: 1h
    agent:
      type: command
      command: ["true"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--config", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "config valid: 1 job(s)\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nope"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("Run() code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "nope"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSubcommandHelpExitsSuccessfully(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "Usage of validate") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
