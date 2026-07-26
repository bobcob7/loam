package chunkstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
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

// TestPassthroughTransactor_RunsFnExactlyOnceAgainstBoundQueries proves
// NewInTx's passthrough path does exactly one thing: hand fn the queries it
// was built with and return. There is no Begin/Commit/Rollback call to
// assert the absence of directly (passthroughTransactor has no reference to
// any pgx.Tx at all), so this instead asserts the positive contract a
// mistaken reintroduction of transaction-opening would violate: fn runs
// exactly once, against exactly the bound queries, with nothing else
// happening around it.
func TestPassthroughTransactor_RunsFnExactlyOnceAgainstBoundQueries(t *testing.T) {
	t.Parallel()
	q := &queriesMock{}
	pt := &passthroughTransactor{q: q}
	calls := 0
	var got queries
	err := pt.withinTx(t.Context(), func(q queries) error {
		calls++
		got = q
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "withinTx must invoke fn exactly once -- no retry, no nested call")
	assert.Same(t, queries(q), got, "withinTx must hand fn the exact queries it was constructed with, not a fresh one")
}

// TestPassthroughTransactor_PropagatesFnError proves withinTx does not
// swallow or wrap fn's error -- there is no commit/rollback step here that
// could otherwise mask it.
func TestPassthroughTransactor_PropagatesFnError(t *testing.T) {
	t.Parallel()
	fnErr := errors.New("boom")
	pt := &passthroughTransactor{q: &queriesMock{}}
	err := pt.withinTx(t.Context(), func(_ queries) error { return fnErr })
	assert.ErrorIs(t, err, fnErr)
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
