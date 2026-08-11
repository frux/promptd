package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/frux/promptd/internal/buildinfo"
	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/daemon"
	"github.com/frux/promptd/internal/store"
)

const usage = `promptd schedules prompts and agent commands.

Usage:
  promptd <command> [options]

Commands:
  version    Print build information
  validate   Validate a configuration file
  daemon     Run the promptd daemon
  help       Show this help
`

// Run executes the CLI and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}

	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version":
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "daemon":
		return runDaemon(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runValidate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(stderr), "path to config file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "invalid config: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "config valid: %d job(s)\n", len(cfg.Jobs))
	return 0
}

func runDaemon(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(stderr), "path to config file")
	statePath := flags.String("state", defaultStatePath(stderr), "path to SQLite state database")
	logDir := flags.String("log-dir", "", "directory for per-run logs (default: next to state database)")
	logFormat := flags.String("log-format", "text", "log format: text or json")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	var handler slog.Handler
	switch *logFormat {
	case "text":
		handler = slog.NewTextHandler(stdout, nil)
	case "json":
		handler = slog.NewJSONHandler(stdout, nil)
	default:
		fmt.Fprintf(stderr, "invalid log format %q: expected text or json\n", *logFormat)
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	resolvedLogDir := *logDir
	if resolvedLogDir == "" {
		resolvedLogDir = filepath.Join(filepath.Dir(*statePath), "logs")
	}
	if err := daemon.Run(ctx, *configPath, *statePath, resolvedLogDir, slog.New(handler)); err != nil {
		fmt.Fprintf(stderr, "daemon failed: %v\n", err)
		return 1
	}
	return 0
}

func defaultStatePath(stderr io.Writer) string {
	path, err := store.DefaultPath()
	if err != nil {
		fmt.Fprintf(stderr, "warning: cannot resolve default state path: %v\n", err)
		return "promptd.db"
	}
	return path
}

func defaultConfigPath(stderr io.Writer) string {
	path, err := config.DefaultPath()
	if err != nil {
		fmt.Fprintf(stderr, "warning: cannot resolve default config path: %v\n", err)
		return "config.yaml"
	}
	return path
}
