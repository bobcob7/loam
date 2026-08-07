//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon. On podman also
// set TESTCONTAINERS_RYUK_DISABLED=true:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/health/... -v
//
// This file exists because of a survivor found by mutation testing, and the
// survivor is worth naming: rewording pgvectorRegistrationMessage survives
// the ENTIRE unit suite in this package. Every unit test that exercises the
// pgvector branch builds its Ping error out of that same constant, so the
// assertion and the subject are the same symbol and cannot disagree. The
// fast loop reports green on a reword no matter what the constant says --
// including a reword to something pgvector-go never emits.
//
// That is precisely the part of this design its own comment calls fragile.
// databaseFailureReason matches on message text because pgvector-go's
// registration failure is a bare fmt.Errorf with no sentinel, no type and
// no wrapped cause, so errors.Is and errors.As have nothing to bind to. A
// text match is only as good as its agreement with the library, and nothing
// in the unit suite checks that agreement.
//
// So this checks it against the library instead of against itself: it makes
// pgvector-go produce the real error, from a real Postgres with no vector
// extension, and drives it through databaseFailureReason. If pgvector-go
// rewords or restructures, THIS fails loudly at the tag that runs per PR,
// rather than /readyz silently reverting to the vaguer 503 in production
// with no test anywhere noticing.
package health

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/testdb"
)

// TestDatabaseFailureReason_PinsPgvectorGosRealMessage is the pin described
// in this file's doc comment.
//
// testdb.PostgresImageWithoutVector is the right image here and the
// pgvector one would defeat the test: the failure being reproduced is
// to_regtype('vector') returning NULL, which needs a database where the
// extension has not been created. Using the plain image gets that without
// this test having to drop anything.
func TestDatabaseFailureReason_PinsPgvectorGosRealMessage(t *testing.T) {
	t.Parallel()
	ctx := context.WithoutCancel(t.Context())
	container, err := postgres.Run(ctx, testdb.PostgresImageWithoutVector,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, container.Terminate(context.WithoutCancel(t.Context())))
	})
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err, "Postgres itself must be reachable, or this test would prove nothing about WHY registration failed")
	t.Cleanup(func() { assert.NoError(t, conn.Close(context.WithoutCancel(t.Context()))) })

	// The real thing: internal/db.NewPool installs exactly this function as
	// the pool's AfterConnect hook, so this is the error /readyz's Ping
	// surfaces through pgxpool's acquisition path.
	registerErr := pgxvec.RegisterTypes(ctx, conn)
	require.Error(t, registerErr, "pgvector-go must still fail when the vector type does not exist -- if it started tolerating it, the whole diagnosis this reason gives is obsolete")
	assert.Contains(t, registerErr.Error(), pgvectorRegistrationMessage,
		"pgvectorRegistrationMessage no longer matches what pgvector-go emits: /readyz would silently fall back to the vaguer %q, and no unit test would notice", databaseReason)

	// Driven through the function under test, wrapped the way pgxpool
	// wraps it, so this pins the reason and not merely the constant.
	assert.Equal(t, pgvectorReason, databaseFailureReason(fmt.Errorf("acquiring connection: %w", registerErr)))
}

// TestDatabaseFailureReason_ARealConnectionFaultIsNotRepaintedAsPgvector is
// the other direction, and it is the promise the carve-out has to keep:
// docs/deployment-spec.md and helm/loam's postgres-statefulset comment both
// tell operators to expect "database unreachable" for a connection that
// cannot be made. A genuine dial failure, produced by pgx rather than
// hand-written, must still land there.
func TestDatabaseFailureReason_ARealConnectionFaultIsNotRepaintedAsPgvector(t *testing.T) {
	t.Parallel()
	ctx := context.WithoutCancel(t.Context())
	_, err := pgx.Connect(ctx, "postgres://loam:loam@127.0.0.1:1/loam?sslmode=disable&connect_timeout=2")
	require.Error(t, err)
	assert.Equal(t, databaseReason, databaseFailureReason(err),
		"a refused connection is not an extension problem: the carve-out refines the diagnosis and must never widen it")
}
