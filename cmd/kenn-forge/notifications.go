package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
)

const databaseOptimizeInterval = 24 * time.Hour

type backgroundLoopHandle struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type notificationLoopSettings struct {
	syncInterval        time.Duration
	propagationInterval time.Duration
	batchSize           int
}

func (h *backgroundLoopHandle) Stop(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.cancel()
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func startBackgroundLoops(ctx context.Context, database *db.DB) *backgroundLoopHandle {
	handle := newBackgroundLoopHandle(ctx)
	handle.startTickerAfterInterval("database optimize", databaseOptimizeInterval, database.Optimize)
	return handle
}

func startNotificationLoops(handle *backgroundLoopHandle, syncer *ghclient.Syncer, cfg *config.Config) {
	settings := notificationLoopSettingsFromConfig(cfg)
	handle.startTicker("notification sync", settings.syncInterval, func(runCtx context.Context) error {
		return syncer.RunNotificationSync(runCtx)
	})
	handle.startTicker("notification read propagation", settings.propagationInterval, func(runCtx context.Context) error {
		return syncer.ProcessQueuedNotificationReadsForAllHosts(runCtx, settings.batchSize)
	})
}

func notificationLoopSettingsFromConfig(cfg *config.Config) notificationLoopSettings {
	return notificationLoopSettings{
		syncInterval:        cfg.NotificationSyncDuration(),
		propagationInterval: cfg.NotificationPropagationDuration(),
		batchSize:           cfg.NotificationBatchSize(),
	}
}

func newBackgroundLoopHandle(parent context.Context) *backgroundLoopHandle {
	ctx, cancel := context.WithCancel(parent)
	return &backgroundLoopHandle{ctx: ctx, cancel: cancel}
}

func (h *backgroundLoopHandle) startTicker(name string, interval time.Duration, run func(context.Context) error) {
	h.startTickerLoop(name, interval, true, run)
}

func (h *backgroundLoopHandle) startTickerAfterInterval(
	name string,
	interval time.Duration,
	run func(context.Context) error,
) {
	h.startTickerLoop(name, interval, false, run)
}

func (h *backgroundLoopHandle) startTickerLoop(
	name string,
	interval time.Duration,
	runImmediately bool,
	run func(context.Context) error,
) {
	h.wg.Go(func() {
		ctx := h.ctx
		runOnce := func() {
			if err := run(ctx); err != nil && ctx.Err() == nil {
				slog.Warn(name+" failed", "err", err)
			}
		}
		if ctx.Err() != nil {
			return
		}
		if runImmediately {
			runOnce()
			if ctx.Err() != nil {
				return
			}
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	})
}
