//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon; see
// internal/db/migrations/integration_test.go for how to run it and the
// podman/ryuk caveat (TESTCONTAINERS_RYUK_DISABLED=true).
package meta_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/meta"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/rolestore"
	"github.com/bobcob7/loam/internal/testdb"
)

// roleStoreAdapter wires internal/rolestore.Store into meta.RoleStore --
// the same two-method shape cmd/server/main.go's own (unexported)
// roleStoreAdapter provides in production, reproduced here so this test
// exercises the real Handler.GetInstructions end to end against a real,
// freshly migrated database, rather than the RoleStoreMock every other
// test in this package uses.
type roleStoreAdapter struct {
	store *rolestore.Store
}

func (a roleStoreAdapter) RoleCapabilities(ctx context.Context, role string) ([]handler.Capability, error) {
	r, err := a.store.GetRole(ctx, role)
	if err != nil {
		return nil, err
	}
	capabilities := make([]handler.Capability, len(r.Operations))
	for i, operation := range r.Operations {
		capabilities[i] = handler.Capability(operation)
	}
	return capabilities, nil
}

func (a roleStoreAdapter) RoleInstructions(ctx context.Context, role string) (string, error) {
	r, err := a.store.GetRole(ctx, role)
	if err != nil {
		return "", err
	}
	return r.Instructions, nil
}

// TestGetInstructions_FreshlyMigratedDatabase_BothBuiltinsHaveNonEmptyRoleInstructions
// is acceptance criterion 5's own literal proof for loam-0pj.17: on a
// database that has only ever seen migrations.Migrate (0001_init's empty
// seed, then 0006_role_instructions_seed's fill), the REAL
// loam.v1.MetaService.GetInstructions RPC -- not a query against roles
// directly -- returns a non-empty role_instructions for BOTH built-in
// roles. A regression that left either role's instructions empty (a
// migration ordering bug, a guard that fired when it should not have, or a
// RoleStore wiring mistake) fails this the same way an agent running `loam
// instructions` against a fresh deployment would notice it: an empty
// role_instructions field.
func TestGetInstructions_FreshlyMigratedDatabase_BothBuiltinsHaveNonEmptyRoleInstructions(t *testing.T) {
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
	t.Cleanup(func() { assert.NoError(t, container.Terminate(context.Background())) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	store := roleStoreAdapter{store: rolestore.NewStore(pool, logger)}
	mapper := handler.NewErrorMapper(logger)
	h := meta.New(store, mapper, logger)

	for _, role := range []string{"author", "reviewer"} {
		agentCtx := httpauth.WithIdentity(ctx, httpauth.Identity{Name: "grace-hopper", ID: "3", Role: role})
		resp, err := h.GetInstructions(agentCtx, connect.NewRequest(&loamv1.GetInstructionsRequest{}))
		require.NoErrorf(t, err, "GetInstructions for role %s", role)
		assert.NotEmptyf(t, resp.Msg.GetRoleInstructions(), "role_instructions for built-in role %s must be non-empty on a freshly migrated database", role)
	}
}
