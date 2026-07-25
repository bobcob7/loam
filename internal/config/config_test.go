package config

import (
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validEncryptionKey returns a base64-encoded 32-byte key, satisfying
// LOAM_ENCRYPTION_KEY's validation.
func validEncryptionKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

// baseEnv sets every required LOAM_* variable, plus LOAM_DATA_DIR pointed at
// dataDir, to a value that passes validation. Callers override individual
// variables afterward to exercise specific failure paths.
//
// NOTE: this helper (and every test below) uses t.Setenv, which the testing
// package forbids combining with t.Parallel() since environment variables
// are process-global. None of the tests in this file call t.Parallel().
func baseEnv(t *testing.T, dataDir string) {
	t.Helper()
	t.Setenv("LOAM_ADMIN_PASSWORD", "hunter2")
	t.Setenv("LOAM_DATABASE_URL", "postgres://user:pass@localhost:5432/loam")
	t.Setenv("LOAM_ENCRYPTION_KEY", validEncryptionKey())
	t.Setenv("LOAM_DATA_DIR", dataDir)
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
	assert.Equal(t, dataDir, cfg.DataDir)
	assert.Equal(t, 60*time.Second, cfg.SyncInterval)
	assert.True(t, cfg.PRAttribution)
	assert.Equal(t, "http://localhost:11434", cfg.EmbedderURL)
	assert.Equal(t, "nomic-embed-text", cfg.EmbedderModel)
	assert.Equal(t, 2, cfg.IngestWorkers)
	assert.Equal(t, slog.LevelInfo, cfg.LogLevel)
	assert.NotNil(t, cfg.Logger)
}

func TestLoad_InvalidInput(t *testing.T) {
	// Not parallel (nor are the subtests below): t.Setenv is incompatible
	// with t.Parallel.
	dataDir := t.TempDir()
	tests := []struct {
		name     string
		override map[string]string
	}{
		{name: "missing admin password", override: map[string]string{"LOAM_ADMIN_PASSWORD": ""}},
		{name: "missing database url", override: map[string]string{"LOAM_DATABASE_URL": ""}},
		{name: "missing encryption key", override: map[string]string{"LOAM_ENCRYPTION_KEY": ""}},
		{name: "encryption key bad base64", override: map[string]string{"LOAM_ENCRYPTION_KEY": "not-valid-base64!!"}},
		{name: "encryption key wrong length", override: map[string]string{"LOAM_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 16))}},
		{name: "unparseable sync interval", override: map[string]string{"LOAM_SYNC_INTERVAL": "not-a-duration"}},
		{name: "invalid log level", override: map[string]string{"LOAM_LOG_LEVEL": "verbose"}},
		{name: "invalid database url scheme", override: map[string]string{"LOAM_DATABASE_URL": "mysql://localhost/loam"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t, dataDir)
			for k, v := range tc.override {
				t.Setenv(k, v)
			}
			_, err := Load()
			require.Error(t, err)
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
	require.Error(t, err)
}
