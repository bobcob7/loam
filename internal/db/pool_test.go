package db

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	pgxvec "github.com/pgvector/pgvector-go/pgx"
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

// TestNewPoolConfigRegistersPgvectorAfterConnect proves the AfterConnect
// hook is wired to pgxvec.RegisterTypes without needing a live database:
// pgxpool.ParseConfig never dials the network, so this exercises the exact
// wiring NewPool performs. This matters because the equivalent
// round-trip-through-a-real-database assertion (see
// pool_integration_test.go) does NOT catch a missing/broken AfterConnect
// hook -- pgvector-go's Vector type also implements database/sql.Scanner
// and driver.Valuer, so pgx silently falls back to that path for an
// unregistered OID and the round trip still passes. Comparing function
// pointers by reflection is deliberate, not incidental: the bead's review
// specifically calls for a direct assignment (`poolCfg.AfterConnect =
// pgxvec.RegisterTypes`) rather than a redundant wrapping closure, and this
// assertion is what actually distinguishes the two.
func TestNewPoolConfigRegistersPgvectorAfterConnect(t *testing.T) {
	t.Parallel()
	poolCfg, err := newPoolConfig("postgres://localhost:1/nonexistent?sslmode=disable")
	require.NoError(t, err)
	require.NotNil(t, poolCfg.AfterConnect, "NewPool must set AfterConnect so pgvector types are registered once migrations have created the extension")
	want := reflect.ValueOf(pgxvec.RegisterTypes).Pointer()
	got := reflect.ValueOf(poolCfg.AfterConnect).Pointer()
	assert.Equal(t, want, got, "AfterConnect must be pgxvec.RegisterTypes directly, not a wrapping closure")
}
