package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validEncryptionKey returns a distinctive (non-zero) 32-byte key so tests
// can assert the decoded value round-trips through Load, rather than a
// value that would coincidentally match a hardcoded-zero mutant.
func validEncryptionKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// baseEnv sets every required LOAM_* variable to a value that passes
// validation, points LOAM_DATA_DIR at dataDir, and explicitly blanks every
// optional variable so the test is hermetic: it must not depend on (or be
// broken by) whatever LOAM_* variables happen to be exported in the
// developer's or CI's ambient environment. Callers override individual
// variables afterward to exercise specific values or failure paths.
//
// NOTE: this helper (and every test below) uses t.Setenv, which the testing
// package forbids combining with t.Parallel() since environment variables
// are process-global. None of the tests in this file call t.Parallel().
func baseEnv(t *testing.T, dataDir string) {
	t.Helper()
	t.Setenv("LOAM_ADMIN_PASSWORD", "hunter2")
	t.Setenv("LOAM_DATABASE_URL", "postgres://user:pass@localhost:5432/loam")
	t.Setenv("LOAM_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(validEncryptionKey()))
	t.Setenv("LOAM_DATA_DIR", dataDir)
	t.Setenv("LOAM_DB_HOST", "")
	t.Setenv("LOAM_DB_PORT", "")
	t.Setenv("LOAM_DB_USER", "")
	t.Setenv("LOAM_DB_PASSWORD", "")
	t.Setenv("LOAM_DB_NAME", "")
	t.Setenv("LOAM_DB_SSLMODE", "")
	t.Setenv("LOAM_HTTP_ADDR", "")
	t.Setenv("LOAM_ADMIN_USER", "")
	t.Setenv("LOAM_SYNC_INTERVAL", "")
	t.Setenv("LOAM_PR_ATTRIBUTION", "")
	t.Setenv("LOAM_EMBEDDER_URL", "")
	t.Setenv("LOAM_EMBEDDER_MODEL", "")
	t.Setenv("LOAM_INGEST_WORKERS", "")
	t.Setenv("LOAM_LOG_LEVEL", "")
}

func TestLoad_Defaults(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	//
	// LOAM_DATA_DIR is overridden to a writable t.TempDir() rather than left
	// at its documented default (/var/lib/loam) because the test process
	// cannot write there; every other field below asserts its literal
	// spec-table default.
	dataDir := t.TempDir()
	baseEnv(t, dataDir)
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, "admin", cfg.AdminUser)
	assert.Equal(t, "hunter2", cfg.AdminPassword)
	assert.Equal(t, validEncryptionKey(), cfg.EncryptionKey)
	assert.Equal(t, dataDir, cfg.DataDir)
	assert.Equal(t, 60*time.Second, cfg.SyncInterval)
	assert.True(t, cfg.PRAttribution)
	assert.Equal(t, "http://localhost:11434", cfg.EmbedderURL)
	assert.Equal(t, "nomic-embed-text", cfg.EmbedderModel)
	assert.Equal(t, 2, cfg.IngestWorkers)
	assert.Equal(t, slog.LevelInfo, cfg.LogLevel)
	require.NotNil(t, cfg.Logger)
	assert.False(t, cfg.Logger.Enabled(t.Context(), slog.LevelDebug), "default level info should not enable debug logging")
	assert.True(t, cfg.Logger.Enabled(t.Context(), slog.LevelInfo), "default level info should enable info logging")
	entries, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the writability probe must clean up its temp file")
}

func TestLoad_Overrides(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	dataDir := t.TempDir()
	baseEnv(t, dataDir)
	overrideDir := t.TempDir()
	t.Setenv("LOAM_HTTP_ADDR", "127.0.0.1:9090")
	t.Setenv("LOAM_ADMIN_USER", "root")
	t.Setenv("LOAM_DATA_DIR", overrideDir)
	t.Setenv("LOAM_SYNC_INTERVAL", "5m")
	t.Setenv("LOAM_PR_ATTRIBUTION", "false")
	t.Setenv("LOAM_EMBEDDER_URL", "http://embedder.internal:9999")
	t.Setenv("LOAM_EMBEDDER_MODEL", "custom-model")
	t.Setenv("LOAM_INGEST_WORKERS", "7")
	t.Setenv("LOAM_LOG_LEVEL", "debug")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9090", cfg.HTTPAddr)
	assert.Equal(t, "root", cfg.AdminUser)
	assert.Equal(t, overrideDir, cfg.DataDir)
	assert.Equal(t, 5*time.Minute, cfg.SyncInterval)
	assert.False(t, cfg.PRAttribution)
	assert.Equal(t, "http://embedder.internal:9999", cfg.EmbedderURL)
	assert.Equal(t, "custom-model", cfg.EmbedderModel)
	assert.Equal(t, 7, cfg.IngestWorkers)
	assert.Equal(t, slog.LevelDebug, cfg.LogLevel)
	require.NotNil(t, cfg.Logger)
	assert.True(t, cfg.Logger.Enabled(t.Context(), slog.LevelDebug), "LOAM_LOG_LEVEL=debug should enable debug logging")
}

func TestLoad_DatabaseURLKeywordValueForm(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	dataDir := t.TempDir()
	baseEnv(t, dataDir)
	const dsn = "host=localhost user=loam dbname=loam sslmode=disable"
	t.Setenv("LOAM_DATABASE_URL", dsn)
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, dsn, cfg.DatabaseURL)
}

// TestLoad_DatabaseURLFromParts covers loam-ytt2.11: when LOAM_DATABASE_URL
// is unset, Load assembles a DSN from the discrete LOAM_DB_* parts instead
// -- the shape a Kubernetes manifest needs so one POSTGRES_PASSWORD value
// feeds both the postgres image and loam.
func TestLoad_DatabaseURLFromParts(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	dataDir := t.TempDir()
	baseEnv(t, dataDir)
	t.Setenv("LOAM_DATABASE_URL", "")
	t.Setenv("LOAM_DB_HOST", "postgres.loam.svc.cluster.local")
	t.Setenv("LOAM_DB_USER", "loam")
	t.Setenv("LOAM_DB_PASSWORD", "hunter2")
	t.Setenv("LOAM_DB_NAME", "loam")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://loam:hunter2@postgres.loam.svc.cluster.local:5432/loam?sslmode=disable", cfg.DatabaseURL)
}

// TestLoad_DatabaseURLPartsDefaults asserts LOAM_DB_PORT and
// LOAM_DB_SSLMODE take their documented defaults (5432, disable) when only
// the required parts are set.
func TestLoad_DatabaseURLPartsDefaults(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	dataDir := t.TempDir()
	baseEnv(t, dataDir)
	t.Setenv("LOAM_DATABASE_URL", "")
	t.Setenv("LOAM_DB_HOST", "db")
	t.Setenv("LOAM_DB_USER", "u")
	t.Setenv("LOAM_DB_PASSWORD", "p")
	t.Setenv("LOAM_DB_NAME", "n")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://u:p@db:5432/n?sslmode=disable", cfg.DatabaseURL)
}

// TestLoad_DatabaseURLPartsOverridePortAndSSLMode asserts LOAM_DB_PORT and
// LOAM_DB_SSLMODE, when set, override their defaults in the assembled DSN.
func TestLoad_DatabaseURLPartsOverridePortAndSSLMode(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	dataDir := t.TempDir()
	baseEnv(t, dataDir)
	t.Setenv("LOAM_DATABASE_URL", "")
	t.Setenv("LOAM_DB_HOST", "db")
	t.Setenv("LOAM_DB_PORT", "6543")
	t.Setenv("LOAM_DB_USER", "u")
	t.Setenv("LOAM_DB_PASSWORD", "p")
	t.Setenv("LOAM_DB_NAME", "n")
	t.Setenv("LOAM_DB_SSLMODE", "require")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://u:p@db:6543/n?sslmode=require", cfg.DatabaseURL)
}

// TestLoad_DatabaseURLPartsPasswordNeedsEncoding is the single most likely
// defect this feature can have: POSTGRES_PASSWORD legally contains '/',
// '@', ':', and '+', all of which are DSN-structural characters. A naive
// fmt.Sprintf-built DSN would corrupt on any of them; url.UserPassword must
// percent-encode the userinfo so the assembled DSN round-trips back to the
// exact original password through url.Parse.
func TestLoad_DatabaseURLPartsPasswordNeedsEncoding(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	dataDir := t.TempDir()
	baseEnv(t, dataDir)
	const rawPassword = `p@ss/w:o+rd`
	t.Setenv("LOAM_DATABASE_URL", "")
	t.Setenv("LOAM_DB_HOST", "db")
	t.Setenv("LOAM_DB_USER", "u")
	t.Setenv("LOAM_DB_PASSWORD", rawPassword)
	t.Setenv("LOAM_DB_NAME", "n")
	cfg, err := Load()
	require.NoError(t, err)
	parsed, err := url.Parse(cfg.DatabaseURL)
	require.NoError(t, err)
	got, ok := parsed.User.Password()
	require.True(t, ok, "assembled DSN must carry a password")
	assert.Equal(t, rawPassword, got, "password must round-trip through the assembled DSN unchanged")
}

// TestLoad_DatabaseURLBothFormsIsConflict covers the precedence decision
// documented on resolveDatabaseURL: setting LOAM_DATABASE_URL AND any
// LOAM_DB_* part is rejected outright rather than silently picking one --
// silently ignoring half a config an operator actually set is its own
// footgun.
func TestLoad_DatabaseURLBothFormsIsConflict(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	dataDir := t.TempDir()
	baseEnv(t, dataDir)
	t.Setenv("LOAM_DB_HOST", "db")
	_, err := Load()
	require.ErrorIs(t, err, errDatabaseConfigConflict)
}

// TestLoad_DatabaseURLPartsMissingRequired table-drives every required
// LOAM_DB_* part (HOST, USER, PASSWORD, NAME) in isolation, confirming each
// one's absence fails Load with errMissingEnv even though the other three
// are present -- i.e. none of the four is silently optional once the parts
// form is in use.
func TestLoad_DatabaseURLPartsMissingRequired(t *testing.T) {
	// Not parallel (nor are the subtests below): t.Setenv is incompatible
	// with t.Parallel.
	dataDir := t.TempDir()
	fullParts := map[string]string{
		"LOAM_DB_HOST":     "db",
		"LOAM_DB_USER":     "u",
		"LOAM_DB_PASSWORD": "p",
		"LOAM_DB_NAME":     "n",
	}
	for _, missing := range []string{"LOAM_DB_HOST", "LOAM_DB_USER", "LOAM_DB_PASSWORD", "LOAM_DB_NAME"} {
		t.Run("missing "+missing, func(t *testing.T) {
			baseEnv(t, dataDir)
			t.Setenv("LOAM_DATABASE_URL", "")
			for k, v := range fullParts {
				if k == missing {
					continue
				}
				t.Setenv(k, v)
			}
			_, err := Load()
			require.ErrorIs(t, err, errMissingEnv)
		})
	}
}

func TestLoad_DataDirCreatedIfMissing(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "nested", "does-not-exist-yet")
	baseEnv(t, dataDir)
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, dataDir, cfg.DataDir)
	info, err := os.Stat(dataDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	entries, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the writability probe must clean up its temp file")
}

func TestLoad_InvalidInput(t *testing.T) {
	// Not parallel (nor are the subtests below): t.Setenv is incompatible
	// with t.Parallel.
	dataDir := t.TempDir()
	regularFile, err := os.CreateTemp(t.TempDir(), "loam-data-dir-is-a-file-*")
	require.NoError(t, err)
	require.NoError(t, regularFile.Close())
	tests := []struct {
		name     string
		override map[string]string
		wantErr  error
	}{
		{name: "missing admin password", override: map[string]string{"LOAM_ADMIN_PASSWORD": ""}, wantErr: errMissingEnv},
		{name: "missing database url", override: map[string]string{"LOAM_DATABASE_URL": ""}, wantErr: errMissingEnv},
		{name: "missing encryption key", override: map[string]string{"LOAM_ENCRYPTION_KEY": ""}, wantErr: errMissingEnv},
		{name: "encryption key bad base64", override: map[string]string{"LOAM_ENCRYPTION_KEY": "not-valid-base64!!"}, wantErr: errInvalidEncryptionKey},
		{name: "encryption key wrong length", override: map[string]string{"LOAM_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 16))}, wantErr: errInvalidEncryptionKey},
		{name: "unparseable sync interval", override: map[string]string{"LOAM_SYNC_INTERVAL": "not-a-duration"}, wantErr: errInvalidDuration},
		{name: "zero sync interval", override: map[string]string{"LOAM_SYNC_INTERVAL": "0s"}, wantErr: errSyncIntervalRange},
		{name: "negative sync interval", override: map[string]string{"LOAM_SYNC_INTERVAL": "-5m"}, wantErr: errSyncIntervalRange},
		{name: "invalid pr attribution bool", override: map[string]string{"LOAM_PR_ATTRIBUTION": "maybe"}, wantErr: errInvalidBool},
		{name: "invalid ingest workers int", override: map[string]string{"LOAM_INGEST_WORKERS": "three"}, wantErr: errInvalidInt},
		{name: "zero ingest workers", override: map[string]string{"LOAM_INGEST_WORKERS": "0"}, wantErr: errIngestWorkersRange},
		{name: "negative ingest workers", override: map[string]string{"LOAM_INGEST_WORKERS": "-1"}, wantErr: errIngestWorkersRange},
		{name: "ingest workers above max", override: map[string]string{"LOAM_INGEST_WORKERS": "257"}, wantErr: errIngestWorkersRange},
		{name: "invalid log level", override: map[string]string{"LOAM_LOG_LEVEL": "verbose"}, wantErr: errInvalidLogLevel},
		{name: "invalid database url scheme", override: map[string]string{"LOAM_DATABASE_URL": "mysql://localhost/loam"}, wantErr: errInvalidDatabaseURL},
		{name: "data dir is a regular file", override: map[string]string{"LOAM_DATA_DIR": regularFile.Name()}, wantErr: errDataDirNotWritable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t, dataDir)
			for k, v := range tc.override {
				t.Setenv(k, v)
			}
			_, err := Load()
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestLoad_SyncIntervalAndIngestWorkersBoundaries exercises the range
// checks loam-35b added at the values right at and around their edges: 0,
// 1, the documented upper bound, and one past it. It also asserts on the
// error message content -- an operator who mistypes one of these two
// variables needs to learn what they set and what the valid range is, not
// just that Load failed.
func TestLoad_SyncIntervalAndIngestWorkersBoundaries(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	dataDir := t.TempDir()
	tests := []struct {
		name        string
		key         string
		value       string
		wantErr     error
		wantErrText string
	}{
		{name: "sync interval exactly zero is rejected", key: "LOAM_SYNC_INTERVAL", value: "0s", wantErr: errSyncIntervalRange, wantErrText: "LOAM_SYNC_INTERVAL: sync interval must be positive: got 0s, want greater than zero"},
		{name: "sync interval negative is rejected", key: "LOAM_SYNC_INTERVAL", value: "-1s", wantErr: errSyncIntervalRange, wantErrText: "LOAM_SYNC_INTERVAL: sync interval must be positive: got -1s, want greater than zero"},
		{name: "sync interval of 1ns is accepted", key: "LOAM_SYNC_INTERVAL", value: "1ns"},
		{name: "ingest workers of zero is rejected", key: "LOAM_INGEST_WORKERS", value: "0", wantErr: errIngestWorkersRange, wantErrText: "LOAM_INGEST_WORKERS: ingest workers out of range: got 0, want between 1 and 256"},
		{name: "ingest workers negative is rejected", key: "LOAM_INGEST_WORKERS", value: "-1", wantErr: errIngestWorkersRange, wantErrText: "LOAM_INGEST_WORKERS: ingest workers out of range: got -1, want between 1 and 256"},
		{name: "ingest workers of 1 is accepted", key: "LOAM_INGEST_WORKERS", value: "1"},
		{name: "ingest workers at the max of 256 is accepted", key: "LOAM_INGEST_WORKERS", value: "256"},
		{name: "ingest workers one past the max is rejected", key: "LOAM_INGEST_WORKERS", value: "257", wantErr: errIngestWorkersRange, wantErrText: "LOAM_INGEST_WORKERS: ingest workers out of range: got 257, want between 1 and 256"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t, dataDir)
			t.Setenv(tc.key, tc.value)
			cfg, err := Load()
			if tc.wantErr == nil {
				require.NoError(t, err)
				if tc.key == "LOAM_SYNC_INTERVAL" {
					assert.Equal(t, time.Nanosecond, cfg.SyncInterval)
				}
				if tc.key == "LOAM_INGEST_WORKERS" {
					assert.Contains(t, []int{1, 256}, cfg.IngestWorkers)
				}
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			assert.EqualError(t, err, tc.wantErrText)
		})
	}
}

func TestLoad_UnwritableDataDir(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "readonly")
	require.NoError(t, os.Mkdir(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	baseEnv(t, dir)
	_, err := Load()
	require.ErrorIs(t, err, errDataDirNotWritable)
}

// TestLoad_UnwritableDataDirErrorNamesUIDAndPath pins the diagnosability
// half of the failure above, which the sentinel assertion alone does not
// reach. This is the first-run failure a compose or Kubernetes operator is
// most likely to hit -- a bind mount or a volume whose ownership does not
// match the image's uid 10001 (deploy/docker-compose.yml's loam-data
// comment, helm/loam's fsGroup) -- and "permission denied" without the uid
// asking for permission is what makes it a mystery rather than a one-line
// fix. Asserting the uid, the gid and the probed path together is the
// contract; without all three the message is back to being unactionable.
func TestLoad_UnwritableDataDirErrorNamesUIDAndPath(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "readonly")
	require.NoError(t, os.Mkdir(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	baseEnv(t, dir)
	_, err := Load()
	require.ErrorIs(t, err, errDataDirNotWritable)
	assert.Contains(t, err.Error(), dir, "the operator cannot act on a permission error that does not name the directory")
	assert.Contains(t, err.Error(), fmt.Sprintf("uid %d", os.Getuid()),
		"the actionable fact is which uid was denied, not merely that something was")
	assert.Contains(t, err.Error(), fmt.Sprintf("gid %d", os.Getgid()))
	assert.Contains(t, err.Error(), fmt.Sprintf("chown -R %d:%d", os.Getuid(), os.Getgid()),
		"the message should carry the fix, not only the diagnosis")
}
