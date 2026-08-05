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

// parseFloatEnv parses the named environment variable as a float64, or
// returns def if it is unset or empty. Note for callers: strconv.ParseFloat
// ACCEPTS "NaN", "Inf", and "-Inf", so a range check of the returned value
// must use math.IsNaN rather than relying on comparisons -- every ordered
// comparison against NaN is false, so `v < lo || v > hi` waves it straight
// through.
func parseFloatEnv(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w: %w", key, errInvalidFloat, err)
	}
	return f, nil
}

// validateOTelEndpoint checks LOAM_OTEL_ENDPOINT looks like the OTLP/HTTP
// base URL internal/telemetry hands to WithEndpointURL: an absolute http or
// https URL with a host. It never dials -- an unreachable collector must
// degrade to dropped telemetry, not to a server that refuses to boot (see
// internal/telemetry.New's doc comment) -- so this catches only the errors
// that are decidable locally.
//
// The scheme is load-bearing, not decoration: it is what selects TLS for the
// exporter, which is why this variable is a full URL rather than the bare
// host:port the OTEL_EXPORTER_OTLP_ENDPOINT convention also permits. A bare
// "otel-collector:4318" parses as a URL with scheme "otel-collector" and no
// host, and is rejected here with a message that says what to write instead
// -- rather than being accepted and silently exporting nowhere.
func validateOTelEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("LOAM_OTEL_ENDPOINT: %w: %w", errInvalidOTelEndpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("LOAM_OTEL_ENDPOINT: %w: want an absolute http:// or https:// URL such as http://otel-collector:4318, got %q", errInvalidOTelEndpoint, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("LOAM_OTEL_ENDPOINT: %w: no host in %q", errInvalidOTelEndpoint, raw)
	}
	return nil
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
		return dataDirError(dir, err)
	}
	f, err := os.CreateTemp(dir, ".loam-write-check-*")
	if err != nil {
		return dataDirError(dir, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return dataDirError(dir, err)
	}
	if err := os.Remove(name); err != nil {
		return dataDirError(dir, err)
	}
	return nil
}

// dataDirError wraps a failed LOAM_DATA_DIR probe with the two facts an
// operator needs in order to act on it in one step: the directory that was
// probed, and the uid/gid this process is actually running as.
//
// The pairing is the whole point. The underlying os error names the path
// and says "permission denied", which is true and not actionable -- the
// question it leaves open is "denied to whom", and the answer is rarely the
// user reading the log. This is the single most likely first-run failure
// for the container image, which runs as uid/gid 10001 (Dockerfile):
// deploy/docker-compose.yml's named volume is seeded from the image with
// that ownership and works, but the moment someone swaps in a bind mount to
// see the mirrors on their host, Docker creates a root-owned directory,
// chowns nothing, and the server crashloops here. Kubernetes has fsGroup
// (helm/loam sets 10001) for exactly this; compose has no equivalent, so
// the error message is the only thing standing between the operator and a
// mystery. Naming the uid turns it into a one-line diagnosis and points
// directly at the chown that fixes it.
//
// It deliberately does NOT stat dir to report its current owner, tempting
// though that is: fs.FileInfo.Sys()'s concrete type is platform-specific,
// so reading it would trade a portable package for one more fact the
// operator can get themselves with `ls -ldn`.
func dataDirError(dir string, err error) error {
	uid, gid := os.Getuid(), os.Getgid()
	return fmt.Errorf(
		"LOAM_DATA_DIR: %w: %s: this process runs as uid %d gid %d and must be able to write there (a bind-mounted host directory is not chowned for you -- `chown -R %d:%d <host path>` before first start): %w",
		errDataDirNotWritable, dir, uid, gid, uid, gid, err)
}
