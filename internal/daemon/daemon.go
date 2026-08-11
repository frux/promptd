package daemon

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/frux/promptd/internal/config"
)

// Run loads the configuration and keeps the process alive until cancellation.
// Scheduling and execution are added in subsequent milestones.
func Run(ctx context.Context, configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logger.Info("daemon started", "config", configPath, "jobs", len(cfg.Jobs))

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
			cfg = next
			logger.Info("config reloaded", "jobs", len(cfg.Jobs))
		}
	}
}
