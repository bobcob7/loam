package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeGetenv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		databaseURL   string
		encryptionKey string
		wantErr       error
	}{
		{name: "both set", databaseURL: "postgres://localhost/db", encryptionKey: "a-secret-key", wantErr: nil},
		{name: "missing database url", databaseURL: "", encryptionKey: "a-secret-key", wantErr: errMissingDatabaseURL},
		{name: "missing encryption key", databaseURL: "postgres://localhost/db", encryptionKey: "", wantErr: errMissingEncryptionKey},
		{name: "both missing", databaseURL: "", encryptionKey: "", wantErr: errMissingDatabaseURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getenv := fakeGetenv(map[string]string{"DATABASE_URL": tt.databaseURL, "LOAM_ENCRYPTION_KEY": tt.encryptionKey})
			cfg, err := loadConfig(getenv)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.databaseURL, cfg.DatabaseURL)
			assert.Equal(t, tt.encryptionKey, cfg.EncryptionKey)
		})
	}
}

func TestLoadConfigReadsFromProcessEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("LOAM_ENCRYPTION_KEY", "a-secret-key")
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "postgres://localhost/db", cfg.DatabaseURL)
	assert.Equal(t, "a-secret-key", cfg.EncryptionKey)
}
