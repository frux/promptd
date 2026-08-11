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
	"github.com/frux/promptd/internal/store"
)

// Run loads the configuration and keeps the process alive until cancellation.
// Scheduling and execution are added in subsequent milestones.
func Run(ctx context.Context, configPath, statePath string, logger *slog.Logger) error {
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

	logger.Info(
		"daemon started",
		"config", configPath,
		"state", statePath,
		"jobs", len(cfg.Jobs),
		"recovered_runs", recovered,
	)

	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)

	for {
		select {
		case <-ctx.Done():
			logger.Info("daemon stopped")
			return nil
		case <-reload:
			next, err := config.Load(configPath)
			if err != nil {
				logger.Error("config reload rejected", "error", err)
				continue
			}
			if err := reconcileConfig(ctx, state, next); err != nil {
				logger.Error("config reload rejected", "error", err)
				continue
			}
			cfg = next
			logger.Info("config reloaded", "jobs", len(cfg.Jobs))
		}
	}
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
		specs = append(specs, store.JobSpec{
			ID:         id,
			ConfigHash: hex.EncodeToString(digest[:]),
			ConfigJSON: snapshot,
		})
	}
	return specs, nil
}
