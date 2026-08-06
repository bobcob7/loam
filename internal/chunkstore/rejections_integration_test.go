//go:build integration

// See integration_test.go's header for the shared container, the shared
// DSN, and the podman/ryuk note; this file reuses all three. Run with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/chunkstore/... -v
//
// These are loam-qj21's tests for the parts of the rejection ledger that
// LIVE IN SQL and are therefore untestable against a mock. The attempt
// count and the pending/exhausted decision are computed by one upsert
// statement's own CASE expression, deliberately, so that `attempts + 1`
// and the state derived from it can never disagree -- which means a Go
// test that stubs the query proves nothing about either.
package chunkstore

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordN records the same path n times and returns the row after the last
// one, so a test can walk the attempt budget without repeating the call.
func recordN(t *testing.T, l *Rejections, repoID uuid.UUID, file string, n int) Rejection {
	t.Helper()
	var last Rejection
	for i := range n {
		require.NoError(t, l.Record(t.Context(), repoID, "main", RejectionInput{
			File:        file,
			ChunksState: ChunksAbsent,
			SQLState:    "22000",
			Error:       "NaN not allowed in vector",
			JobID:       uuid.Must(uuid.NewV7()),
			RejectedRef: "ref-" + string(rune('a'+i)),
		}))
		last = rowFor(t, l, repoID, file)
	}
	return last
}

func rowFor(t *testing.T, l *Rejections, repoID uuid.UUID, file string) Rejection {
	t.Helper()
	rows, err := l.List(t.Context(), repoID, "main")
	require.NoError(t, err)
	for _, r := range rows {
		if r.File == file {
			return r
		}
	}
	t.Fatalf("no ledger row for %s", file)
	return Rejection{}
}

// TestRejections_UpsertClimbsToTheCeilingAndStopsThere is the attempt
// budget as the database actually enforces it, walked one rejection at a
// time to the exact ceiling and one past it.
//
// Every attempt count is asserted EXACTLY, never "increasing": an
// implementation that exhausted on the first rejection and one that never
// exhausted at all would both satisfy a monotonicity check, and they are
// the two ways this can be wrong.
func TestRejections_UpsertClimbsToTheCeilingAndStopsThere(t *testing.T) {
	t.Parallel()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(t.Context(), t, pool, "group/ledger-ceiling-"+uuid.NewString())
	l := NewRejections(pool, rejectionsLogger())

	for attempt := 1; attempt <= MaxRejectionAttempts; attempt++ {
		row := recordN(t, l, repoID, "hopeless.go", 1)
		assert.Equal(t, attempt, row.Attempts, "the upsert must increment by exactly one per rejection")
		if attempt < MaxRejectionAttempts {
			assert.Equal(t, RejectionPending, row.State, "still inside the budget at %d of %d", attempt, MaxRejectionAttempts)
		} else {
			assert.Equal(t, RejectionExhausted, row.State,
				"the state must flip AT the ceiling, not one past it: the CASE compares attempts+1 >= max, and >= vs > is the whole off-by-one")
		}
	}

	// One rejection past the ceiling. It cannot happen through the
	// orchestrator (an exhausted path is not planned) but it can happen
	// through a real diff naming the file, and the row must stay coherent.
	beyond := recordN(t, l, repoID, "hopeless.go", 1)
	assert.Equal(t, MaxRejectionAttempts+1, beyond.Attempts, "the count keeps its honest history")
	assert.Equal(t, RejectionExhausted, beyond.State, "and the state stays exhausted rather than wrapping back to pending")
}

// TestRejections_UpsertKeepsFirstRejectedAtAndRefreshesEverythingElse. The
// onset date is what tells an operator whether they are looking at
// something that started this morning or has been rotting for a month, and
// the upsert is the only thing protecting it -- ON CONFLICT DO UPDATE
// overwrites every column it names, so this is a property of which columns
// the statement deliberately omits.
//
// Everything else must refresh, and chunks_state is the one that matters
// most: it legitimately changes from stale to absent when a full rebuild
// drops the prior chunks an earlier rejection had left intact, and a
// ledger frozen at the first answer would keep telling an operator the
// file was still searchable after it stopped being so.
func TestRejections_UpsertKeepsFirstRejectedAtAndRefreshesEverythingElse(t *testing.T) {
	t.Parallel()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(t.Context(), t, pool, "group/ledger-onset-"+uuid.NewString())
	l := NewRejections(pool, rejectionsLogger())
	firstJob := uuid.Must(uuid.NewV7())
	require.NoError(t, l.Record(t.Context(), repoID, "main", RejectionInput{
		File: "fragile.go", ChunksState: ChunksStale, SQLState: "22000",
		Error: "first failure", JobID: firstJob, RejectedRef: "oldref",
	}))
	first := rowFor(t, l, repoID, "fragile.go")

	secondJob := uuid.Must(uuid.NewV7())
	require.NoError(t, l.Record(t.Context(), repoID, "main", RejectionInput{
		File: "fragile.go", ChunksState: ChunksAbsent, SQLState: "23505",
		Error: "second failure", JobID: secondJob, RejectedRef: "newref",
	}))
	second := rowFor(t, l, repoID, "fragile.go")

	assert.Equal(t, first.FirstRejectedAt, second.FirstRejectedAt,
		"the onset must survive every later rejection: it is the only column that dates the PROBLEM rather than the last symptom")
	assert.True(t, second.LastRejectedAt.After(first.FirstRejectedAt) || second.LastRejectedAt.Equal(first.LastRejectedAt),
		"last_rejected_at tracks the most recent rejection")
	assert.Equal(t, ChunksAbsent, second.ChunksState,
		"a full rebuild can turn a stale file into an absent one, so this column must follow reality rather than latch")
	assert.Equal(t, "23505", second.SQLState, "the newest failure's code replaces the older one")
	assert.Equal(t, "second failure", second.Error)
	assert.Equal(t, secondJob, second.JobID, "and the row points at the job that most recently observed the problem")
	assert.Equal(t, "newref", second.RejectedRef)
	assert.Equal(t, 2, second.Attempts)
}

// TestRejections_ClearRemovesOnlyTheNamedPaths. The clear is an
// `= ANY($3::text[])` delete, which is the one place a wrong parameter
// binding would silently widen into "delete the repo's whole ledger" --
// a mistake that looks exactly like success (the ledger empties) and loses
// every outstanding retry.
func TestRejections_ClearRemovesOnlyTheNamedPaths(t *testing.T) {
	t.Parallel()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(t.Context(), t, pool, "group/ledger-clear-"+uuid.NewString())
	l := NewRejections(pool, rejectionsLogger())
	for _, f := range []string{"a.go", "b.go", "c.go"} {
		recordN(t, l, repoID, f, 1)
	}

	require.NoError(t, l.Clear(t.Context(), repoID, "main", []string{"a.go", "c.go"}))

	rows, err := l.List(t.Context(), repoID, "main")
	require.NoError(t, err)
	require.Len(t, rows, 1, "exactly the two named paths must go: three rows in means the survivor is distinguishable from a whole-table delete")
	assert.Equal(t, "b.go", rows[0].File)
}

// TestRejections_ClearAndListAreScopedToOneBranch. The primary key spans
// (repo_id, target_branch, file), so two branches of one repo can both
// hold a row for the same path -- and a clear driven by one branch's
// ingest must not touch the other's, whose file may be entirely healthy.
func TestRejections_ClearAndListAreScopedToOneBranch(t *testing.T) {
	t.Parallel()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(t.Context(), t, pool, "group/ledger-branch-"+uuid.NewString())
	l := NewRejections(pool, rejectionsLogger())
	recordN(t, l, repoID, "shared.go", 1)
	require.NoError(t, l.Record(t.Context(), repoID, "release", RejectionInput{
		File: "shared.go", ChunksState: ChunksAbsent, Error: "boom", RejectedRef: "r",
	}))

	require.NoError(t, l.Clear(t.Context(), repoID, "main", []string{"shared.go"}))

	main, err := l.List(t.Context(), repoID, "main")
	require.NoError(t, err)
	assert.Empty(t, main)
	release, err := l.List(t.Context(), repoID, "release")
	require.NoError(t, err)
	require.Len(t, release, 1, "the other branch's row for the SAME path must survive")
}

// TestRejections_ClearAllEmptiesOneBranchOfOneRepo is the full-rebuild
// path. It has to be repo-and-branch scoped and no wider: a rebuild of one
// branch says nothing about another's index.
func TestRejections_ClearAllEmptiesOneBranchOfOneRepo(t *testing.T) {
	t.Parallel()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(t.Context(), t, pool, "group/ledger-clearall-"+uuid.NewString())
	otherID := insertRepo(t.Context(), t, pool, "group/ledger-clearall-other-"+uuid.NewString())
	l := NewRejections(pool, rejectionsLogger())
	recordN(t, l, repoID, "a.go", 1)
	recordN(t, l, repoID, "b.go", 1)
	recordN(t, l, otherID, "a.go", 1)

	require.NoError(t, l.ClearAll(t.Context(), repoID, "main"))

	rows, err := l.List(t.Context(), repoID, "main")
	require.NoError(t, err)
	assert.Empty(t, rows)
	otherRows, err := l.List(t.Context(), otherID, "main")
	require.NoError(t, err)
	require.Len(t, otherRows, 1, "another repo's ledger must be untouched")
}

// TestRejections_ClearAllResetsTheAttemptBudget pins a consequence of
// ClearAll that is easy to read as a bug and is deliberate: a full rebuild
// empties the ledger, so a path that rejects again during it starts over
// at one attempt.
//
// The justification is what the ceiling is FOR. It exists to stop a
// hopeless file adding a re-read, a re-chunk and an embedder round trip to
// every INCREMENTAL ingest. A full rebuild re-chunks every file in the
// tree whether or not the ledger asked it to, so retrying under KindFull
// costs nothing the rebuild was not already paying.
func TestRejections_ClearAllResetsTheAttemptBudget(t *testing.T) {
	t.Parallel()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(t.Context(), t, pool, "group/ledger-reset-"+uuid.NewString())
	l := NewRejections(pool, rejectionsLogger())
	exhausted := recordN(t, l, repoID, "hopeless.go", MaxRejectionAttempts)
	require.Equal(t, RejectionExhausted, exhausted.State)

	require.NoError(t, l.ClearAll(t.Context(), repoID, "main"))
	after := recordN(t, l, repoID, "hopeless.go", 1)

	assert.Equal(t, 1, after.Attempts, "the row is new, so its history starts over")
	assert.Equal(t, RejectionPending, after.State)
}

// TestRejections_RepoDeletionCascades. The ledger is derived state about a
// repo, so removing the repo must remove it -- otherwise
// internal/reporemove leaves rows behind that reference a repo id nothing
// can resolve, and they would be retried against a mirror that no longer
// exists.
func TestRejections_RepoDeletionCascades(t *testing.T) {
	t.Parallel()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(t.Context(), t, pool, "group/ledger-cascade-"+uuid.NewString())
	l := NewRejections(pool, rejectionsLogger())
	recordN(t, l, repoID, "a.go", 1)

	_, err := pool.Exec(t.Context(), `DELETE FROM repos WHERE id = $1`, repoID)
	require.NoError(t, err, "the delete must succeed at all: a missing ON DELETE CASCADE would fail it on the FK")

	rows, err := l.List(t.Context(), repoID, "main")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestCountFileChunks_SeesTheCallersOwnUncommittedDrop is the property the
// stale/absent classification rests on, and it is only observable against
// a real transaction.
//
// A full rebuild issues DropRepoBranch inside the swap transaction and
// then writes. If the count that classifies a rejection ran on any other
// connection it would not see that uncommitted delete, would report the
// file as still having chunks, and would ledger a file whose prior chunks
// are already gone as merely "stale" -- understating the damage exactly
// where a full rebuild does the most of it.
func TestCountFileChunks_SeesTheCallersOwnUncommittedDrop(t *testing.T) {
	t.Parallel()
	pool := newRegisteredPool(t, sharedDSN)
	ctx := t.Context()
	repoID := insertRepo(ctx, t, pool, "group/count-in-tx-"+uuid.NewString())
	logger := rejectionsLogger()
	_, err := New(pool, logger).ReplaceFileChunks(ctx, repoID, "main", "a.go", []ChunkInput{
		{StartLine: 1, EndLine: 2, Content: "func A() {}", Embedding: unit(0)},
	})
	require.NoError(t, err)

	standalone, err := New(pool, logger).CountFileChunks(ctx, repoID, "main", "a.go")
	require.NoError(t, err)
	require.Equal(t, 1, standalone, "the chunk must exist before the drop, or the test proves nothing")

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	inTx := NewInTx(tx, logger)
	require.NoError(t, inTx.DropRepoBranch(ctx, repoID, "main"))

	inTxCount, err := inTx.CountFileChunks(ctx, repoID, "main", "a.go")
	require.NoError(t, err)
	assert.Zero(t, inTxCount,
		"inside the transaction the drop is visible, so a rejection here classifies as ABSENT -- which is what a full rebuild actually did to the file")

	outsideCount, err := New(pool, logger).CountFileChunks(ctx, repoID, "main", "a.go")
	require.NoError(t, err)
	assert.Equal(t, 1, outsideCount,
		"and on any other session it is still 1 -- that difference is exactly why the count has to run on the swap's own transaction")
}
