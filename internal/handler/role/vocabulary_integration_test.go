//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon. Run
// explicitly with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/handler/role/... -v
//
// (see internal/db/migrations/integration_test.go for why
// TESTCONTAINERS_RYUK_DISABLED is a podman-only workaround, not a CI
// setting).
package role

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/testdb"
)

// constraintLiteral pulls each quoted value out of a rendered CHECK
// constraint definition. Postgres renders
// role_operations_operation_check as
// `CHECK (operation = ANY (ARRAY['work.start'::text, ...]))`, so the
// quoted literals are exactly the vocabulary the database will accept.
var constraintLiteral = regexp.MustCompile(`'([^']*)'`)

// TestDatabaseCheckConstraintMatchesTheGoVocabulary is the drift guard
// between the two statements of the fixed capability vocabulary that
// cannot be expressed as one:
//
//   - internal/handler's Capability constants and AllCapabilities, which
//     this handler validates every CreateRole/UpdateRole request against;
//   - role_operations_operation_check in migration 0001_init, which the
//     database enforces underneath it.
//
// Everything else in Go reads the first (this package holds no copy of the
// list; see validateOperations). The migration cannot, so this test asserts
// the two agree SET-WISE rather than trusting a comment that says they do.
// Both directions matter and both are checked:
//
//   - a capability in Go but not in the CHECK is an operation an admin can
//     be granted through the API and that the database will then refuse,
//     failing the whole transaction at write time;
//   - a value in the CHECK but not in Go is an operation the database would
//     store and that no gate in the system will ever honour, since
//     CapabilityChecker only ever asks about members of the Go vocabulary.
//
// This is the same treatment loam-czi applied when a prose inventory of
// scheduler collaborators went stale three times and was replaced with
// compile-time assertions: an inventory that cannot be checked WILL drift.
func TestDatabaseCheckConstraintMatchesTheGoVocabulary(t *testing.T) {
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
		assert.NoError(t, container.Terminate(context.Background()))
	})
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var definition string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'role_operations_operation_check'`,
	).Scan(&definition), "the CHECK constraint must exist under that exact name")
	matches := constraintLiteral.FindAllStringSubmatch(definition, -1)
	require.NotEmpty(t, matches, "the constraint definition must contain quoted operation literals: %s", definition)
	accepted := make([]string, 0, len(matches))
	for _, match := range matches {
		accepted = append(accepted, match[1])
	}
	expected := make([]string, 0, len(handler.AllCapabilities()))
	for _, capability := range handler.AllCapabilities() {
		expected = append(expected, string(capability))
	}
	assert.ElementsMatch(t, expected, accepted,
		"role_operations_operation_check and internal/handler's capability vocabulary have drifted apart: %s", definition)
}
