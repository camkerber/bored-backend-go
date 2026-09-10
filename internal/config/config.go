// Package config loads and validates process configuration from the
// environment, following 12-factor conventions.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DatabaseName is fixed, matching DATABASE_NAME in the TypeScript backend. Both
// services run against the same database during the migration cutover.
const DatabaseName = "boring-db"

// Config is the fully resolved configuration for a server process.
type Config struct {
	// MongoURI is the Atlas connection string. Required.
	MongoURI string
	// Port is the TCP port to listen on. Defaults to 3000.
	Port int
	// CORSOrigins are extra allowed origins, unioned with the baseline list.
	CORSOrigins []string
	// CronSecret gates the /cron/cleanup routes. When empty those routes
	// reject every request with CRON_NOT_CONFIGURED.
	CronSecret string
	// CleanupInterval controls the in-process expiry sweep.
	CleanupInterval string
}

// Load reads configuration from the environment and validates it. It fails
// fast: a missing Mongo URI is a startup error, not a per-request surprise.
func Load() (Config, error) {
	cfg := Config{
		MongoURI:        os.Getenv("BORED_MONGODB_URI"),
		CronSecret:      os.Getenv("CRON_SECRET"),
		CleanupInterval: envOr("CLEANUP_INTERVAL", "1h"),
	}

	if cfg.MongoURI == "" {
		return Config{}, errors.New("BORED_MONGODB_URI is not set")
	}

	port := envOr("PORT", "3000")
	parsed, err := strconv.Atoi(port)
	if err != nil {
		return Config{}, fmt.Errorf("PORT %q is not a number: %w", port, err)
	}
	cfg.Port = parsed

	for _, origin := range strings.Split(os.Getenv("CORS_ORIGINS"), ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			cfg.CORSOrigins = append(cfg.CORSOrigins, origin)
		}
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// LoadDotEnv reads a .env file into the process environment if one exists,
// replacing the dotenv dependency. Existing environment variables always win,
// and a missing file is not an error - in a container there is none.
func LoadDotEnv(path string) error {
	// The path is supplied by our own entrypoints, never by request input.
	file, err := os.Open(path) //nolint:gosec // G304: caller-controlled, not user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
