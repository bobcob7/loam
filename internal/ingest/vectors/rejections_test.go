package vectors

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/ingest/chunker"
)

// These are loam-qj21's tests for what Persist now reports about a
// rejection, as opposed to how many there were (loam-2d44 covers the
// count). The distinction the ledger needs is per-path, so these tests are
// all about telling ONE path from another and one FAILURE MODE from
// another.

// rejectingStore builds a store mock that rejects exactly the named file
// with rejectErr and answers CountFileChunks with priorChunks for it,
// which is what decides stale vs absent.
func rejectingStore(t *testing.T, file string, rejectErr error, priorChunks int) *storeMock {
	t.Helper()
	st, _ := newFakeStore()
	succeeding := st.ReplaceFileChunksFunc
	st.ReplaceFileChunksFunc = func(ctx context.Context, repoID uuid.UUID, targetBranch, f string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
		if f == file {
			return nil, rejectErr
		}
		return succeeding(ctx, repoID, targetBranch, f, inputs)
	}
	st.CountFileChunksFunc = func(_ context.Context, _ uuid.UUID, _, f string) (int, error) {
		if f == file {
			return priorChunks, nil
		}
		return 0, nil
	}
	return st
}

func persistOne(t *testing.T, st store, files []chunker.FileChunks) (Stats, error) {
	t.Helper()
	ix := New(newFakeEmbedder(), testLogger())
	repoID := uuid.Must(uuid.NewV7())
	prepared, _, err := ix.Prepare(t.Context(), repoID, testBranch, files)
	require.NoError(t, err)
	return ix.Persist(t.Context(), st, repoID, testBranch, prepared)
}

// TestPersist_RejectionsNameTheRightPaths is the fixture question this
// bead has to answer before any of the rest means anything: does the
// reported path identify the file that was actually rejected, or merely
// SOME file?
//
// Two of four files are rejected, and they are the SECOND and FOURTH --
// not the first two, not the last two, not a contiguous run. That rules
// out, by arithmetic rather than by assertion text, an implementation that
// reports the first N paths, the last N, the whole batch, or the files
// that SUCCEEDED (which here are also exactly two, so a count-only test
// could not tell those apart -- see loam-2d44's round-1 fixture for the
// same trap caught the same way).
func TestPersist_RejectionsNameTheRightPaths(t *testing.T) {
	t.Parallel()
	rejectErr := errors.New("boom")
	st, _ := newFakeStore()
	succeeding := st.ReplaceFileChunksFunc
	st.ReplaceFileChunksFunc = func(ctx context.Context, repoID uuid.UUID, targetBranch, f string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
		if f == "b.go" || f == "d.go" {
			return nil, rejectErr
		}
		return succeeding(ctx, repoID, targetBranch, f, inputs)
	}

	stats, err := persistOne(t, st, []chunker.FileChunks{
		unitsFor("a.go", "alpha"),
		unitsFor("b.go", "beta"),
		unitsFor("c.go", "gamma"),
		unitsFor("d.go", "delta"),
	})
	require.NoError(t, err)

	require.Len(t, stats.Rejected, 2)
	assert.Equal(t, []string{"b.go", "d.go"}, []string{stats.Rejected[0].Path, stats.Rejected[1].Path},
		"the reported paths must be the ones actually rejected, in attempt order -- the survivors are also two files, so a count cannot tell these apart")
	assert.Equal(t, 2, stats.FilesReplaced, "the survivor count is deliberately the same number, so it cannot stand in for the rejected paths")
}

// TestPersist_RejectedCountMatchesTheNamedPaths pins the invariant
// Stats.Rejected's doc comment claims. The two fields have different
// consumers -- the count reaches ingest_jobs.stats, the paths reach the
// ledger -- and nothing but this test stops them drifting apart, which
// would leave an operator with a job reporting three casualties and a
// ledger naming one.
func TestPersist_RejectedCountMatchesTheNamedPaths(t *testing.T) {
	t.Parallel()
	st := rejectingStore(t, testFileB, errors.New("boom"), 0)

	stats, err := persistOne(t, st, []chunker.FileChunks{
		unitsFor(testFileA, "alpha"), unitsFor(testFileB, "beta"), unitsFor("pkg/c/c.go", "gamma"),
	})
	require.NoError(t, err)

	assert.Equal(t, stats.FilesRejected, len(stats.Rejected),
		"FilesRejected and len(Rejected) describe the same event and must never disagree")
}

// TestPersist_RejectedFileWithPriorChunksIsStale and its sibling below are
// the two failure modes the ledger has to keep apart, and they are the
// reason Persist asks the store a question instead of inferring one.
//
// STALE: the file was already indexed. ReplaceFileChunks' delete and its
// inserts unwind together to the per-file savepoint, so the prior chunks
// survive whole and the file is still searchable -- at an older commit than
// the one the ingested ref is about to claim.
func TestPersist_RejectedFileWithPriorChunksIsStale(t *testing.T) {
	t.Parallel()
	st := rejectingStore(t, testFileA, errors.New("boom"), 4)

	stats, err := persistOne(t, st, []chunker.FileChunks{unitsFor(testFileA, "one")})
	require.NoError(t, err)

	require.Len(t, stats.Rejected, 1)
	assert.Equal(t, chunkstore.ChunksStale, stats.Rejected[0].ChunksState,
		"surviving prior chunks mean the file is stale, not absent: it still answers searches, with older content")
}

// ABSENT: there were no prior chunks to survive -- a first ingest, or a
// full rebuild whose repo-scoped drop already ran in this transaction
// OUTSIDE the savepoints, so nothing unwound it. The file is not in RAG
// search at all, which is the more urgent of the two.
func TestPersist_RejectedFileWithNoPriorChunksIsAbsent(t *testing.T) {
	t.Parallel()
	st := rejectingStore(t, testFileA, errors.New("boom"), 0)

	stats, err := persistOne(t, st, []chunker.FileChunks{unitsFor(testFileA, "one")})
	require.NoError(t, err)

	require.Len(t, stats.Rejected, 1)
	assert.Equal(t, chunkstore.ChunksAbsent, stats.Rejected[0].ChunksState)
}

// TestPersist_RecordsTheSQLStateWhenThereIsOne. The code is what lets an
// operator classify a rejection without parsing prose -- 22P02 is a bad
// value (pgvector's NaN), a 23xxx is a constraint, a 54xxx is a limit, and
// they call for different responses.
func TestPersist_RecordsTheSQLStateWhenThereIsOne(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: pgerrcode.InvalidTextRepresentation, Message: "NaN not allowed in vector"}
	st := rejectingStore(t, testFileA, pgErr, 0)

	stats, err := persistOne(t, st, []chunker.FileChunks{unitsFor(testFileA, "one")})
	require.NoError(t, err)

	require.Len(t, stats.Rejected, 1)
	assert.Equal(t, "22P02", stats.Rejected[0].SQLState,
		"pgvector's NaN rejection is SQLSTATE 22P02, and it must survive into the ledger")
	assert.ErrorIs(t, stats.Rejected[0].Err, pgErr, "the original error must still be reachable, not flattened to its code")
}

// TestPersist_NoSQLStateForANonPostgresError is the control. A store that
// is not Postgres -- a caller's own fake, or a future implementation --
// produces no PgError, and reporting an invented code would be worse than
// reporting none.
func TestPersist_NoSQLStateForANonPostgresError(t *testing.T) {
	t.Parallel()
	st := rejectingStore(t, testFileA, errors.New("some store said no"), 0)

	stats, err := persistOne(t, st, []chunker.FileChunks{unitsFor(testFileA, "one")})
	require.NoError(t, err)

	require.Len(t, stats.Rejected, 1)
	assert.Empty(t, stats.Rejected[0].SQLState)
}

// TestPersist_ClassificationFailureAbortsTheBatch is the deliberate
// narrowing of Persist's tolerance, and it is worth a test of its own
// because it looks like a regression: a rejection alone does NOT fail the
// batch, and here one does.
//
// The reason is the whole design. Tolerating a rejection is only safe
// because the ledger will bring the file back, and the ledger cannot be
// written honestly without knowing whether the file is stale or absent.
// Committing anyway would advance the ingested ref past a file with no
// usable record of it -- exactly the defect. Failing instead rolls the swap
// back, leaving the previous index and the previous ingested ref intact, so
// the next attempt re-plans the same work.
func TestPersist_ClassificationFailureAbortsTheBatch(t *testing.T) {
	t.Parallel()
	countErr := errors.New("cannot count: transaction is in a bad way")
	rejectErr := errors.New("the original rejection")
	st, _ := newFakeStore()
	succeeding := st.ReplaceFileChunksFunc
	st.ReplaceFileChunksFunc = func(ctx context.Context, repoID uuid.UUID, targetBranch, f string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
		if f == testFileA {
			return nil, rejectErr
		}
		return succeeding(ctx, repoID, targetBranch, f, inputs)
	}
	st.CountFileChunksFunc = func(context.Context, uuid.UUID, string, string) (int, error) { return 0, countErr }

	_, err := persistOne(t, st, []chunker.FileChunks{unitsFor(testFileA, "one"), unitsFor(testFileB, "two")})

	require.Error(t, err, "a rejection that cannot be classified must fail the ingest rather than commit an unrecorded one")
	assert.ErrorIs(t, err, countErr)
	assert.Contains(t, err.Error(), testFileA, "the error must name the file being classified")
	assert.Contains(t, err.Error(), "the original rejection",
		"the rejection that triggered the classification must be carried along, or the operator sees only the symptom")
	require.Len(t, st.CountFileChunksCalls(), 1, "the batch stops at the file it could not classify")
}

// TestPersist_NoRejectionsAsksTheStoreNothingExtra is the cost control.
// The classification query runs per REJECTION, and rejections are rare; a
// healthy batch must pay nothing at all for this mechanism, because the
// swap transaction's whole design goal is to be short.
func TestPersist_NoRejectionsAsksTheStoreNothingExtra(t *testing.T) {
	t.Parallel()
	st, _ := newFakeStore()

	stats, err := persistOne(t, st, []chunker.FileChunks{unitsFor(testFileA, "one"), unitsFor(testFileB, "two")})
	require.NoError(t, err)

	assert.Empty(t, stats.Rejected)
	assert.Empty(t, st.CountFileChunksCalls(),
		"no rejection means no classification query -- one per file would be a per-ingest tax paid by every healthy repo")
}

// TestStatsMerge_ConcatenatesTheRejectedPaths. Stats.merge is an addition
// for every counter, and a slice cannot be added -- it has to be appended,
// and appending in the wrong direction or not at all would leave
// IngestFileChunks (the combined entry point) reporting a non-zero
// FilesRejected with no paths to go with it.
func TestStatsMerge_ConcatenatesTheRejectedPaths(t *testing.T) {
	t.Parallel()
	s := Stats{FilesRejected: 1, Rejected: []Rejection{{Path: "first.go"}}}

	s.merge(Stats{FilesRejected: 2, Rejected: []Rejection{{Path: "second.go"}, {Path: "third.go"}}})

	assert.Equal(t, 3, s.FilesRejected)
	assert.Equal(t, []string{"first.go", "second.go", "third.go"},
		[]string{s.Rejected[0].Path, s.Rejected[1].Path, s.Rejected[2].Path})
}
