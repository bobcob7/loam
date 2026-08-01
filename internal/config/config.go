// Package config loads and validates the Loam server's environment-variable
// configuration surface described in docs/server-spec.md. Load never panics
// and never calls os.Exit; that decision belongs to cmd/server/main.go, which
// keeps the loader itself testable.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"
)

// maxIngestWorkers bounds LOAM_INGEST_WORKERS from above. Unlike the sync
// scheduler's per-cycle git subprocess pair (cmd/server's
// defaultMaxConcurrentCycles doc comment), an ingest worker is just a
// goroutine plus a pgx connection borrowed from the shared pool for the
// duration of a claim/run cycle -- there is no per-worker OS resource
// (file descriptor, subprocess) that makes a large count immediately
// dangerous. The ceiling here exists purely to catch an operator's typo
// (an extra zero, a pasted port number) rather than to protect a hard OS
// limit: pgx pool's own default MaxConns is max(4, NumCPU), so any
// setting above a few dozen already outstrips available DB connections
// and just queues on Acquire, and 256 leaves generous headroom above that
// for unusually large machines while still refusing a five- or six-digit
// value outright.
const maxIngestWorkers = 256

// Config holds the fully validated server configuration decoded from the
// LOAM_* environment variables in docs/server-spec.md.
type Config struct {
	HTTPAddr      string
	AdminUser     string
	AdminPassword string
	DatabaseURL   string
	DataDir       string
	EncryptionKey []byte
	SyncInterval  time.Duration
	PRAttribution bool
	EmbedderURL   string
	EmbedderModel string
	IngestWorkers int
	LogLevel      slog.Level
	Logger        *slog.Logger
}

// Load reads and validates every LOAM_* environment variable, applying the
// documented defaults where a variable is optional. It fails fast, returning
// on the first problem it finds: a missing required variable, an invalid
// value, or an unwritable data directory. The only filesystem write Load
// performs is creating LOAM_DATA_DIR (mode 0o700) if it does not already
// exist, as an unavoidable part of probing it for writability; that probe
// runs last, after every other variable has validated, so a config that is
// ultimately rejected never touches the filesystem.
func Load() (Config, error) {
	var cfg Config
	if err := loadRequired(&cfg); err != nil {
		return Config{}, err
	}
	if err := loadOptional(&cfg); err != nil {
		return Config{}, err
	}
	cfg.Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	return cfg, nil
}

// loadRequired populates and validates the environment variables that have
// no default: LOAM_ADMIN_PASSWORD, LOAM_ENCRYPTION_KEY, and the Postgres DSN
// (LOAM_DATABASE_URL directly, or assembled from the discrete LOAM_DB_*
// parts -- see resolveDatabaseURL's doc comment for the precedence rule).
func loadRequired(cfg *Config) error {
	adminPassword, err := lookupRequired("LOAM_ADMIN_PASSWORD")
	if err != nil {
		return err
	}
	cfg.AdminPassword = adminPassword
	databaseURL, err := resolveDatabaseURL()
	if err != nil {
		return err
	}
	cfg.DatabaseURL = databaseURL
	encryptionKeyRaw, err := lookupRequired("LOAM_ENCRYPTION_KEY")
	if err != nil {
		return err
	}
	encryptionKey, err := parseEncryptionKey(encryptionKeyRaw)
	if err != nil {
		return err
	}
	cfg.EncryptionKey = encryptionKey
	return nil
}

// loadOptional populates and validates the environment variables that carry
// a default value from the server-spec table.
func loadOptional(cfg *Config) error {
	cfg.HTTPAddr = lookupDefault("LOAM_HTTP_ADDR", ":8080")
	cfg.AdminUser = lookupDefault("LOAM_ADMIN_USER", "admin")
	cfg.DataDir = lookupDefault("LOAM_DATA_DIR", "/var/lib/loam")
	syncInterval, err := parseDurationEnv("LOAM_SYNC_INTERVAL", 60*time.Second)
	if err != nil {
		return err
	}
	if syncInterval <= 0 {
		return fmt.Errorf("LOAM_SYNC_INTERVAL: %w: got %s, want greater than zero", errSyncIntervalRange, syncInterval)
	}
	cfg.SyncInterval = syncInterval
	prAttribution, err := parseBoolEnv("LOAM_PR_ATTRIBUTION", true)
	if err != nil {
		return err
	}
	cfg.PRAttribution = prAttribution
	cfg.EmbedderURL = lookupDefault("LOAM_EMBEDDER_URL", "http://localhost:11434")
	cfg.EmbedderModel = lookupDefault("LOAM_EMBEDDER_MODEL", "nomic-embed-text")
	ingestWorkers, err := parseIntEnv("LOAM_INGEST_WORKERS", 2)
	if err != nil {
		return err
	}
	if ingestWorkers < 1 || ingestWorkers > maxIngestWorkers {
		return fmt.Errorf("LOAM_INGEST_WORKERS: %w: got %d, want between 1 and %d", errIngestWorkersRange, ingestWorkers, maxIngestWorkers)
	}
	cfg.IngestWorkers = ingestWorkers
	logLevel, err := parseLogLevel(lookupDefault("LOAM_LOG_LEVEL", "info"))
	if err != nil {
		return err
	}
	cfg.LogLevel = logLevel
	if err := checkDataDirWritable(cfg.DataDir); err != nil {
		return err
	}
	return nil
}
