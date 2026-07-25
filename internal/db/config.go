// Package db provides the Postgres connection pool used by the server, built
// on the pgx/v5 pure-Go driver.
package db

import (
	"errors"
	"fmt"
	"os"
)

// errMissingDatabaseURL is returned when DATABASE_URL is unset or empty.
var errMissingDatabaseURL = errors.New("DATABASE_URL is required")

// errMissingEncryptionKey is returned when LOAM_ENCRYPTION_KEY is unset or empty.
var errMissingEncryptionKey = errors.New("LOAM_ENCRYPTION_KEY is required")

// Config holds the database-layer settings read from the environment: the
// Postgres DSN and the app-level secret-encryption key. Length and encoding
// validation of the encryption key is the responsibility of its consumer
// (the AES-GCM encryptor), not this package.
type Config struct {
	// DatabaseURL is the Postgres connection string (DATABASE_URL).
	DatabaseURL string
	// EncryptionKey is the app-level secret-encryption key (LOAM_ENCRYPTION_KEY).
	EncryptionKey string
}

// LoadConfig reads Config from the environment, requiring both DATABASE_URL
// and LOAM_ENCRYPTION_KEY to be set to a non-empty value.
func LoadConfig() (Config, error) {
	return loadConfig(os.Getenv)
}

// loadConfig builds a Config using getenv as the environment source, so tests
// can supply a fake lookup instead of mutating process-wide environment
// variables.
func loadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{DatabaseURL: getenv("DATABASE_URL"), EncryptionKey: getenv("LOAM_ENCRYPTION_KEY")}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("loading config: %w", errMissingDatabaseURL)
	}
	if cfg.EncryptionKey == "" {
		return Config{}, fmt.Errorf("loading config: %w", errMissingEncryptionKey)
	}
	return cfg, nil
}
