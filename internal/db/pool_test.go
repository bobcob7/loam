package db

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestNewPoolEmptyDatabaseURL(t *testing.T) {
	t.Parallel()
	pool, err := NewPool(t.Context(), Config{DatabaseURL: "", EncryptionKey: "key"}, testLogger())
	assert.Nil(t, pool)
	assert.ErrorIs(t, err, errMissingDatabaseURL)
}

func TestNewPoolUnparseableDSN(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dsn  string
	}{
		{name: "not a dsn", dsn: "not a dsn at all"},
		{name: "malformed scheme", dsn: "://bad"},
		{name: "trailing garbage", dsn: "postgres://localhost/db?sslmode=disable extra garbage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool, err := NewPool(t.Context(), Config{DatabaseURL: tt.dsn, EncryptionKey: "key"}, testLogger())
			assert.Nil(t, pool)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "parsing database url")
		})
	}
}

func TestNewPoolUnreachableDSN(t *testing.T) {
	t.Parallel()
	pool, err := NewPool(t.Context(), Config{DatabaseURL: "postgres://localhost:1/nonexistent?sslmode=disable&connect_timeout=1", EncryptionKey: "key"}, testLogger())
	assert.Nil(t, pool)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pinging database")
}
