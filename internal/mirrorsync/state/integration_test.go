//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag, so CI stays green without one (loam-66a tracks wiring it into CI).
// Run explicitly with:
//
//	go test -tags=integration ./internal/mirrorsync/state/... -run TestReporter -v
//
// On podman (e.g. a `podman machine` forwarding /var/run/docker.sock), also
// set TESTCONTAINERS_RYUK_DISABLED=true (see internal/db/migrations's
// integration_test.go for why: podman's Docker-compat API does not resolve
// the reaper sidecar's expected `bridge` network, so the container never
// starts without it). This is a local convenience only, not a CI setting.
//
// DEFERRED-WIP: enrollment.feature: Enrolled repos report sync status ->
// TestReporter_IdleOnSuccessSetsLastSyncedAtAndClearsError,
// TestReporter_ErrorCarriesTheMessage (covers the persistence side of
// repos.sync_state for the non-ingest steps (1,2,3,5) only. Does NOT cover
// the ingest-owned half of the column, written by loam-c94.13's worker when
// step 4 enqueues a job, nor "I view the repo" -- the web-visible sync
// status surface, which docs/testing-spec.md :60-67 drives through the
// connect-go admin client rather than this package. The godog scenario
// stays @wip pending loam-li0.5.)
//
// DEFERRED-WIP: sync.feature: Sync failures are retried on the next cycle ->
// TestReporter_ErrorCarriesTheMessage, TestReporter_IdleOnSuccessSetsLastSyncedAtAndClearsError
// (this is the persistence-of-repos.sync_state half that
// internal/mirrorsync/scheduler_test.go:283 (giq.1) explicitly disclaimed as
// "owned by giq.9" -- landed here. Still does NOT cover the real "upstream
// forge unreachable" condition (giq.2 + fake-forge) or the web-visible sync
// status (testing-spec :60-67). The godog scenario stays @wip.)
package state

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/mirrorsync"
)

// syncRow is the slice of repos this package's tests read back after each
// Reporter call, to check the actual column values Postgres now holds
// rather than trust the reporter's own success return.
type syncRow struct {
	state        string
	lastSyncedAt *string
	syncError    *string
}

func readSyncRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) syncRow {
	t.Helper()
	var row syncRow
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT sync_state, last_synced_at::text, sync_error FROM repos WHERE name = $1`, name,
	).Scan(&row.state, &row.lastSyncedAt, &row.syncError))
	return row
}

// insertRepo inserts a minimal repos row (idle, no last_synced_at, no
// sync_error -- the migration's own defaults) under name, the identifier
// Reporter itself keys writes by.
func insertRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
		 VALUES (gen_random_uuid(), $1, 'https://example.com/repo.git', 'example.com', 'main')`,
		name,
	)
	require.NoError(t, err)
}

// newTestPool spins up a real Postgres via testcontainers-go, applies the
// production migration set (so this test proves Reporter against the
// authoritative schema, not a hand-copied one) and returns a connected pool,
// registering cleanup.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, "postgres:16-alpine",
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
	return pool
}

// TestReporter_SyncingSetAtCycleStart proves ReportSyncing writes
// sync_state = 'syncing' against a real column, on a repo that starts idle.
func TestReporter_SyncingSetAtCycleStart(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	const name = "group/syncing-repo"
	insertRepo(ctx, t, pool, name)
	r := New(pool)
	require.NoError(t, r.ReportSyncing(ctx, mirrorsync.RepoID(name)))
	row := readSyncRow(ctx, t, pool, name)
	assert.Equal(t, "syncing", row.state)
	assert.Nil(t, row.lastSyncedAt)
}

// TestReporter_IdleOnSuccessSetsLastSyncedAtAndClearsError proves the
// syncing -> idle transition (docs/sync-spec.md :85) writes last_synced_at
// and clears any sync_error left over from a prior failed cycle, when all
// non-ingest steps completed without error and no ingest was enqueued.
func TestReporter_IdleOnSuccessSetsLastSyncedAtAndClearsError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	const name = "group/idle-repo"
	insertRepo(ctx, t, pool, name)
	_, err := pool.Exec(ctx, `UPDATE repos SET sync_state = 'error', sync_error = 'stale failure' WHERE name = $1`, name)
	require.NoError(t, err)
	r := New(pool)
	require.NoError(t, r.ReportSyncing(ctx, mirrorsync.RepoID(name)))
	require.NoError(t, r.ReportIdle(ctx, mirrorsync.RepoID(name), false))
	row := readSyncRow(ctx, t, pool, name)
	assert.Equal(t, "idle", row.state)
	require.NotNil(t, row.lastSyncedAt)
	assert.NotEmpty(t, *row.lastSyncedAt)
	assert.Nil(t, row.syncError, "a successful cycle must clear a stale error message")
}

// TestReporter_ErrorCarriesTheMessage proves the syncing -> error transition
// records the failing step's message in sync_error, per docs/sync-spec.md
// :85 ("error with the message on failure").
func TestReporter_ErrorCarriesTheMessage(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	const name = "group/error-repo"
	insertRepo(ctx, t, pool, name)
	r := New(pool)
	require.NoError(t, r.ReportSyncing(ctx, mirrorsync.RepoID(name)))
	cycleErr := errors.New("fetching repo group/error-repo: connection refused")
	require.NoError(t, r.ReportError(ctx, mirrorsync.RepoID(name), cycleErr, false))
	row := readSyncRow(ctx, t, pool, name)
	assert.Equal(t, "error", row.state)
	require.NotNil(t, row.syncError)
	assert.Equal(t, cycleErr.Error(), *row.syncError)
}

// TestReporter_OwnershipTransfer_IdleDoesNotClobberIngestWorker is this
// bead's core acceptance: when enqueuedIngest is true, ReportIdle must not
// write the terminal state, because ownership of sync_state for that tick
// has already passed to the ingest worker (loam-c94.13). This test proves
// it end to end against real Postgres: it sets syncing, calls ReportIdle
// with enqueuedIngest = true, asserts the row is UNCHANGED (still syncing,
// no last_synced_at), then simulates the ingest worker's own write landing
// afterward and asserts THAT write is what the row reflects -- i.e.
// Reporter left the column alone for the real owner to write, rather than
// racing it.
func TestReporter_OwnershipTransfer_IdleDoesNotClobberIngestWorker(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	const name = "group/handoff-idle-repo"
	insertRepo(ctx, t, pool, name)
	r := New(pool)
	require.NoError(t, r.ReportSyncing(ctx, mirrorsync.RepoID(name)))
	require.NoError(t, r.ReportIdle(ctx, mirrorsync.RepoID(name), true))
	row := readSyncRow(ctx, t, pool, name)
	assert.Equal(t, "syncing", row.state, "ReportIdle must not write the terminal state once ownership has transferred")
	assert.Nil(t, row.lastSyncedAt)
	_, err := pool.Exec(ctx,
		`UPDATE repos SET sync_state = 'idle', last_synced_at = now() WHERE name = $1`, name)
	require.NoError(t, err, "simulated ingest-worker write")
	row = readSyncRow(ctx, t, pool, name)
	assert.Equal(t, "idle", row.state, "the ingest worker's own write must land untouched by Reporter")
	assert.NotNil(t, row.lastSyncedAt)
}

// TestReporter_OwnershipTransfer_ErrorDoesNotClobberIngestWorker is the
// error-path mirror: step 4 can succeed (handing off ownership) while step 5
// subsequently fails, so ReportError must also honor the hand-off.
func TestReporter_OwnershipTransfer_ErrorDoesNotClobberIngestWorker(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	const name = "group/handoff-error-repo"
	insertRepo(ctx, t, pool, name)
	r := New(pool)
	require.NoError(t, r.ReportSyncing(ctx, mirrorsync.RepoID(name)))
	require.NoError(t, r.ReportError(ctx, mirrorsync.RepoID(name), errors.New("polling PRs: timeout"), true))
	row := readSyncRow(ctx, t, pool, name)
	assert.Equal(t, "syncing", row.state, "ReportError must not write the terminal state once ownership has transferred")
	assert.Nil(t, row.syncError)
	_, err := pool.Exec(ctx,
		`UPDATE repos SET sync_state = 'error', sync_error = 'ingest job failed' WHERE name = $1`, name)
	require.NoError(t, err, "simulated ingest-worker write")
	row = readSyncRow(ctx, t, pool, name)
	assert.Equal(t, "error", row.state)
	require.NotNil(t, row.syncError)
	assert.Equal(t, "ingest job failed", *row.syncError)
}

// TestReporter_RepoNotFound proves a report against an unknown RepoID
// surfaces errRepoNotFound instead of silently doing nothing.
func TestReporter_RepoNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	r := New(pool)
	err := r.ReportSyncing(ctx, mirrorsync.RepoID("group/does-not-exist"))
	require.Error(t, err)
	assert.ErrorIs(t, err, errRepoNotFound)
}
