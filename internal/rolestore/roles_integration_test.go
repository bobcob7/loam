//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon. Run
// explicitly with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/rolestore/... -v
//
// (see internal/db/migrations/integration_test.go for why
// TESTCONTAINERS_RYUK_DISABLED is a podman-only workaround, not a CI
// setting). Uses testdb.PostgresImage (pgvector/pgvector:pg16), the image
// every integration test that runs migrations.Migrate must use, since
// migration 0002_code_intel issues CREATE EXTENSION vector.
package rolestore

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
)

// newTestStore migrates a fresh Postgres container (proving roles_name_key
// and role_operations_operation_check actually exist against the real
// 0001_init schema, not just that the Go compiles) and returns a Store
// wired over the real sqlc-generated Queries.
func newTestStore(t *testing.T) *Store {
	t.Helper()
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
	return NewStore(pool, logger)
}

// TestGetRole_BuiltinAuthor_ResolvesSeededOperations proves GetRole
// resolves the "author" role seeded by 0001_init.up.sql to exactly the
// operations that migration grants it -- against the real schema, not a
// fixture this package invented.
func TestGetRole_BuiltinAuthor_ResolvesSeededOperations(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	role, err := store.GetRole(t.Context(), "author")
	require.NoError(t, err)
	assert.Equal(t, "author", role.Name)
	assert.True(t, role.Builtin, "the seeded author role is builtin")
	assert.ElementsMatch(t, []string{
		"work.start", "work.set", "work.request_review", "work.reply",
		"git.clone", "git.push", "work.read", "graph.query", "search",
	}, role.Operations, "must match 0001_init.up.sql's author seed exactly -- neither more nor fewer operations")
	assert.NotContains(t, role.Operations, "work.verdict", "an author must not be seeded with the reviewer-only work.verdict operation")
}

// TestGetRole_BuiltinReviewer_ResolvesSeededOperations mirrors the above
// for "reviewer", including proving it lacks work.start and git.push --
// the two operations roles.feature's "A reviewer may not start a work
// branch or push" scenario depends on.
func TestGetRole_BuiltinReviewer_ResolvesSeededOperations(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	role, err := store.GetRole(t.Context(), "reviewer")
	require.NoError(t, err)
	assert.Equal(t, "reviewer", role.Name)
	assert.True(t, role.Builtin)
	assert.ElementsMatch(t, []string{
		"work.read", "work.reply", "work.verdict", "git.clone", "graph.query", "search",
	}, role.Operations)
	assert.NotContains(t, role.Operations, "work.start", "a reviewer must not be able to start a work branch")
	assert.NotContains(t, role.Operations, "git.push", "a reviewer must not be able to push")
}

// TestGetRole_UnknownRole_ReturnsErrNotFound proves an unrecognized role
// name -- e.g. a typo'd Loam-Agent-Role header -- is rejected against the
// real roles_name_key unique index, not just a mocked assumption.
func TestGetRole_UnknownRole_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	_, err := store.GetRole(t.Context(), "not-a-real-role")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestListRoles_AttachesEachBuiltinsOperationsToTheRightRole proves
// ListRoles' two-query grouping attaches the right operations to the right
// role against the real seed -- the failure mode a single-role fixture
// cannot catch is operations landing on the wrong role, so this asserts
// two roles, keyed BY NAME.
//
// Keyed by name and not by position on purpose (loam-w8li). This test used
// to read roles[0] as author and roles[1] as reviewer over a two-role seed,
// behind a require.Len(roles, 2). Migration 0009_orchestrator_role seeds a
// third built-in, so by name order roles[1] became the ORCHESTRATOR.
//
// What actually happened at runtime is worth being exact about, because the
// lesson is not the obvious one: the require.Len called FailNow, so the four
// operation assertions never executed at all. The count guard was the ONLY
// thing standing between this suite and four assertions silently addressing
// the wrong role -- and it was standing there by accident, since a count is
// not what any of them is about. Renumbering the 2 to a 3 would have removed
// that accidental guard and let them run misdirected: three would still have
// passed, one of them (NotContains git.push, against a role that happens not
// to hold it) for entirely the wrong reason. Keying by name removes the
// count deliberately and makes the misdirection impossible instead of merely
// fatal, so the next built-in role does not touch this test at all.
//
// Dropping the count costs nothing HERE: what this test is about is
// ListRoles' grouping, and ListRoles reads a database that may legitimately
// hold operator-created roles besides the seeded ones. The seed's exact
// membership is pinned where it is checkable -- internal/db/migrations'
// assertBuiltinRolesSeeded, over a fresh database.
func TestListRoles_AttachesEachBuiltinsOperationsToTheRightRole(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	roles, err := store.ListRoles(t.Context())
	require.NoError(t, err)
	byName := make(map[string]Role, len(roles))
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		byName[role.Name] = role
		names = append(names, role.Name)
	}
	require.Len(t, byName, len(roles), "ListRoles must return each role once -- a duplicate name would hide one role behind another in this map, and every assertion below would then be reading a role it did not mean to")
	author, ok := byName["author"]
	require.Truef(t, ok, "the built-in author role must be listed; got %v", names)
	reviewer, ok := byName["reviewer"]
	require.Truef(t, ok, "the built-in reviewer role must be listed; got %v", names)
	// ListRoles documents "ordered by name"; asserted over the whole slice
	// rather than by pinning indexes, so the ordering contract survives the
	// switch to name keying and still holds for any number of roles.
	assert.IsIncreasing(t, names, "ListRoles returns roles ordered by name")
	assert.True(t, author.Builtin)
	assert.True(t, reviewer.Builtin)
	assert.Contains(t, author.Operations, "git.push", "the author role carries git.push")
	assert.NotContains(t, reviewer.Operations, "git.push", "the reviewer role must not carry git.push")
	assert.Contains(t, reviewer.Operations, "work.verdict", "the reviewer role carries work.verdict")
	assert.NotContains(t, author.Operations, "work.verdict", "the author role must not carry work.verdict")
}

// TestCreateRole_RoundTripsThroughGetRole proves a created role and its
// operations are readable back through the same path every capability
// check uses, with builtin false.
func TestCreateRole_RoundTripsThroughGetRole(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	created, err := store.CreateRole(t.Context(), RoleParams{
		Name:         "release-captain",
		Instructions: "cut releases",
		Operations:   []string{"git.push", "git.clone", "work.read"},
	})
	require.NoError(t, err)
	assert.False(t, created.Builtin, "a role created through the store is never built-in")
	assert.Equal(t, []string{"git.clone", "git.push", "work.read"}, created.Operations, "operations come back sorted, matching every read path")
	read, err := store.GetRole(t.Context(), "release-captain")
	require.NoError(t, err)
	assert.Equal(t, created, read, "a created role must equal the same role read back")
}

// TestCreateRole_DuplicateName_ReturnsErrAlreadyExists proves the
// roles_name_key collision is classified, against the real index.
func TestCreateRole_DuplicateName_ReturnsErrAlreadyExists(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	_, err := store.CreateRole(t.Context(), RoleParams{Name: "author", Operations: []string{"search"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAlreadyExists)
	assert.NotErrorIs(t, err, ErrNotFound)
}

// TestCreateRole_UnknownOperation_RollsBackTheWholeRole is the
// transaction's reason for existing, proved rather than asserted in prose:
// role_operations_operation_check refuses the bogus operation, and the
// roles row inserted moments earlier in the same transaction must go with
// it. A non-transactional implementation leaves behind a role with a
// partial (silently under-privileged) capability set that nothing later
// can detect as wrong.
func TestCreateRole_UnknownOperation_RollsBackTheWholeRole(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	_, err := store.CreateRole(t.Context(), RoleParams{
		Name:       "half-written",
		Operations: []string{"search", "admin.everything"},
	})
	require.Error(t, err, "the CHECK constraint must refuse an operation outside the fixed vocabulary")
	_, err = store.GetRole(t.Context(), "half-written")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound, "the roles row must not survive its failed operation grant")
}

// TestUpdateRole_ReplacesOperationsAndInstructions proves the update
// REPLACES rather than merges: the operation dropped from the request must
// be gone from the stored role.
// The two operation sets are DISJOINT on purpose. An overlapping pair
// would make a merging implementation collide with PRIMARY KEY (role_id,
// operation) and fail loudly for the wrong reason -- proving nothing about
// replacement. With no overlap, a merge succeeds at the database level and
// is caught only by the assertions below, which is the failure this test
// exists to catch.
func TestUpdateRole_ReplacesOperationsAndInstructions(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	_, err := store.CreateRole(t.Context(), RoleParams{
		Name:       "release-captain",
		Operations: []string{"work.read", "search"},
	})
	require.NoError(t, err)
	updated, err := store.UpdateRole(t.Context(), RoleParams{
		Name:         "release-captain",
		Instructions: "now you may push",
		Operations:   []string{"git.clone", "git.push"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"git.clone", "git.push"}, updated.Operations)
	assert.Equal(t, "now you may push", updated.Instructions)
	read, err := store.GetRole(t.Context(), "release-captain")
	require.NoError(t, err)
	assert.Equal(t, []string{"git.clone", "git.push"}, read.Operations)
	assert.NotContains(t, read.Operations, "work.read", "an operation left out of the update must be revoked, not merged")
	assert.NotContains(t, read.Operations, "search", "an operation left out of the update must be revoked, not merged")
}

// TestUpdateRole_BuiltinRole_IsAllowedAndKeepsItsBuiltinFlag pins the
// judgement call at the store layer: a built-in stays editable (its
// instructions ship empty and could otherwise never be set), and the
// update never writes the flag that protects it from deletion.
func TestUpdateRole_BuiltinRole_IsAllowedAndKeepsItsBuiltinFlag(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	updated, err := store.UpdateRole(t.Context(), RoleParams{
		Name:         "reviewer",
		Instructions: "review carefully",
		Operations:   []string{"work.read", "work.verdict", "git.clone"},
	})
	require.NoError(t, err)
	assert.True(t, updated.Builtin, "updating a built-in must not clear its builtin flag")
	assert.Equal(t, "review carefully", updated.Instructions)
	read, err := store.GetRole(t.Context(), "reviewer")
	require.NoError(t, err)
	assert.True(t, read.Builtin)
	assert.Equal(t, "review carefully", read.Instructions)
}

// TestUpdateRole_UnknownRole_ReturnsErrNotFound proves the update's
// zero-rows case is classified rather than reported as a silent success.
func TestUpdateRole_UnknownRole_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	_, err := store.UpdateRole(t.Context(), RoleParams{Name: "ghost", Operations: []string{"search"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestUpdateRole_UnknownOperation_LeavesTheExistingGrantsIntact is the
// update's half of the atomicity proof, and the more dangerous half: the
// statement order is DELETE-then-INSERT, so a non-transactional
// implementation would strip the role to zero operations and leave it
// there.
func TestUpdateRole_UnknownOperation_LeavesTheExistingGrantsIntact(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	_, err := store.CreateRole(t.Context(), RoleParams{
		Name:       "release-captain",
		Operations: []string{"git.clone", "work.read"},
	})
	require.NoError(t, err)
	_, err = store.UpdateRole(t.Context(), RoleParams{
		Name:       "release-captain",
		Operations: []string{"git.push", "admin.everything"},
	})
	require.Error(t, err)
	read, err := store.GetRole(t.Context(), "release-captain")
	require.NoError(t, err)
	assert.Equal(t, []string{"git.clone", "work.read"}, read.Operations, "a failed update must leave the previous grants untouched")
}

// TestDeleteRole_CustomRole_RemovesItAndItsOperations proves the delete
// reaches role_operations through the FK cascade -- a leftover operation
// row would be re-granted to any future role that reused the id.
func TestDeleteRole_CustomRole_RemovesItAndItsOperations(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	created, err := store.CreateRole(t.Context(), RoleParams{Name: "release-captain", Operations: []string{"git.clone"}})
	require.NoError(t, err)
	require.NoError(t, store.DeleteRole(t.Context(), "release-captain"))
	_, err = store.GetRole(t.Context(), "release-captain")
	assert.ErrorIs(t, err, ErrNotFound)
	operations, err := store.q.ListRoleOperations(t.Context(), pgUUID(created.ID))
	require.NoError(t, err)
	assert.Empty(t, operations, "role_operations must cascade with the role")
}

// TestDeleteRole_BuiltinRole_IsRefusedByTheStatement proves the store's
// own `AND NOT builtin` backstop: even called directly, with the handler's
// read-first check bypassed entirely, a built-in survives.
func TestDeleteRole_BuiltinRole_IsRefusedByTheStatement(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	err := store.DeleteRole(t.Context(), "author")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound, "the statement matches no row, which this store reports as not-found")
	role, err := store.GetRole(t.Context(), "author")
	require.NoError(t, err, "the built-in author role must still exist")
	assert.True(t, role.Builtin)
}
