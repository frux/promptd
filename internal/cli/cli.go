package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/frux/promptd/internal/buildinfo"
	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/control"
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
  status     Show all registered jobs
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
	case "status":
		return runStatus(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	statePath := flags.String("state", defaultStatePath(stderr), "path to SQLite state database")
	socketPath := flags.String("socket", "", "path to daemon control socket (default: next to state database)")
	offline := flags.Bool("offline", false, "read the state database without contacting the daemon")
	format := flags.String("format", "text", "output format: text or json")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "invalid status format %q: expected text or json\n", *format)
		return 2
	}

	resolvedSocketPath := *socketPath
	if resolvedSocketPath == "" {
		resolvedSocketPath = control.DefaultSocketPath(*statePath)
	}
	rows, err := loadStatus(context.Background(), *statePath, resolvedSocketPath, *offline)
	if err != nil {
		fmt.Fprintf(stderr, "status failed: %v\n", err)
		return 1
	}

	if *format == "json" {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(rows); err != nil {
			fmt.Fprintf(stderr, "status failed: encode output: %v\n", err)
			return 1
		}
		return 0
	}
	if err := writeTextStatus(stdout, rows); err != nil {
		fmt.Fprintf(stderr, "status failed: write output: %v\n", err)
		return 1
	}
	return 0
}

func loadStatus(ctx context.Context, statePath, socketPath string, offline bool) ([]control.JobStatus, error) {
	if !offline {
		response, err := control.NewClient(socketPath).Status(ctx)
		if err == nil {
			return response.Jobs, nil
		}
		if !errors.Is(err, control.ErrUnavailable) {
			return nil, err
		}
	}

	state, err := store.OpenReadOnly(ctx, statePath)
	if err != nil {
		return nil, err
	}
	defer state.Close()
	statuses, err := state.ListJobStatuses(ctx)
	if err != nil {
		return nil, err
	}
	return control.StatusFromStore(statuses).Jobs, nil
}

func writeTextStatus(output io.Writer, rows []control.JobStatus) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(output, "no registered jobs")
		return err
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "JOB\tSTATE\tNEXT RUN\tLAST RUN"); err != nil {
		return err
	}
	for _, row := range rows {
		state := "disabled"
		if row.Enabled {
			state = "enabled"
		}
		next := "-"
		if row.NextRun != nil {
			next = row.NextRun.UTC().Format(time.RFC3339)
		}
		last := "-"
		if row.LastRun != nil {
			when := row.LastRun.FinishedAt
			if when == nil {
				when = row.LastRun.StartedAt
			}
			if when == nil {
				when = row.LastRun.ScheduledAt
			}
			last = string(row.LastRun.Status)
			if when != nil {
				last += " @ " + when.UTC().Format(time.RFC3339)
			}
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", row.ID, state, next, last); err != nil {
			return err
		}
	}
	return writer.Flush()
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
	socketPath := flags.String("socket", "", "path to control socket (default: next to state database)")
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
	resolvedSocketPath := *socketPath
	if resolvedSocketPath == "" {
		resolvedSocketPath = control.DefaultSocketPath(*statePath)
	}
	if err := daemon.Run(ctx, *configPath, *statePath, resolvedLogDir, resolvedSocketPath, slog.New(handler)); err != nil {
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
