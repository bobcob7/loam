package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// lookupDefault returns the value of the named environment variable, or def
// if it is unset or empty.
func lookupDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// lookupRequired returns the value of the named environment variable, or an
// error wrapping errMissingEnv if it is unset or empty. The message
// distinguishes the two cases for diagnosability: an unset variable and a
// secretRef that resolved to an empty string look identical to an operator
// unless the error says which one happened.
func lookupRequired(key string) (string, error) {
	v, ok := os.LookupEnv(key)
	if ok && v == "" {
		return "", fmt.Errorf("%s: %w (set but empty)", key, errMissingEnv)
	}
	if !ok {
		return "", fmt.Errorf("%s: %w (not set)", key, errMissingEnv)
	}
	return v, nil
}

// parseEncryptionKey decodes a base64-encoded AES-GCM key and validates it
// is exactly 32 bytes.
func parseEncryptionKey(raw string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("LOAM_ENCRYPTION_KEY: %w: %w", errInvalidEncryptionKey, err)
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("LOAM_ENCRYPTION_KEY: %w: got %d decoded bytes, want 32", errInvalidEncryptionKey, len(decoded))
	}
	return decoded, nil
}

// parseDurationEnv parses the named environment variable as a Go duration,
// or returns def if it is unset or empty.
func parseDurationEnv(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w: %w", key, errInvalidDuration, err)
	}
	return d, nil
}

// parseBoolEnv parses the named environment variable as a bool, or returns
// def if it is unset or empty.
func parseBoolEnv(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w: %w", key, errInvalidBool, err)
	}
	return b, nil
}

// parseIntEnv parses the named environment variable as an int, or returns
// def if it is unset or empty.
func parseIntEnv(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w: %w", key, errInvalidInt, err)
	}
	return n, nil
}

// parseLogLevel maps the LOAM_LOG_LEVEL string to a slog.Level.
func parseLogLevel(v string) (slog.Level, error) {
	switch strings.ToLower(v) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOAM_LOG_LEVEL: %w: %s", errInvalidLogLevel, v)
	}
}

// validateDatabaseURL parses the DSN string and checks it looks like a
// Postgres connection string, in either of pgx's two accepted forms: a URL
// (postgres://... or postgresql://...) or libpq keyword/value pairs
// (host=... user=... dbname=...), which carry no scheme at all. It never
// opens a connection — reachability is validated later in the startup
// sequence.
func validateDatabaseURL(raw string) error {
	if !strings.Contains(raw, "://") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("LOAM_DATABASE_URL: %w: %w", errInvalidDatabaseURL, err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("LOAM_DATABASE_URL: %w: scheme must be postgres or postgresql, got %q", errInvalidDatabaseURL, u.Scheme)
	}
	return nil
}

// checkDataDirWritable probes dir for writability by creating and removing a
// temp file inside it, creating dir first (mode 0o700, since mirrors under it
// hold private repo content) if it does not yet exist.
func checkDataDirWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("LOAM_DATA_DIR: %w: %w", errDataDirNotWritable, err)
	}
	f, err := os.CreateTemp(dir, ".loam-write-check-*")
	if err != nil {
		return fmt.Errorf("LOAM_DATA_DIR: %w: %w", errDataDirNotWritable, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("LOAM_DATA_DIR: %w: %w", errDataDirNotWritable, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("LOAM_DATA_DIR: %w: %w", errDataDirNotWritable, err)
	}
	return nil
}
