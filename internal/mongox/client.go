// Package mongox owns the MongoDB client lifecycle and index management.
//
// Unlike the TypeScript backend, which memoised a connect promise and awaited
// it in middleware on every DB-backed route to survive Vercel cold starts, this
// process holds a single pooled client for its lifetime. There is nothing to
// await per request.
package mongox

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Collection names, matching the existing database exactly.
const (
	CollCamnections     = "camnections"
	CollWordle          = "wordle"
	CollWatcherSessions = "watcher_sessions"
	CollBingoBoards     = "bingo_boards"
)

// DB wraps the connected client and the target database.
type DB struct {
	Client   *mongo.Client
	Database *mongo.Database
}

// Connect dials MongoDB and verifies reachability with a ping. Pool sizing and
// timeouts are set explicitly - the TypeScript version left every one of them
// at the driver default because a serverless function never reused a pool.
func Connect(ctx context.Context, uri, database string) (*DB, error) {
	opts := options.Client().
		ApplyURI(uri).
		SetAppName("bored-backend-go").
		SetMaxPoolSize(50).
		SetMinPoolSize(2).
		SetMaxConnIdleTime(5 * time.Minute).
		SetServerSelectionTimeout(10 * time.Second)

	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("connect to mongo: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("ping mongo: %w", err)
	}

	return &DB{Client: client, Database: client.Database(database)}, nil
}

// Collection returns a handle to a named collection.
func (db *DB) Collection(name string) *mongo.Collection {
	return db.Database.Collection(name)
}

// Close disconnects the client, draining the pool.
func (db *DB) Close(ctx context.Context) error {
	return db.Client.Disconnect(ctx)
}

// EnsureIndexes creates every index the application depends on. It is
// idempotent, so the server runs it at startup; cmd/ensureindexes exposes it as
// a standalone ops command. This replaces scripts/ensure*Indexes.ts.
//
// Index names are left to MongoDB rather than set explicitly. The existing
// indexes were created unnamed by the TypeScript scripts, so the server auto-
// generated "code_1" and "expiresAt_1"; supplying a different name makes this
// call fail with IndexOptionsConflict against the live database.
//
// The TTL indexes do not make the application-level expiresAt checks redundant:
// MongoDB's TTL monitor only runs about once a minute, so an expired document
// stays readable in between sweeps.
func (db *DB) EnsureIndexes(ctx context.Context) error {
	sessions := db.Collection(CollWatcherSessions)
	if _, err := sessions.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "code", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{
			Keys:    bson.D{{Key: "expiresAt", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(0),
		},
	}); err != nil {
		return fmt.Errorf("ensure %s indexes: %w", CollWatcherSessions, err)
	}

	boards := db.Collection(CollBingoBoards)
	if _, err := boards.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "expiresAt", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0),
	}); err != nil {
		return fmt.Errorf("ensure %s indexes: %w", CollBingoBoards, err)
	}

	return nil
}
