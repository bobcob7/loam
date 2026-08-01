package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// databaseURLPartKeys lists the discrete LOAM_DB_* variables
// assembleDatabaseURL reads, checked collectively to decide whether the
// discrete form is in use at all.
var databaseURLPartKeys = []string{
	"LOAM_DB_HOST", "LOAM_DB_PORT", "LOAM_DB_USER",
	"LOAM_DB_PASSWORD", "LOAM_DB_NAME", "LOAM_DB_SSLMODE",
}

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

// isEnvSet reports whether key is present in the environment with a
// non-empty value, matching the "unset" convention lookupDefault and the
// parse*Env helpers above already use: not-present and present-but-empty
// are treated the same.
func isEnvSet(key string) bool {
	v, ok := os.LookupEnv(key)
	return ok && v != ""
}

// resolveDatabaseURL determines the Postgres DSN the server needs, either by
// reading LOAM_DATABASE_URL directly or by assembling one from the discrete
// LOAM_DB_* parts, and returns it already checked by validateDatabaseURL.
//
// Precedence: LOAM_DATABASE_URL wins when it is the only one set -- an
// operator pointed at a managed database supplies a DSN and nothing else.
// LOAM_DB_* parts are used when they are the only ones set -- this is what
// lets a Kubernetes manifest pass one POSTGRES_PASSWORD value to both the
// postgres image (which only initializes its superuser from
// POSTGRES_PASSWORD) and loam, instead of also hand-embedding that same
// password into a second, DSN-shaped copy that nothing keeps in sync.
// Setting BOTH is rejected as a conflict rather than silently preferring
// one side: silently ignoring half of a config an operator actually set is
// its own footgun, and a quiet mismatch between the two is exactly the
// misdiagnosable "database unreachable" failure this form exists to
// prevent. Setting NEITHER falls through to lookupRequired's own "not set"
// error on LOAM_DATABASE_URL, unchanged from before this form existed.
func resolveDatabaseURL() (string, error) {
	urlSet := isEnvSet("LOAM_DATABASE_URL")
	partsSet := false
	for _, key := range databaseURLPartKeys {
		if isEnvSet(key) {
			partsSet = true
			break
		}
	}
	if urlSet && partsSet {
		return "", fmt.Errorf("LOAM_DATABASE_URL: %w: set LOAM_DATABASE_URL or the discrete LOAM_DB_* variables, not both", errDatabaseConfigConflict)
	}
	var (
		databaseURL string
		err         error
	)
	if partsSet {
		databaseURL, err = assembleDatabaseURL()
	} else {
		databaseURL, err = lookupRequired("LOAM_DATABASE_URL")
	}
	if err != nil {
		return "", err
	}
	if err := validateDatabaseURL(databaseURL); err != nil {
		return "", err
	}
	return databaseURL, nil
}

// assembleDatabaseURL builds a Postgres DSN from the discrete LOAM_DB_*
// parts. LOAM_DB_HOST, LOAM_DB_USER, LOAM_DB_PASSWORD, and LOAM_DB_NAME are
// required; LOAM_DB_PORT defaults to 5432 and LOAM_DB_SSLMODE to "disable"
// (the in-cluster Postgres addon this form exists for terminates no TLS of
// its own). url.UserPassword percent-encodes the userinfo, so a password
// containing '/', '@', ':', or '+' -- all legal in POSTGRES_PASSWORD -- is
// carried correctly instead of corrupting the DSN the way naive string
// concatenation would. The returned string is never logged or wrapped into
// an error by this function or its caller: it carries the password in
// cleartext.
func assembleDatabaseURL() (string, error) {
	host, err := lookupRequired("LOAM_DB_HOST")
	if err != nil {
		return "", err
	}
	user, err := lookupRequired("LOAM_DB_USER")
	if err != nil {
		return "", err
	}
	password, err := lookupRequired("LOAM_DB_PASSWORD")
	if err != nil {
		return "", err
	}
	name, err := lookupRequired("LOAM_DB_NAME")
	if err != nil {
		return "", err
	}
	port := lookupDefault("LOAM_DB_PORT", "5432")
	sslmode := lookupDefault("LOAM_DB_SSLMODE", "disable")
	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, port),
		Path:     "/" + name,
		RawQuery: url.Values{"sslmode": {sslmode}}.Encode(),
	}
	return dsn.String(), nil
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
