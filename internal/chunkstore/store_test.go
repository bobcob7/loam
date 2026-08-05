package chunkstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/db/gen"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// realTx wires transactorMock to actually invoke fn against q, the shape
// every test that exercises ReplaceFileChunks's happy and error paths uses
// -- it stands in for pgxTransactor without needing a real pgx.Tx.
func realTx(q queries) *transactorMock {
	return &transactorMock{
		withinTxFunc: func(_ context.Context, fn func(q queries) error) error {
			return fn(q)
		},
	}
}

func TestReplaceFileChunks_DeletesThenInsertsInOrder(t *testing.T) {
	t.Parallel()
	var order []string
	repoID := uuid.New()
	q := &queriesMock{
		DeleteChunksByFileFunc: func(_ context.Context, arg gen.DeleteChunksByFileParams) error {
			order = append(order, "delete")
			assert.Equal(t, pgUUID(repoID), arg.RepoID)
			assert.Equal(t, "main", arg.TargetBranch)
			assert.Equal(t, "a.go", arg.File)
			return nil
		},
		InsertChunkFunc: func(_ context.Context, arg gen.InsertChunkParams) (gen.Chunk, error) {
			order = append(order, "insert:"+arg.Content)
			return gen.Chunk{
				ID:           arg.ID,
				RepoID:       arg.RepoID,
				TargetBranch: arg.TargetBranch,
				File:         arg.File,
				StartLine:    arg.StartLine,
				EndLine:      arg.EndLine,
				Content:      arg.Content,
				Embedding:    arg.Embedding,
				CreatedAt:    pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
			}, nil
		},
	}
	s := newStore(q, realTx(q), testLogger())
	inputs := []ChunkInput{
		{StartLine: 1, EndLine: 2, Content: "first", Embedding: []float32{1, 0}},
		{StartLine: 3, EndLine: 4, Content: "second", Embedding: []float32{0, 1}},
	}
	result, err := s.ReplaceFileChunks(t.Context(), repoID, "main", "a.go", inputs)
	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, []string{"delete", "insert:first", "insert:second"}, order,
		"delete must run before any insert, and inserts must preserve input order")
	assert.Equal(t, repoID, result[0].RepoID)
	assert.Equal(t, "first", result[0].Content)
	assert.Equal(t, []float32{1, 0}, result[0].Embedding)
	assert.NotEqual(t, uuid.Nil, result[0].ID)
	assert.NotEqual(t, result[0].ID, result[1].ID, "each inserted chunk must get a distinct id")
}

func TestReplaceFileChunks_EmptyInputs_DeletesOnlyAndReturnsEmpty(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	deleteCalls := 0
	q := &queriesMock{
		DeleteChunksByFileFunc: func(_ context.Context, _ gen.DeleteChunksByFileParams) error {
			deleteCalls++
			return nil
		},
		InsertChunkFunc: func(_ context.Context, _ gen.InsertChunkParams) (gen.Chunk, error) {
			t.Fatal("InsertChunk must not be called when inputs is empty")
			return gen.Chunk{}, nil
		},
	}
	s := newStore(q, realTx(q), testLogger())
	result, err := s.ReplaceFileChunks(t.Context(), repoID, "main", "removed.go", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, deleteCalls)
	assert.Empty(t, result)
}

func TestReplaceFileChunks_DeleteFails_WrapsErrorWithoutInserting(t *testing.T) {
	t.Parallel()
	deleteErr := errors.New("connection reset")
	insertCalled := false
	q := &queriesMock{
		DeleteChunksByFileFunc: func(_ context.Context, _ gen.DeleteChunksByFileParams) error {
			return deleteErr
		},
		InsertChunkFunc: func(_ context.Context, _ gen.InsertChunkParams) (gen.Chunk, error) {
			insertCalled = true
			return gen.Chunk{}, nil
		},
	}
	s := newStore(q, realTx(q), testLogger())
	inputs := []ChunkInput{{StartLine: 1, EndLine: 2, Content: "x", Embedding: []float32{1}}}
	result, err := s.ReplaceFileChunks(t.Context(), uuid.New(), "main", "a.go", inputs)
	require.Error(t, err)
	assert.ErrorIs(t, err, deleteErr)
	assert.Nil(t, result)
	assert.False(t, insertCalled, "a failed delete must short-circuit before any insert runs")
}

func TestReplaceFileChunks_InsertFails_WrapsError(t *testing.T) {
	t.Parallel()
	insertErr := errors.New("vector cannot have more than 16000 dimensions")
	q := &queriesMock{
		DeleteChunksByFileFunc: func(_ context.Context, _ gen.DeleteChunksByFileParams) error {
			return nil
		},
		InsertChunkFunc: func(_ context.Context, _ gen.InsertChunkParams) (gen.Chunk, error) {
			return gen.Chunk{}, insertErr
		},
	}
	s := newStore(q, realTx(q), testLogger())
	inputs := []ChunkInput{{StartLine: 1, EndLine: 2, Content: "x", Embedding: []float32{1}}}
	result, err := s.ReplaceFileChunks(t.Context(), uuid.New(), "main", "a.go", inputs)
	require.Error(t, err)
	assert.ErrorIs(t, err, insertErr)
	assert.Nil(t, result)
}

func TestReplaceFileChunks_TransactionFails_PropagatesError(t *testing.T) {
	t.Parallel()
	beginErr := errors.New("beginning transaction: connection refused")
	// deleteCalled must stay false: ReplaceFileChunks must route every write
	// through s.tx.withinTx, never call s.queries directly. A queriesMock
	// with real (non-nil) funcs, rather than a bare &queriesMock{}, means a
	// mutation that bypasses withinTx fails this test by the assertion
	// below instead of panicking on a nil mock func -- a panic would abort
	// the whole test binary and mask every other test in the package,
	// which is strictly worse evidence than an assertion failure.
	deleteCalled := false
	q := &queriesMock{
		DeleteChunksByFileFunc: func(_ context.Context, _ gen.DeleteChunksByFileParams) error {
			deleteCalled = true
			return nil
		},
	}
	tx := &transactorMock{
		withinTxFunc: func(_ context.Context, _ func(q queries) error) error {
			return beginErr
		},
	}
	s := newStore(q, tx, testLogger())
	result, err := s.ReplaceFileChunks(t.Context(), uuid.New(), "main", "a.go", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, beginErr)
	assert.Nil(t, result)
	assert.False(t, deleteCalled, "writes must run inside withinTx, never against the pool directly")
}

func TestReplaceFileChunks_InsertedChunkHasInvalidID_ReturnsErrInvalidUUID(t *testing.T) {
	t.Parallel()
	q := &queriesMock{
		DeleteChunksByFileFunc: func(_ context.Context, _ gen.DeleteChunksByFileParams) error { return nil },
		InsertChunkFunc: func(_ context.Context, arg gen.InsertChunkParams) (gen.Chunk, error) {
			return gen.Chunk{ID: pgtype.UUID{Valid: false}, RepoID: arg.RepoID}, nil
		},
	}
	s := newStore(q, realTx(q), testLogger())
	inputs := []ChunkInput{{StartLine: 1, EndLine: 2, Content: "x", Embedding: []float32{1}}}
	result, err := s.ReplaceFileChunks(t.Context(), uuid.New(), "main", "a.go", inputs)
	require.Error(t, err)
	assert.ErrorIs(t, err, errInvalidUUID)
	assert.Nil(t, result)
}

func TestSearch_EmptyRepoIDs_ReturnsNilWithoutQuerying(t *testing.T) {
	t.Parallel()
	q := &queriesMock{
		SearchChunksByEmbeddingScopedFunc: func(_ context.Context, _ gen.SearchChunksByEmbeddingScopedParams) ([]gen.Chunk, error) {
			t.Fatal("SearchChunksByEmbeddingScoped must not be called for an empty repo-id scope")
			return nil, nil
		},
	}
	s := newStore(q, realTx(q), testLogger())
	result, err := s.Search(t.Context(), nil, "main", []float32{1, 0}, 5)
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestSearch_NonPositiveLimit_ReturnsNilWithoutQuerying(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		q := &queriesMock{
			SearchChunksByEmbeddingScopedFunc: func(_ context.Context, _ gen.SearchChunksByEmbeddingScopedParams) ([]gen.Chunk, error) {
				t.Fatalf("SearchChunksByEmbeddingScoped must not be called for limit=%d", limit)
				return nil, nil
			},
		}
		s := newStore(q, realTx(q), testLogger())
		result, err := s.Search(t.Context(), []uuid.UUID{uuid.New()}, "main", []float32{1, 0}, limit)
		require.NoError(t, err)
		assert.Nil(t, result)
	}
}

func TestSearch_BuildsScopedQueryAndPreservesOrder(t *testing.T) {
	t.Parallel()
	repoA := uuid.New()
	repoB := uuid.New()
	var gotArg gen.SearchChunksByEmbeddingScopedParams
	q := &queriesMock{
		SearchChunksByEmbeddingScopedFunc: func(_ context.Context, arg gen.SearchChunksByEmbeddingScopedParams) ([]gen.Chunk, error) {
			gotArg = arg
			return []gen.Chunk{
				chunkRow(repoA, "near.go", []float32{1, 0}),
				chunkRow(repoB, "far.go", []float32{0, 1}),
			}, nil
		},
	}
	s := newStore(q, realTx(q), testLogger())
	result, err := s.Search(t.Context(), []uuid.UUID{repoA, repoB}, "main", []float32{1, 0}, 2)
	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, "near.go", result[0].File, "Search must not reorder rows -- the HNSW ORDER BY already ranks them nearest-first")
	assert.Equal(t, "far.go", result[1].File)
	assert.Equal(t, []pgtype.UUID{pgUUID(repoA), pgUUID(repoB)}, gotArg.Column1)
	assert.Equal(t, "main", gotArg.TargetBranch)
	assert.Equal(t, int32(2), gotArg.Limit)
	assert.Equal(t, []float32{1, 0}, gotArg.Embedding.Slice())
}

func TestSearch_QueryFails_WrapsError(t *testing.T) {
	t.Parallel()
	queryErr := errors.New("cannot scan unknown type")
	q := &queriesMock{
		SearchChunksByEmbeddingScopedFunc: func(_ context.Context, _ gen.SearchChunksByEmbeddingScopedParams) ([]gen.Chunk, error) {
			return nil, queryErr
		},
	}
	s := newStore(q, realTx(q), testLogger())
	result, err := s.Search(t.Context(), []uuid.UUID{uuid.New()}, "main", []float32{1}, 1)
	require.Error(t, err)
	assert.ErrorIs(t, err, queryErr)
	assert.Nil(t, result)
}

func TestSearch_RowWithInvalidRepoID_ReturnsErrInvalidUUID(t *testing.T) {
	t.Parallel()
	q := &queriesMock{
		SearchChunksByEmbeddingScopedFunc: func(_ context.Context, _ gen.SearchChunksByEmbeddingScopedParams) ([]gen.Chunk, error) {
			return []gen.Chunk{{ID: pgUUID(uuid.New()), RepoID: pgtype.UUID{Valid: false}}}, nil
		},
	}
	s := newStore(q, realTx(q), testLogger())
	result, err := s.Search(t.Context(), []uuid.UUID{uuid.New()}, "main", []float32{1}, 1)
	require.Error(t, err)
	assert.ErrorIs(t, err, errInvalidUUID)
	assert.Nil(t, result)
}

// recordingExec returns a savepointExecer mock that records every statement
// it is asked to run and fails the ones fail names, so a test can pin the
// exact savepoint sequence rather than a count of calls.
func recordingExec(fail map[string]error) (*savepointExecerMock, *[]string) {
	var issued []string
	return &savepointExecerMock{
		ExecFunc: func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
			issued = append(issued, sql)
			return pgconn.CommandTag{}, fail[sql]
		},
	}, &issued
}

const (
	sqlSavepoint  = "SAVEPOINT loam_chunkstore_file"
	sqlRollbackTo = "ROLLBACK TO SAVEPOINT loam_chunkstore_file"
	sqlRelease    = "RELEASE SAVEPOINT loam_chunkstore_file"
)

// TestSavepointTransactor_Success_EstablishesThenReleasesAroundFn pins the
// happy-path statement sequence exactly, in order: nothing before the
// SAVEPOINT, nothing between it and fn, and a RELEASE after. Asserting the
// statements rather than a call count is what makes this test able to
// notice a savepoint that is established but never released -- which would
// leak one per file across a batch and is otherwise completely silent.
//
// It also keeps the contract its predecessor
// (TestPassthroughTransactor_RunsFnExactlyOnceAgainstBoundQueries) covered:
// fn runs exactly once, against exactly the bound queries. A savepoint is
// not a nested transaction, so fn must still see the caller's own
// transaction, not some fresh connection.
func TestSavepointTransactor_Success_EstablishesThenReleasesAroundFn(t *testing.T) {
	t.Parallel()
	q := &queriesMock{}
	ex, issued := recordingExec(nil)
	st := &savepointTransactor{tx: ex, q: q}
	calls := 0
	var got queries
	var atFn []string
	err := st.withinTx(t.Context(), func(q queries) error {
		calls++
		got = q
		atFn = append([]string(nil), *issued...)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "withinTx must invoke fn exactly once -- no retry, no nested call")
	assert.Same(t, queries(q), got, "withinTx must hand fn the exact queries it was constructed with, not a fresh one")
	assert.Equal(t, []string{sqlSavepoint}, atFn, "the savepoint must be established BEFORE fn runs, or it protects nothing")
	assert.Equal(t, []string{sqlSavepoint, sqlRelease}, *issued)
}

// TestSavepointTransactor_FnError_UnwindsToTheSavepointAndReturnsTheErrorBare
// is the whole point of loam-c94.24 in miniature. Two things must be true
// together: the failed call's statements are unwound (ROLLBACK TO), and the
// savepoint is then destroyed (RELEASE) so a batch of rejections cannot
// accumulate savepoints on the server -- ROLLBACK TO leaves the savepoint
// established, it does not remove it.
//
// The returned error must be fn's own, UNWRAPPED by any
// ErrTransactionUnusable: that is the signal a batch loop reads as "only
// this file failed, keep going" (internal/ingest/vectors.Persist), so
// wrapping it here would silently turn every rejection back into a
// whole-batch abort.
func TestSavepointTransactor_FnError_UnwindsToTheSavepointAndReturnsTheErrorBare(t *testing.T) {
	t.Parallel()
	fnErr := errors.New("boom")
	ex, issued := recordingExec(nil)
	st := &savepointTransactor{tx: ex, q: &queriesMock{}}
	err := st.withinTx(t.Context(), func(_ queries) error { return fnErr })
	assert.ErrorIs(t, err, fnErr)
	assert.NotErrorIs(t, err, ErrTransactionUnusable,
		"a rejection this store successfully unwound leaves the transaction usable, and must not be reported as if it did not")
	assert.Equal(t, []string{sqlSavepoint, sqlRollbackTo, sqlRelease}, *issued)
}

// The three savepoint statements are themselves statements that must reach
// the server, so each one failing is proof the transaction (or the
// connection under it) is gone -- the one classification that cannot be
// made by inspecting an error's shape, and the reason
// ErrTransactionUnusable exists (loam-c94.24). Table-driven over all three
// positions because it is the position most likely to be missed in a later
// edit, not the sentinel, that this guards.
func TestSavepointTransactor_SavepointStatementFails_ReportsTheTransactionUnusable(t *testing.T) {
	t.Parallel()
	execErr := errors.New("write tcp 127.0.0.1:5432: broken pipe")
	fnErr := errors.New("invalid byte sequence for encoding \"UTF8\"")
	for _, tc := range []struct {
		name     string
		failing  string
		fnErr    error
		wantRan  bool
		wantSQL  []string
		wantText string
	}{
		{
			name:    "establishing",
			failing: sqlSavepoint,
			wantSQL: []string{sqlSavepoint},
		},
		{
			name:     "releasing after a successful fn",
			failing:  sqlRelease,
			wantRan:  true,
			wantSQL:  []string{sqlSavepoint, sqlRelease},
			wantText: "releasing savepoint",
		},
		{
			name:     "rolling back after a failed fn",
			failing:  sqlRollbackTo,
			fnErr:    fnErr,
			wantRan:  true,
			wantSQL:  []string{sqlSavepoint, sqlRollbackTo},
			wantText: fnErr.Error(),
		},
		{
			name:     "releasing after a rolled-back fn",
			failing:  sqlRelease,
			fnErr:    fnErr,
			wantRan:  true,
			wantSQL:  []string{sqlSavepoint, sqlRollbackTo, sqlRelease},
			wantText: fnErr.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ex, issued := recordingExec(map[string]error{tc.failing: execErr})
			st := &savepointTransactor{tx: ex, q: &queriesMock{}}
			ran := false
			err := st.withinTx(t.Context(), func(_ queries) error {
				ran = true
				return tc.fnErr
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrTransactionUnusable, "a savepoint statement that could not be driven means the transaction is gone, not that one file was rejected")
			assert.ErrorIs(t, err, execErr, "the driver's own error must stay in the chain for the operator")
			assert.Equal(t, tc.wantRan, ran, "fn must run if and only if the savepoint was established first")
			assert.Equal(t, tc.wantSQL, *issued, "the sequence must stop at the statement that failed -- continuing to issue statements on a dead transaction is exactly the cascade this sentinel exists to prevent")
			if tc.wantText != "" {
				assert.Contains(t, err.Error(), tc.wantText, "the message must still name what was being unwound, or an operator sees only the symptom")
			}
		})
	}
}

func chunkRow(repoID uuid.UUID, file string, embedding []float32) gen.Chunk {
	return gen.Chunk{
		ID:           pgUUID(uuid.New()),
		RepoID:       pgUUID(repoID),
		TargetBranch: "main",
		File:         file,
		StartLine:    1,
		EndLine:      1,
		Content:      "content",
		Embedding:    pgvector.NewVector(embedding),
		CreatedAt:    pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
	}
}
