package codegraph

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestReplaceFileSymbols_DeletesBeforeInserting(t *testing.T) {
	t.Parallel()
	var order []string
	repoID := uuid.Must(uuid.NewV7())
	mock := &querierMock{
		DeleteSymbolsForFileFunc: func(ctx context.Context, arg gen.DeleteSymbolsForFileParams) error {
			order = append(order, "delete")
			assert.Equal(t, "main", arg.TargetBranch)
			assert.Equal(t, "a.go", arg.File)
			return nil
		},
		InsertSymbolsFunc: func(ctx context.Context, arg []gen.InsertSymbolsParams) (int64, error) {
			order = append(order, "insert")
			require.Len(t, arg, 1)
			assert.Equal(t, "Foo", arg[0].Name)
			assert.True(t, arg[0].ID.Valid, "inserted row must carry a generated id")
			return int64(len(arg)), nil
		},
	}
	store := New(mock, testLogger())
	line := int32(3)
	inserted, err := store.ReplaceFileSymbols(t.Context(), repoID, "main", "a.go", []SymbolInput{{Line: &line, Name: "Foo", Kind: "function"}})
	require.NoError(t, err)
	require.Len(t, inserted, 1)
	assert.NotEqual(t, uuid.UUID{}, inserted[0].ID, "returned symbol must carry the generated id")
	assert.Equal(t, []string{"delete", "insert"}, order, "delete-and-replace must delete before inserting")
}

func TestReplaceFileSymbols_EmptyInputSkipsInsert(t *testing.T) {
	t.Parallel()
	insertCalled := false
	mock := &querierMock{
		DeleteSymbolsForFileFunc: func(ctx context.Context, arg gen.DeleteSymbolsForFileParams) error { return nil },
		InsertSymbolsFunc: func(ctx context.Context, arg []gen.InsertSymbolsParams) (int64, error) {
			insertCalled = true
			return 0, nil
		},
	}
	store := New(mock, testLogger())
	inserted, err := store.ReplaceFileSymbols(t.Context(), uuid.Must(uuid.NewV7()), "main", "gone.go", nil)
	require.NoError(t, err)
	assert.Empty(t, inserted)
	assert.False(t, insertCalled, "an empty symbol set must delete-and-stop, never call InsertSymbols")
}

var errBoom = errors.New("boom")

func TestReplaceFileSymbols_DeleteErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		DeleteSymbolsForFileFunc: func(ctx context.Context, arg gen.DeleteSymbolsForFileParams) error { return errBoom },
	}
	store := New(mock, testLogger())
	_, err := store.ReplaceFileSymbols(t.Context(), uuid.Must(uuid.NewV7()), "main", "a.go", []SymbolInput{{Name: "Foo", Kind: "function"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom, "the underlying error must be matchable by identity through the wrap")
}

func TestRecomputeGraphEdges_DeletesResolvesThenInserts(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV7())
	fromID, toID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var order []string
	var insertedParams []gen.InsertGraphEdgesParams
	mock := &querierMock{
		DeleteGraphEdgesForRepoBranchFunc: func(ctx context.Context, arg gen.DeleteGraphEdgesForRepoBranchParams) error {
			order = append(order, "delete")
			return nil
		},
		ResolveGraphEdgeCandidatesFunc: func(ctx context.Context, arg gen.ResolveGraphEdgeCandidatesParams) ([]gen.ResolveGraphEdgeCandidatesRow, error) {
			order = append(order, "resolve")
			return []gen.ResolveGraphEdgeCandidatesRow{{
				FromSymbolID: pgUUID(fromID),
				ToSymbolID:   pgUUID(toID),
			}}, nil
		},
		InsertGraphEdgesFunc: func(ctx context.Context, arg []gen.InsertGraphEdgesParams) (int64, error) {
			order = append(order, "insert")
			insertedParams = arg
			return int64(len(arg)), nil
		},
	}
	store := New(mock, testLogger())
	count, err := store.RecomputeGraphEdges(t.Context(), repoID, "main")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
	assert.Equal(t, []string{"delete", "resolve", "insert"}, order, "recompute must delete stale edges, then resolve, then insert")
	require.Len(t, insertedParams, 1)
	assert.Equal(t, "dependency", insertedParams[0].Kind)
	assert.Equal(t, pgUUID(fromID), insertedParams[0].FromSymbolID)
	assert.Equal(t, pgUUID(toID), insertedParams[0].ToSymbolID)
	assert.True(t, insertedParams[0].ID.Valid, "each inserted edge must carry a generated id")
}

func TestRecomputeGraphEdges_NoCandidatesSkipsInsert(t *testing.T) {
	t.Parallel()
	insertCalled := false
	mock := &querierMock{
		DeleteGraphEdgesForRepoBranchFunc: func(ctx context.Context, arg gen.DeleteGraphEdgesForRepoBranchParams) error { return nil },
		ResolveGraphEdgeCandidatesFunc: func(ctx context.Context, arg gen.ResolveGraphEdgeCandidatesParams) ([]gen.ResolveGraphEdgeCandidatesRow, error) {
			return nil, nil
		},
		InsertGraphEdgesFunc: func(ctx context.Context, arg []gen.InsertGraphEdgesParams) (int64, error) {
			insertCalled = true
			return 0, nil
		},
	}
	store := New(mock, testLogger())
	count, err := store.RecomputeGraphEdges(t.Context(), uuid.Must(uuid.NewV7()), "main")
	require.NoError(t, err)
	assert.Zero(t, count)
	assert.False(t, insertCalled)
}

func TestDependents_ClampsNonPositiveLimitToDefault(t *testing.T) {
	t.Parallel()
	var gotLimit int32
	mock := &querierMock{
		DependentsFunc: func(ctx context.Context, arg gen.DependentsParams) ([]gen.DependentsRow, error) {
			gotLimit = arg.Limit
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	_, err := store.Dependents(t.Context(), uuid.Must(uuid.NewV7()), "main", uuid.Must(uuid.NewV7()), 0)
	require.NoError(t, err)
	assert.Equal(t, int32(defaultLimit), gotLimit, "a non-positive caller limit must be clamped to defaultLimit")
}

func TestDependents_PassesThroughPositiveLimit(t *testing.T) {
	t.Parallel()
	var gotLimit int32
	mock := &querierMock{
		DependentsFunc: func(ctx context.Context, arg gen.DependentsParams) ([]gen.DependentsRow, error) {
			gotLimit = arg.Limit
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	_, err := store.Dependents(t.Context(), uuid.Must(uuid.NewV7()), "main", uuid.Must(uuid.NewV7()), 7)
	require.NoError(t, err)
	assert.Equal(t, int32(7), gotLimit)
}

func TestDependents_MapsRowsToDependencies(t *testing.T) {
	t.Parallel()
	symID := uuid.Must(uuid.NewV7())
	repoID := uuid.Must(uuid.NewV7())
	mock := &querierMock{
		DependentsFunc: func(ctx context.Context, arg gen.DependentsParams) ([]gen.DependentsRow, error) {
			return []gen.DependentsRow{{
				ID:           pgUUID(symID),
				RepoID:       pgUUID(repoID),
				TargetBranch: "main",
				File:         "a.go",
				Line:         pgtype.Int4{Int32: 5, Valid: true},
				Name:         "Foo",
				Kind:         "function",
				Depth:        2,
			}}, nil
		},
	}
	store := New(mock, testLogger())
	deps, err := store.Dependents(t.Context(), repoID, "main", uuid.Must(uuid.NewV7()), 10)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, symID, deps[0].Symbol.ID)
	assert.Equal(t, "Foo", deps[0].Symbol.Name)
	require.NotNil(t, deps[0].Symbol.Line)
	assert.Equal(t, int32(5), *deps[0].Symbol.Line)
	assert.Equal(t, int32(2), deps[0].Depth)
}

func TestHistory_ClampsNonPositiveLimitToDefault(t *testing.T) {
	t.Parallel()
	var gotLimit int32
	mock := &querierMock{
		SymbolHistoryFunc: func(ctx context.Context, arg gen.SymbolHistoryParams) ([]gen.SymbolHistory, error) {
			gotLimit = arg.Limit
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	_, err := store.History(t.Context(), uuid.Must(uuid.NewV7()), -1)
	require.NoError(t, err)
	assert.Equal(t, int32(defaultLimit), gotLimit)
}

func TestAppendSymbolHistory_EmptyInputSkipsInsert(t *testing.T) {
	t.Parallel()
	insertCalled := false
	mock := &querierMock{
		InsertSymbolHistoryFunc: func(ctx context.Context, arg []gen.InsertSymbolHistoryParams) (int64, error) {
			insertCalled = true
			return 0, nil
		},
	}
	store := New(mock, testLogger())
	count, err := store.AppendSymbolHistory(t.Context(), nil)
	require.NoError(t, err)
	assert.Zero(t, count)
	assert.False(t, insertCalled)
}

func TestAppendSymbolHistory_GeneratesIDPerEntry(t *testing.T) {
	t.Parallel()
	symID := uuid.Must(uuid.NewV7())
	var gotParams []gen.InsertSymbolHistoryParams
	mock := &querierMock{
		InsertSymbolHistoryFunc: func(ctx context.Context, arg []gen.InsertSymbolHistoryParams) (int64, error) {
			gotParams = arg
			return int64(len(arg)), nil
		},
	}
	store := New(mock, testLogger())
	count, err := store.AppendSymbolHistory(t.Context(), []HistoryEntryInput{
		{SymbolID: symID, Commit: "abc123", Ref: "main", Message: "init"},
		{SymbolID: symID, Commit: "def456", Ref: "main", Message: "fix"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)
	require.Len(t, gotParams, 2)
	assert.True(t, gotParams[0].ID.Valid)
	assert.True(t, gotParams[1].ID.Valid)
	assert.NotEqual(t, gotParams[0].ID, gotParams[1].ID, "each history entry must get its own generated id")
}

func TestUUIDFromPg_InvalidReturnsZeroValue(t *testing.T) {
	t.Parallel()
	assert.Equal(t, uuid.UUID{}, uuidFromPg(pgtype.UUID{Valid: false}))
}

func TestPgInt4RoundTrip(t *testing.T) {
	t.Parallel()
	assert.Nil(t, fromPgInt4(pgInt4(nil)))
	line := int32(42)
	got := fromPgInt4(pgInt4(&line))
	require.NotNil(t, got)
	assert.Equal(t, line, *got)
}
