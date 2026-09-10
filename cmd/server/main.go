// Command server runs the bored-backend HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/camkerber/bored-backend-go/internal/config"
	"github.com/camkerber/bored-backend-go/internal/mongox"
	"github.com/camkerber/bored-backend-go/internal/server"
)

// shutdownTimeout bounds how long in-flight requests get to finish.
const shutdownTimeout = 20 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// A .env file is a local convenience; in a container the environment is
	// already populated and there is no file to read.
	if err := config.LoadDotEnv(".env"); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Signals cancel this context, which unwinds the whole process.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := mongox.Connect(ctx, cfg.MongoURI, config.DatabaseName)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.Close(closeCtx); err != nil {
			slog.Error("failed to disconnect from mongo", "error", err)
		}
	}()

	// Idempotent, so it runs on every boot. This replaces the one-shot
	// ensure*Indexes scripts as the primary mechanism.
	if err := db.EnsureIndexes(ctx); err != nil {
		return err
	}
	slog.Info("indexes ensured")

	app := server.New(server.Options{
		DB:          db,
		CORSOrigins: cfg.CORSOrigins,
		CronSecret:  cfg.CronSecret,
	})

	cleanupInterval, err := time.ParseDuration(cfg.CleanupInterval)
	if err != nil {
		return err
	}
	go runCleanup(ctx, app, cleanupInterval)

	srv := &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(cfg.Port)),
		Handler:           app.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout must accommodate SSE streams, which stay open for the
		// life of a session. The streaming handler clears its own write
		// deadline via http.ResponseController, so a bounded value here is
		// safe for ordinary requests.
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return context.Background() },
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
	}

	// Drain in-flight requests before disconnecting Mongo. The Node version
	// called process.exit immediately after closing the client, dropping
	// anything still in flight.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		return err
	}

	slog.Info("shutdown complete")
	return nil
}

// runCleanup sweeps expired watcher sessions and bingo boards on a ticker.
//
// This replaces the two Vercel Cron jobs, which could only run daily on the
// Hobby plan. The HTTP cleanup routes are still exposed for manual triggering.
func runCleanup(ctx context.Context, app *server.App, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if deleted, err := app.Watcher.CleanupExpired(sweepCtx); err != nil {
				slog.Error("watcher cleanup failed", "error", err)
			} else if deleted > 0 {
				slog.Info("watcher cleanup", "deleted", deleted)
			}
			if deleted, err := app.Bingo.CleanupExpired(sweepCtx); err != nil {
				slog.Error("bingo cleanup failed", "error", err)
			} else if deleted > 0 {
				slog.Info("bingo cleanup", "deleted", deleted)
			}
			cancel()
		}
	}
}
