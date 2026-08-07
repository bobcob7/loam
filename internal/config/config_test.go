package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	t.Setenv("LOAM_OTEL_ENDPOINT", "")
	t.Setenv("LOAM_OTEL_SERVICE_NAME", "")
	t.Setenv("LOAM_OTEL_SAMPLE_RATIO", "")
	t.Setenv("LOAM_OTEL_DB_ACQUIRE_THRESHOLD", "")
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
		{name: "unparseable otel sample ratio", override: map[string]string{"LOAM_OTEL_SAMPLE_RATIO": "half"}, wantErr: errInvalidFloat},
		{name: "otel endpoint with no scheme", override: map[string]string{"LOAM_OTEL_ENDPOINT": "otel-collector:4318"}, wantErr: errInvalidOTelEndpoint},
		{name: "otel endpoint with a non-http scheme", override: map[string]string{"LOAM_OTEL_ENDPOINT": "grpc://otel-collector:4317"}, wantErr: errInvalidOTelEndpoint},
		{name: "otel endpoint with no host", override: map[string]string{"LOAM_OTEL_ENDPOINT": "http:///v1/traces"}, wantErr: errInvalidOTelEndpoint},
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

// unwritableDataDirShape is one way of making LOAM_DATA_DIR unusable that
// UID 0 CANNOT BYPASS. plant builds the shape under a fresh parent and
// returns the value to point LOAM_DATA_DIR at; wantErrno is the errno the
// probe must fail with, so a shape that stopped working says which one it
// was rather than "an error is expected but got nil".
type unwritableDataDirShape struct {
	name      string
	plant     func(t *testing.T, parent string) string
	wantErrno syscall.Errno
}

// unwritableDataDirShapes is the shared fixture behind the two tests below.
//
// BOTH USED TO CHMOD A DIRECTORY TO 0o500 AND SKIP UNDER ROOT, WHICH MEANT
// NEITHER HAD EVER RUN IN CI (loam-0nuo). The per-PR gate runs `go test
// ./... -race` inside golang:1.26.5 as uid 0; root holds CAP_DAC_OVERRIDE
// and writes into a 0o500 directory regardless, so `if os.Geteuid() == 0 {
// t.Skip(...) }` was the only thing keeping the gate green -- and a skip and
// a pass are indistinguishable in the summary output CI is read from, so the
// coverage looked present while the branch was never once exercised.
//
// The three shapes here fail for reasons that are not permission checks, so
// uid 0 receives them exactly as an unprivileged user does (the technique is
// loam-9g17's; internal/cli/staging_test.go carries its long-form rationale):
//
//   - ENOTDIR, because a path component is a regular file. os.MkdirAll stats
//     the component, finds a non-directory, and fails.
//   - EEXIST, because the data dir is a dangling symlink. MkdirAll's Mkdir
//     gets EEXIST, its Lstat says the thing that exists is not a directory,
//     and it reports the Mkdir failure rather than treating the path as
//     already-created.
//   - ENAMETOOLONG, because the data dir sits at the deepest path the
//     filesystem will create. This is the one that reaches the SECOND arm of
//     checkDataDirWritable: MkdirAll succeeds (the directory is already
//     there), and the temp-file write probe inside it is what fails --
//     structurally the same thing the 0o500 fixture used to do, which is
//     what makes it a replacement rather than merely another error.
//
// Go 1.26.5 maps ONLY ENOENT to os.ErrNotExist (verified by running it, not
// inherited: syscall.Errno.Is answers false for ENOTDIR, EEXIST and
// ENAMETOOLONG), so all three are genuine failures rather than the
// "directory is simply absent" path, which checkDataDirWritable creates its
// way out of.
//
// WHAT THIS DOES NOT COVER: EACCES specifically. That is acceptable because
// checkDataDirWritable does not discriminate on errno -- every failure of
// either arm goes through the same dataDirError wrapper -- so the errno is
// not part of the behaviour under test. What is under test is that a data
// directory which cannot be written to fails Load loudly and says who was
// denied and where, and that is now asserted at every uid, including inside
// the gate.
func unwritableDataDirShapes() []unwritableDataDirShape {
	return []unwritableDataDirShape{
		{
			name: "a path component of the data directory is a regular file",
			plant: func(t *testing.T, parent string) string {
				blocker := filepath.Join(parent, "loam-data")
				require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0o644))
				return filepath.Join(blocker, "mirrors-would-go-here")
			},
			wantErrno: syscall.ENOTDIR,
		},
		{
			name: "the data directory is a dangling symlink",
			plant: func(t *testing.T, parent string) string {
				dir := filepath.Join(parent, "loam-data")
				require.NoError(t, os.Symlink(filepath.Join(parent, "gone", "target"), dir))
				info, err := os.Lstat(dir)
				require.NoError(t, err)
				require.NotZero(t, info.Mode()&os.ModeSymlink, "precondition: %s must actually be a symlink", dir)
				return dir
			},
			wantErrno: syscall.EEXIST,
		},
		{
			name: "the data directory exists but the write probe inside it cannot be completed",
			plant: func(t *testing.T, parent string) string {
				return longestCreatableDir(t, parent)
			},
			wantErrno: syscall.ENAMETOOLONG,
		},
	}
}

// longestCreatableDir grows a directory chain under parent until the
// filesystem refuses the next component, and returns the deepest directory
// it actually created. A temp file cannot then be created inside it: its
// name is longer than the single character whose addition just failed, so
// the limit is exceeded by strictly more. Widths descend so the result is
// the exact maximum rather than up to 128 characters short of it, which
// matters because the probe's own filename is only ~28 characters long.
//
// The limit is a property of the filesystem, not of the caller's privileges
// (PATH_MAX is 4096 on Linux and 1024 on darwin), so this is discovered at
// runtime rather than hardcoded, and holds identically at uid 0.
func longestCreatableDir(t *testing.T, parent string) string {
	t.Helper()
	dir := parent
	for _, width := range []int{128, 16, 1} {
		component := strings.Repeat("d", width)
		for {
			next := filepath.Join(dir, component)
			if err := os.Mkdir(next, 0o700); err != nil {
				break
			}
			dir = next
		}
	}
	return dir
}

// probeDataDir repeats, step for step, what checkDataDirWritable does to
// LOAM_DATA_DIR. The tests below call it as a PRECONDITION: proof that the
// shape they planted really did make the directory unwritable for whoever
// is running them. Without it, a shape that some future runtime, filesystem
// or privilege level quietly made writable would fail as a bare "an error is
// expected but got nil" -- the exact failure that cost loam-9g17 three
// agents' worth of triage before anyone worked out that root ignores mode
// bits.
func probeDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".loam-write-check-*")
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(f.Name())
}

func TestLoad_UnwritableDataDir(t *testing.T) {
	// Not parallel (nor are the subtests): t.Setenv is incompatible with
	// t.Parallel.
	for _, shape := range unwritableDataDirShapes() {
		t.Run(shape.name, func(t *testing.T) {
			dir := shape.plant(t, t.TempDir())
			probeErr := probeDataDir(dir)
			require.Error(t, probeErr, "precondition: the planted shape must really make %s unwritable for whoever is running this test", dir)
			require.ErrorIs(t, probeErr, shape.wantErrno, "precondition: the failure must be the one this shape manufactures")
			require.NotErrorIs(t, probeErr, os.ErrNotExist, "precondition: it must be unwritable, not merely absent -- absent is the create-it-and-carry-on path")
			baseEnv(t, dir)
			_, err := Load()
			require.ErrorIs(t, err, errDataDirNotWritable)
		})
	}
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
//
// It runs over every shape because the contract belongs to dataDirError,
// which both arms of checkDataDirWritable funnel into: a refactor that kept
// the uid on one arm and dropped it on the other would be caught here.
func TestLoad_UnwritableDataDirErrorNamesUIDAndPath(t *testing.T) {
	// Not parallel (nor are the subtests): t.Setenv is incompatible with
	// t.Parallel.
	for _, shape := range unwritableDataDirShapes() {
		t.Run(shape.name, func(t *testing.T) {
			dir := shape.plant(t, t.TempDir())
			probeErr := probeDataDir(dir)
			require.Error(t, probeErr, "precondition: the planted shape must really make %s unwritable for whoever is running this test", dir)
			require.ErrorIs(t, probeErr, shape.wantErrno, "precondition: the failure must be the one this shape manufactures")
			baseEnv(t, dir)
			_, err := Load()
			require.ErrorIs(t, err, errDataDirNotWritable)
			assert.Contains(t, err.Error(), dir, "the operator cannot act on a permission error that does not name the directory")
			assert.Contains(t, err.Error(), fmt.Sprintf("uid %d", os.Getuid()),
				"the actionable fact is which uid was denied, not merely that something was")
			assert.Contains(t, err.Error(), fmt.Sprintf("gid %d", os.Getgid()))
			assert.Contains(t, err.Error(), fmt.Sprintf("chown -R %d:%d", os.Getuid(), os.Getgid()),
				"the message should carry the fix, not only the diagnosis")
		})
	}
}

// TestLoad_TelemetryDefaults pins the state a deployment that has never
// heard of OpenTelemetry gets: no endpoint, and therefore telemetry
// disabled entirely (internal/telemetry treats an empty endpoint as the
// single off switch). The two other variables still resolve to their
// documented defaults, so an operator who later sets only
// LOAM_OTEL_ENDPOINT gets a working, conservatively-sampled configuration
// without having to discover two more variables.
func TestLoad_TelemetryDefaults(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	baseEnv(t, t.TempDir())
	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.OTelEndpoint)
	assert.Equal(t, "loam", cfg.OTelServiceName)
	assert.InDelta(t, 0.1, cfg.OTelSampleRatio, 0)
}

// TestLoad_TelemetryOverrides proves all three variables are read, not just
// declared, and -- the part that matters structurally -- that a fully
// configured telemetry setup still uses only OPTIONAL variables. Nothing
// here is lookupRequired, which is what keeps internal/deploycheck's
// TestComposeEnvironmentSatisfiesConfigLoad green while deployment wiring
// remains a separate bead.
func TestLoad_TelemetryOverrides(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	baseEnv(t, t.TempDir())
	t.Setenv("LOAM_OTEL_ENDPOINT", "https://collector.example:4318")
	t.Setenv("LOAM_OTEL_SERVICE_NAME", "loam-staging")
	t.Setenv("LOAM_OTEL_SAMPLE_RATIO", "0.25")
	t.Setenv("LOAM_OTEL_DB_ACQUIRE_THRESHOLD", "5ms")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "https://collector.example:4318", cfg.OTelEndpoint)
	assert.Equal(t, "loam-staging", cfg.OTelServiceName)
	assert.InDelta(t, 0.25, cfg.OTelSampleRatio, 0)
	assert.Equal(t, 5*time.Millisecond, cfg.OTelDBAcquireThreshold)
}

// TestLoad_OTelDBAcquireThreshold covers the variable that lets an operator
// whose pool sits chronically just under the default see its waits at all
// (loam-9v9s). The default case is asserted here rather than only in
// TestLoad_Defaults because the value is load-bearing: internal/db treats
// zero as "use the default", so a loader that silently produced zero would
// look identical in a Config dump while behaving correctly -- and a loader
// that produced zero for an operator who set "0s" would too.
func TestLoad_OTelDBAcquireThreshold(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	t.Run("defaults when unset", func(t *testing.T) {
		baseEnv(t, t.TempDir())
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, defaultOTelDBAcquireThreshold, cfg.OTelDBAcquireThreshold)
	})
	t.Run("rejects a negative duration", func(t *testing.T) {
		baseEnv(t, t.TempDir())
		t.Setenv("LOAM_OTEL_DB_ACQUIRE_THRESHOLD", "-1s")
		_, err := Load()
		require.Error(t, err)
		assert.ErrorIs(t, err, errInvalidDuration)
		assert.Contains(t, err.Error(), "LOAM_OTEL_DB_ACQUIRE_THRESHOLD", "the message must name the variable an operator has to fix")
	})
	t.Run("rejects an unparseable duration", func(t *testing.T) {
		baseEnv(t, t.TempDir())
		t.Setenv("LOAM_OTEL_DB_ACQUIRE_THRESHOLD", "soon")
		_, err := Load()
		require.Error(t, err)
		assert.ErrorIs(t, err, errInvalidDuration)
	})
}

// TestLoad_OTelSampleRatioBoundaries exercises LOAM_OTEL_SAMPLE_RATIO's
// range check exactly the way TestLoad_SyncIntervalAndIngestWorkersBoundaries
// exercises LOAM_INGEST_WORKERS': at and around both edges, asserting on the
// sentinel AND on the message an operator would actually read.
//
// The NaN row is the one that is not merely thorough. strconv.ParseFloat
// ACCEPTS "NaN", and every ordered comparison against NaN is false, so a
// range check written as `v < 0 || v > 1` -- the obvious translation of the
// LOAM_INGEST_WORKERS check this one is modelled on -- passes NaN straight
// through to sdktrace.TraceIDRatioBased. Delete the math.IsNaN guard in
// loadTelemetry and this row is the only thing in the suite that fails.
func TestLoad_OTelSampleRatioBoundaries(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	tests := []struct {
		name        string
		value       string
		want        float64
		wantErr     error
		wantErrText string
	}{
		{name: "zero is accepted", value: "0", want: 0},
		{name: "one is accepted", value: "1", want: 1},
		{name: "a fraction is accepted", value: "0.05", want: 0.05},
		{name: "just below zero is rejected", value: "-0.0001", wantErr: errOTelSampleRatioRange, wantErrText: "LOAM_OTEL_SAMPLE_RATIO: OTel sample ratio out of range: got -0.0001, want between 0 and 1"},
		{name: "just above one is rejected", value: "1.0001", wantErr: errOTelSampleRatioRange, wantErrText: "LOAM_OTEL_SAMPLE_RATIO: OTel sample ratio out of range: got 1.0001, want between 0 and 1"},
		{name: "NaN is rejected", value: "NaN", wantErr: errOTelSampleRatioRange, wantErrText: "LOAM_OTEL_SAMPLE_RATIO: OTel sample ratio out of range: got NaN, want between 0 and 1"},
		{name: "positive infinity is rejected", value: "Inf", wantErr: errOTelSampleRatioRange, wantErrText: "LOAM_OTEL_SAMPLE_RATIO: OTel sample ratio out of range: got +Inf, want between 0 and 1"},
		{name: "negative infinity is rejected", value: "-Inf", wantErr: errOTelSampleRatioRange, wantErrText: "LOAM_OTEL_SAMPLE_RATIO: OTel sample ratio out of range: got -Inf, want between 0 and 1"},
		{name: "a percentage is rejected rather than silently rescaled", value: "10", wantErr: errOTelSampleRatioRange, wantErrText: "LOAM_OTEL_SAMPLE_RATIO: OTel sample ratio out of range: got 10, want between 0 and 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t, t.TempDir())
			t.Setenv("LOAM_OTEL_SAMPLE_RATIO", tc.value)
			cfg, err := Load()
			if tc.wantErr == nil {
				require.NoError(t, err)
				assert.InDelta(t, tc.want, cfg.OTelSampleRatio, 0)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			assert.EqualError(t, err, tc.wantErrText)
		})
	}
}

// TestLoad_OTelEndpointRejectsWhatWouldSilentlyExportNowhere covers the
// forms an operator plausibly writes. The bare host:port case is the
// interesting one: it is what the OTEL_EXPORTER_OTLP_ENDPOINT convention
// accepts elsewhere, it parses as a perfectly valid url.URL (scheme
// "otel-collector", empty host), and without this check it would reach
// otlptracehttp.WithEndpointURL and export to nowhere without a word.
func TestLoad_OTelEndpointRejectsWhatWouldSilentlyExportNowhere(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "http url with a port", value: "http://otel-collector:4318", valid: true},
		{name: "https url", value: "https://collector.example.com", valid: true},
		{name: "http url with a path prefix", value: "http://collector.example.com/otlp", valid: true},
		{name: "bare host and port", value: "otel-collector:4318"},
		{name: "bare host", value: "otel-collector"},
		{name: "grpc scheme", value: "grpc://otel-collector:4317"},
		{name: "scheme with no host", value: "http://"},
		{name: "control character in the url", value: "http://otel\x7f-collector:4318"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t, t.TempDir())
			t.Setenv("LOAM_OTEL_ENDPOINT", tc.value)
			cfg, err := Load()
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, tc.value, cfg.OTelEndpoint)
				return
			}
			require.ErrorIs(t, err, errInvalidOTelEndpoint)
		})
	}
}
