// Command ensureindexes creates the MongoDB indexes the API depends on.
//
// The server does this at startup too, so this exists for ops: running it
// against a fresh database, or re-checking one, without booting the service.
// It replaces scripts/ensureWatcherIndexes.ts and scripts/ensureBingoIndexes.ts.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/camkerber/bored-backend-go/internal/config"
	"github.com/camkerber/bored-backend-go/internal/mongox"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("failed to ensure indexes", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(".env"); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, err := mongox.Connect(ctx, cfg.MongoURI, config.DatabaseName)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close(context.Background()) }()

	if err := db.EnsureIndexes(ctx); err != nil {
		return err
	}

	slog.Info("indexes ensured",
		"watcher_sessions", "{code:1 unique} {expiresAt:1 ttl}",
		"bingo_boards", "{expiresAt:1 ttl}")
	return nil
}
