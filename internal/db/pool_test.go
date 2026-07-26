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
	assert.ErrorIs(t, err, ErrMissingDatabaseURL)
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

// TestNewPoolConfigSetsAfterConnect is a DB-free smoke check that NewPool
// wires something into AfterConnect at all: pgxpool.ParseConfig never
// dials the network, so this exercises newPoolConfig without a live
// database. It intentionally does not pin AfterConnect to a specific
// function value -- reflect.Value.Pointer() is documented as not a
// reliable function identity check, and a closure that calls
// pgxvec.RegisterTypes plus does something else (e.g. logs, registers an
// additional type) would be an equally correct implementation that a
// stricter check would wrongly fail. What actually needs to be true --
// that the pgvector codec is the thing active on a real pooled connection,
// not merely that some function is assigned -- is proven against a real
// database by assertVectorTypeRegistered in pool_integration_test.go.
func TestNewPoolConfigSetsAfterConnect(t *testing.T) {
	t.Parallel()
	poolCfg, err := newPoolConfig("postgres://localhost:1/nonexistent?sslmode=disable")
	require.NoError(t, err)
	require.NotNil(t, poolCfg.AfterConnect, "NewPool must set AfterConnect so pgvector types are registered once migrations have created the extension")
}
