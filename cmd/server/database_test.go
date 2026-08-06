package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/db"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// callOrder is a thread-safe append-only log the tests below use to prove
// the exact sequence connectDatabase invoked its two collaborators in,
// without a real Postgres.
type callOrder struct {
	mu    sync.Mutex
	calls []string
}

func (c *callOrder) record(call string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *callOrder) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// TestConnectDatabase_MigratesBeforeConnectingPool is the discriminating
// proof behind this bead's fix to loam-ut9: docs/server-spec.md's Startup
// step 2 must run migrate, then newPool, never the reverse (a pool built
// before migrations have created the pgvector extension deadlocks
// permanently on a virgin database -- internal/db/pool.go's NewPool doc
// comment). A mutant connectDatabase that swapped the two calls, or that
// called newPool unconditionally regardless of migrate's error, would
// pass a test that merely checked "no error" -- this asserts the actual
// call order via a spy, so a swap is caught by assertion rather than by a
// real database wedging.
func TestConnectDatabase_MigratesBeforeConnectingPool(t *testing.T) {
	t.Parallel()
	var order callOrder
	migrate := func(_ context.Context, dsn string, _ *slog.Logger) error {
		order.record("migrate:" + dsn)
		return nil
	}
	newPool := func(_ context.Context, cfg db.Config, _ *slog.Logger) (*pgxpool.Pool, error) {
		order.record("newPool:" + cfg.DatabaseURL)
		return &pgxpool.Pool{}, nil
	}
	cfg := config.Config{DatabaseURL: "postgres://example/loam", EncryptionKey: []byte("key"), Logger: testLogger()}
	pool, err := connectDatabase(t.Context(), cfg, migrate, newPool)
	require.NoError(t, err)
	require.NotNil(t, pool)
	assert.Equal(t, []string{"migrate:postgres://example/loam", "newPool:postgres://example/loam"}, order.snapshot())
}

// TestConnectDatabase_MigrateFailureNeverCallsNewPool proves the abort
// path: when migrate fails, newPool must never be invoked at all -- not
// just that connectDatabase eventually returns an error, since a mutant
// that called both unconditionally and merely returned migrate's error
// afterward would still open a pool against an unmigrated database before
// this function's caller ever saw the failure.
func TestConnectDatabase_MigrateFailureNeverCallsNewPool(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("migration exploded")
	migrate := func(context.Context, string, *slog.Logger) error { return wantErr }
	var newPoolCalled bool
	newPool := func(context.Context, db.Config, *slog.Logger) (*pgxpool.Pool, error) {
		newPoolCalled = true
		return nil, nil
	}
	cfg := config.Config{DatabaseURL: "postgres://example/loam", Logger: testLogger()}
	pool, err := connectDatabase(t.Context(), cfg, migrate, newPool)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Nil(t, pool)
	assert.False(t, newPoolCalled, "newPool must never run against a database whose migrations failed")
}

// TestConnectDatabase_MigrateGetsDSNWithoutPoolParameters is the loam-lhc9
// regression proof at the wiring level: the two collaborators must receive
// DIFFERENT strings. migrate reaches Postgres through database/sql, which
// forwards pgxpool's own pool_max_conns to the server as a startup option
// and is refused with `FATAL: unrecognized configuration parameter`; newPool
// must still get the operator's DSN intact, or the boot would have been
// fixed by silently discarding their pool sizing. Both halves are asserted,
// because a mutant that stripped the parameter from BOTH would pass a test
// that only checked migrate.
func TestConnectDatabase_MigrateGetsDSNWithoutPoolParameters(t *testing.T) {
	t.Parallel()
	const dsn = "postgres://example/loam?sslmode=disable&pool_max_conns=17"
	var migrateDSN, poolDSN string
	migrate := func(_ context.Context, got string, _ *slog.Logger) error {
		migrateDSN = got
		return nil
	}
	newPool := func(_ context.Context, cfg db.Config, _ *slog.Logger) (*pgxpool.Pool, error) {
		poolDSN = cfg.DatabaseURL
		return &pgxpool.Pool{}, nil
	}
	cfg := config.Config{DatabaseURL: dsn, EncryptionKey: []byte("key"), Logger: testLogger()}
	_, err := connectDatabase(t.Context(), cfg, migrate, newPool)
	require.NoError(t, err)
	migrateCfg, err := pgx.ParseConfig(migrateDSN)
	require.NoError(t, err)
	assert.NotContains(t, migrateCfg.RuntimeParams, "pool_max_conns",
		"migrate connects via database/sql, which would forward pool_max_conns to the server as a startup option")
	assert.Equal(t, "example", migrateCfg.Host, "stripping the pool parameter must not disturb the connection target")
	assert.Equal(t, dsn, poolDSN, "newPool must receive the operator's DSN intact -- the pool parameters are addressed to it")
}

// TestConnectDatabase_UnusableDSNNeverReachesMigrate proves the new failure
// mode fails fast: a DSN whose pool parameters pgxpool itself rejects stops
// the boot before either collaborator runs, rather than being passed on to
// fail later somewhere with a worse message.
func TestConnectDatabase_UnusableDSNNeverReachesMigrate(t *testing.T) {
	t.Parallel()
	var migrateCalled, newPoolCalled bool
	migrate := func(context.Context, string, *slog.Logger) error {
		migrateCalled = true
		return nil
	}
	newPool := func(context.Context, db.Config, *slog.Logger) (*pgxpool.Pool, error) {
		newPoolCalled = true
		return &pgxpool.Pool{}, nil
	}
	cfg := config.Config{DatabaseURL: "postgres://example/loam?pool_max_conns=lots", Logger: testLogger()}
	pool, err := connectDatabase(t.Context(), cfg, migrate, newPool)
	require.Error(t, err)
	assert.Nil(t, pool)
	assert.Contains(t, err.Error(), "preparing migration database url")
	assert.False(t, migrateCalled, "migrate must not run against a DSN that could not be prepared")
	assert.False(t, newPoolCalled, "newPool must not run against a DSN that could not be prepared")
}

// TestConnectDatabase_NewPoolFailurePropagates proves the second half of
// the fail-fast contract: a newPool error (e.g. the database became
// unreachable between migrate and connect) surfaces to the caller rather
// than being swallowed.
func TestConnectDatabase_NewPoolFailurePropagates(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("connection refused")
	migrate := func(context.Context, string, *slog.Logger) error { return nil }
	newPool := func(context.Context, db.Config, *slog.Logger) (*pgxpool.Pool, error) {
		return nil, wantErr
	}
	cfg := config.Config{DatabaseURL: "postgres://example/loam", Logger: testLogger()}
	pool, err := connectDatabase(t.Context(), cfg, migrate, newPool)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Nil(t, pool)
}
