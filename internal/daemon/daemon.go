package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/control"
	"github.com/frux/promptd/internal/runner"
	"github.com/frux/promptd/internal/scheduler"
	"github.com/frux/promptd/internal/store"
)

// Run loads the configuration, schedules jobs, and keeps the process alive
// until cancellation.
func Run(ctx context.Context, configPath, statePath, logDir, socketPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	state, err := store.Open(ctx, statePath)
	if err != nil {
		return fmt.Errorf("open state store: %w", err)
	}
	defer func() {
		if err := state.Close(); err != nil {
			logger.Error("state store close failed", "error", err)
		}
	}()

	if err := reconcileConfig(ctx, state, cfg); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	recovered, err := state.RecoverInterruptedRuns(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	execution, err := newJobRuntime(ctx, state, runner.New(nil), logDir, logger, cfg.Jobs)
	if err != nil {
		return err
	}
	defer execution.Shutdown()

	engine, err := scheduler.New(ctx, state, scheduler.Definitions(cfg.Jobs), execution.Trigger)
	if err != nil {
		return fmt.Errorf("initialize scheduler: %w", err)
	}
	controlServer, err := control.Start(socketPath, state, logger)
	if err != nil {
		return fmt.Errorf("start control API: %w", err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := controlServer.Shutdown(shutdownContext); err != nil {
			logger.Error("control API shutdown failed", "error", err)
		}
	}()
	schedule := startScheduler(ctx, engine)

	logger.Info(
		"daemon started",
		"config", configPath,
		"state", statePath,
		"logs", logDir,
		"socket", socketPath,
		"jobs", len(cfg.Jobs),
		"recovered_runs", recovered,
	)

	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)

	for {
		select {
		case <-ctx.Done():
			if schedule != nil {
				if err := schedule.stop(); err != nil {
					return fmt.Errorf("stop scheduler: %w", err)
				}
			}
			logger.Info("daemon stopped")
			return nil
		case err := <-schedule.done:
			schedule = nil
			if ctx.Err() != nil && err == nil {
				logger.Info("daemon stopped")
				return nil
			}
			if err == nil {
				return fmt.Errorf("scheduler stopped unexpectedly")
			}
			return fmt.Errorf("scheduler failed: %w", err)
		case err := <-controlServer.Done():
			if schedule != nil {
				if stopErr := schedule.stop(); stopErr != nil {
					return fmt.Errorf("control API failed: %v; stop scheduler: %w", err, stopErr)
				}
				schedule = nil
			}
			if err == nil {
				return fmt.Errorf("control API stopped unexpectedly")
			}
			return fmt.Errorf("control API failed: %w", err)
		case <-reload:
			next, err := config.Load(configPath)
			if err != nil {
				logger.Error("config reload rejected", "error", err)
				continue
			}
			if err := schedule.stop(); err != nil {
				return fmt.Errorf("stop scheduler for reload: %w", err)
			}
			schedule = nil

			if err := reconcileConfig(ctx, state, next); err != nil {
				logger.Error("config reload rejected", "error", err)
				schedule, err = rebuildScheduler(ctx, state, cfg, execution)
				if err != nil {
					return fmt.Errorf("restart scheduler after rejected reload: %w", err)
				}
				continue
			}
			engine, err := scheduler.New(ctx, state, scheduler.Definitions(next.Jobs), execution.Trigger)
			if err != nil {
				reloadErr := err
				if rollbackErr := reconcileConfig(ctx, state, cfg); rollbackErr != nil {
					return fmt.Errorf("reload scheduler: %v; roll back config: %w", reloadErr, rollbackErr)
				}
				schedule, err = rebuildScheduler(ctx, state, cfg, execution)
				if err != nil {
					return fmt.Errorf("restart scheduler after rollback: %w", err)
				}
				logger.Error("config reload rejected", "error", reloadErr)
				continue
			}
			execution.Replace(next.Jobs)
			schedule = startScheduler(ctx, engine)
			cfg = next
			logger.Info("config reloaded", "jobs", len(cfg.Jobs))
		}
	}
}

type schedulerProcess struct {
	cancel context.CancelFunc
	done   chan error
}

func startScheduler(parent context.Context, engine *scheduler.Engine) *schedulerProcess {
	ctx, cancel := context.WithCancel(parent)
	process := &schedulerProcess{cancel: cancel, done: make(chan error, 1)}
	go func() {
		process.done <- engine.Run(ctx)
	}()
	return process
}

func (p *schedulerProcess) stop() error {
	p.cancel()
	return <-p.done
}

func rebuildScheduler(ctx context.Context, state *store.Store, cfg *config.Config, execution *jobRuntime) (*schedulerProcess, error) {
	engine, err := scheduler.New(ctx, state, scheduler.Definitions(cfg.Jobs), execution.Trigger)
	if err != nil {
		return nil, err
	}
	return startScheduler(ctx, engine), nil
}

func reconcileConfig(ctx context.Context, state *store.Store, cfg *config.Config) error {
	specs, err := jobSpecs(cfg)
	if err != nil {
		return err
	}
	return state.ReconcileJobs(ctx, specs)
}

func jobSpecs(cfg *config.Config) ([]store.JobSpec, error) {
	ids := make([]string, 0, len(cfg.Jobs))
	for id := range cfg.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	specs := make([]store.JobSpec, 0, len(ids))
	for _, id := range ids {
		job := cfg.Jobs[id]
		snapshot, err := json.Marshal(job)
		if err != nil {
			return nil, fmt.Errorf("encode job %q: %w", id, err)
		}
		digest := sha256.Sum256(snapshot)
		scheduleSnapshot, err := json.Marshal(job.Schedule)
		if err != nil {
			return nil, fmt.Errorf("encode schedule for job %q: %w", id, err)
		}
		scheduleDigest := sha256.Sum256(scheduleSnapshot)
		specs = append(specs, store.JobSpec{
			ID:           id,
			ConfigHash:   hex.EncodeToString(digest[:]),
			ScheduleHash: hex.EncodeToString(scheduleDigest[:]),
			ConfigJSON:   snapshot,
		})
	}
	return specs, nil
}
