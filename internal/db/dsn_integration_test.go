//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag. Run explicitly with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/db/... -run TestMigrationDSNAgainstRealPostgres -v
//
// See pool_integration_test.go's header for why ryuk is disabled under
// podman.
package db

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
)

// poolMaxConns is declared in dsn_test.go, which carries no build tag and is
// therefore compiled into this build too. Deliberately shared rather than
// restated: the value has to be one pgxpool's max(4, NumCPU) default could
// not also produce, and a copy of the number without a copy of that reason
// is how the next edit picks a plausible-looking core count and quietly
// stops testing anything.

// TestMigrationDSNAgainstRealPostgres is the proof loam-lhc9 asked for, and
// it cannot be a unit test: the rejection that breaks the boot comes from
// the SERVER's startup-option handling, not from any Go-side parse. pgx
// parses `pool_max_conns=97` happily and hands it to Postgres, which refuses
// the connection outright.
//
// The test runs the real boot sequence -- Migrate then NewPool, the order
// internal/db/pool.go's NewPool doc comment requires -- with a DSN carrying
// a pgxpool parameter, and asserts three things, because "no error" alone
// would not tell the fix from an accident:
//
//  1. The unfixed call still fails, against THIS Postgres. Without this the
//     suite could go green because the server tolerated the unknown
//     parameter rather than because MigrationDSN removed it, and the test
//     would keep passing after the fix was reverted.
//  2. Migrate succeeds on MigrationDSN(dsn) -- the boot path is unblocked.
//  3. The pool built from the ORIGINAL dsn actually has MaxConns == 97.
//     This is the assertion that separates "the parameter was stripped for
//     database/sql" from "the parameter was dropped everywhere": a fix that
//     quietly discarded the operator's pool sizing would satisfy 1 and 2
//     and be a different bug.
func TestMigrationDSNAgainstRealPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, container.Terminate(context.WithoutCancel(t.Context())))
	})
	base, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	dsn := fmt.Sprintf("%s&pool_max_conns=%d", base, poolMaxConns)

	// (1) The negative control: the pre-fix behaviour, still reproducible.
	// Migrate connects through database/sql, which forwards pool_max_conns
	// to the server as a startup option; the server rejects it with
	// SQLSTATE 42704.
	err = migrations.Migrate(ctx, dsn, logger)
	require.Error(t, err, "handing a pgxpool parameter to database/sql must still be rejected by the server")
	assert.Contains(t, err.Error(), `unrecognized configuration parameter "pool_max_conns"`)

	// (2) The fix: the same DSN, minus exactly what pgxpool owns, migrates.
	migrationDSN, err := MigrationDSN(dsn)
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, migrationDSN, logger),
		"MigrationDSN must leave a DSN database/sql can connect with")

	// (3) The pool still gets what the operator configured.
	pool, err := NewPool(ctx, Config{DatabaseURL: dsn, EncryptionKey: "key"}, logger)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	assert.Equal(t, int32(poolMaxConns), pool.Config().MaxConns,
		"the pool must keep the operator's pool_max_conns -- stripping it from BOTH consumers would be a different bug")
	var one int
	require.NoError(t, pool.QueryRow(ctx, `SELECT 1`).Scan(&one))
	assert.Equal(t, 1, one)
}

// TestMigrationDSNBootsServerWithPoolParameters walks the full startup
// sequence in the order cmd/server's connectDatabase does it -- prepare the
// migration DSN, migrate, then build the pool -- against a virgin database
// with a pool parameter set. Before loam-lhc9 this sequence could not get
// past its first step, and the operator's second log line was
// `vector type not found in the database`, which is about a subsystem that
// was never at fault. Asserting the vector round trip at the end proves the
// sequel is gone for the right reason: the extension really is there,
// because migrations really did run.
func TestMigrationDSNBootsServerWithPoolParameters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, container.Terminate(context.WithoutCancel(t.Context())))
	})
	base, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	dsn := fmt.Sprintf("%s&pool_max_conns=%d&application_name=loam", base, poolMaxConns)

	migrationDSN, err := MigrationDSN(dsn)
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, migrationDSN, logger))
	pool, err := NewPool(ctx, Config{DatabaseURL: dsn, EncryptionKey: "key"}, logger)
	require.NoError(t, err, "the server must boot with a pgxpool parameter in LOAM_DATABASE_URL")
	t.Cleanup(pool.Close)

	// application_name is not pgxpool's, so it must have survived into the
	// migration connection too -- the fix must not be "drop everything
	// database/sql does not recognize".
	var appName string
	require.NoError(t, pool.QueryRow(ctx, `SHOW application_name`).Scan(&appName))
	assert.Equal(t, "loam", appName)

	assertVectorTypeRegistered(ctx, t, pool)
}
