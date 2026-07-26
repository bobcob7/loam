package codegraph

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
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

var errBoom = errors.New("boom")

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

// TestReplaceFileSymbols_InsertErrorWraps covers the InsertSymbols error
// path: DeleteSymbolsForFile succeeds so InsertSymbols actually runs and
// fails, proving its error is wrapped with identity intact too, not just
// the delete's.
func TestReplaceFileSymbols_InsertErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		DeleteSymbolsForFileFunc: func(ctx context.Context, arg gen.DeleteSymbolsForFileParams) error { return nil },
		InsertSymbolsFunc: func(ctx context.Context, arg []gen.InsertSymbolsParams) (int64, error) {
			return 0, errBoom
		},
	}
	store := New(mock, testLogger())
	_, err := store.ReplaceFileSymbols(t.Context(), uuid.Must(uuid.NewV7()), "main", "a.go", []SymbolInput{{Name: "Foo", Kind: "function"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
}

func TestReplaceFileReferences_DeleteErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		DeleteSymbolReferencesForFileFunc: func(ctx context.Context, arg gen.DeleteSymbolReferencesForFileParams) error { return errBoom },
	}
	store := New(mock, testLogger())
	_, err := store.ReplaceFileReferences(t.Context(), uuid.Must(uuid.NewV7()), "main", "a.go", []ReferenceInput{{Name: "Foo", Kind: "function", Line: 1}})
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
}

func TestReplaceFileReferences_InsertErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		DeleteSymbolReferencesForFileFunc: func(ctx context.Context, arg gen.DeleteSymbolReferencesForFileParams) error { return nil },
		InsertSymbolReferencesFunc: func(ctx context.Context, arg []gen.InsertSymbolReferencesParams) (int64, error) {
			return 0, errBoom
		},
	}
	store := New(mock, testLogger())
	_, err := store.ReplaceFileReferences(t.Context(), uuid.Must(uuid.NewV7()), "main", "a.go", []ReferenceInput{{Name: "Foo", Kind: "function", Line: 1}})
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
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

func TestRecomputeGraphEdges_DeleteErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		DeleteGraphEdgesForRepoBranchFunc: func(ctx context.Context, arg gen.DeleteGraphEdgesForRepoBranchParams) error { return errBoom },
	}
	store := New(mock, testLogger())
	_, err := store.RecomputeGraphEdges(t.Context(), uuid.Must(uuid.NewV7()), "main")
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
}

func TestRecomputeGraphEdges_ResolveErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		DeleteGraphEdgesForRepoBranchFunc: func(ctx context.Context, arg gen.DeleteGraphEdgesForRepoBranchParams) error { return nil },
		ResolveGraphEdgeCandidatesFunc: func(ctx context.Context, arg gen.ResolveGraphEdgeCandidatesParams) ([]gen.ResolveGraphEdgeCandidatesRow, error) {
			return nil, errBoom
		},
	}
	store := New(mock, testLogger())
	_, err := store.RecomputeGraphEdges(t.Context(), uuid.Must(uuid.NewV7()), "main")
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
}

func TestRecomputeGraphEdges_InsertErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		DeleteGraphEdgesForRepoBranchFunc: func(ctx context.Context, arg gen.DeleteGraphEdgesForRepoBranchParams) error { return nil },
		ResolveGraphEdgeCandidatesFunc: func(ctx context.Context, arg gen.ResolveGraphEdgeCandidatesParams) ([]gen.ResolveGraphEdgeCandidatesRow, error) {
			return []gen.ResolveGraphEdgeCandidatesRow{{FromSymbolID: pgUUID(uuid.Must(uuid.NewV7())), ToSymbolID: pgUUID(uuid.Must(uuid.NewV7()))}}, nil
		},
		InsertGraphEdgesFunc: func(ctx context.Context, arg []gen.InsertGraphEdgesParams) (int64, error) {
			return 0, errBoom
		},
	}
	store := New(mock, testLogger())
	_, err := store.RecomputeGraphEdges(t.Context(), uuid.Must(uuid.NewV7()), "main")
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
}

// lineNumber returns a pointer to v, for building gen.Symbol test rows.
// integration_test.go's int32Ptr is unavailable here: it lives behind the
// integration build tag and this file has none.
func lineNumber(v int32) *int32 { return &v }

// genSymbol builds a gen.Symbol row for LookupSymbolsByName mock returns.
func genSymbol(id, repoID uuid.UUID, file, name string, line *int32) gen.Symbol {
	return gen.Symbol{
		ID:           pgUUID(id),
		RepoID:       pgUUID(repoID),
		TargetBranch: "main",
		File:         file,
		Line:         pgInt4(line),
		Name:         name,
		Kind:         "function",
	}
}

// TestLookupSymbolsByName_EmptyRepoIDsSkipsQuery mirrors
// internal/chunkstore.Search's "empty scope matches nothing" rule: an
// empty repoIDs must return immediately without ever reaching the query
// layer, not be treated as "no filter".
func TestLookupSymbolsByName_EmptyRepoIDsSkipsQuery(t *testing.T) {
	t.Parallel()
	queryCalled := false
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			queryCalled = true
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	symbols, truncated, err := store.LookupSymbolsByName(t.Context(), nil, "main", "Login", "", 10)
	require.NoError(t, err)
	assert.Empty(t, symbols)
	assert.False(t, truncated)
	assert.False(t, queryCalled, "an empty repoIDs scope must never reach the query layer")
}

// TestLookupSymbolsByName_ScopesByRepoIDsAndFile proves the Go-level
// parameters land in the exact gen params LookupSymbolsByName's SQL
// expects: repoIDs converted to pgtype.UUID under Column1 (the ANY($1::
// uuid[]) scope), targetBranch and name passed through, and file passed as
// Column4 (the "empty string means no narrowing" sentinel the SQL comment
// documents).
func TestLookupSymbolsByName_ScopesByRepoIDsAndFile(t *testing.T) {
	t.Parallel()
	repoA, repoB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var got gen.LookupSymbolsByNameParams
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			got = arg
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.LookupSymbolsByName(t.Context(), []uuid.UUID{repoA, repoB}, "main", "Login", "auth.go", 10)
	require.NoError(t, err)
	assert.Equal(t, []pgtype.UUID{pgUUID(repoA), pgUUID(repoB)}, got.Column1)
	assert.Equal(t, "main", got.TargetBranch)
	assert.Equal(t, "Login", got.Name)
	assert.Equal(t, "auth.go", got.Column4, "a non-empty --file must be passed through as the narrowing param")
}

// TestLookupSymbolsByName_NoFileFilterPassesEmptyString proves omitting
// --file (the Go-level empty string) reaches the query as an empty
// string too, not some other sentinel -- the SQL's ($4::text = ” OR ...)
// clause depends on this exact value to mean "no narrowing".
func TestLookupSymbolsByName_NoFileFilterPassesEmptyString(t *testing.T) {
	t.Parallel()
	var got gen.LookupSymbolsByNameParams
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			got = arg
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.LookupSymbolsByName(t.Context(), []uuid.UUID{uuid.Must(uuid.NewV7())}, "main", "Login", "", 10)
	require.NoError(t, err)
	assert.Equal(t, "", got.Column4)
}

// TestLookupSymbolsByName_FetchesLimitPlusOne mirrors
// TestDependents_FetchesLimitPlusOne: the Store must ask for one more row
// than the caller's limit so it can detect truncation without a second
// round trip.
func TestLookupSymbolsByName_FetchesLimitPlusOne(t *testing.T) {
	t.Parallel()
	var gotLimit int32
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			gotLimit = arg.Limit
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.LookupSymbolsByName(t.Context(), []uuid.UUID{uuid.Must(uuid.NewV7())}, "main", "Login", "", 7)
	require.NoError(t, err)
	assert.Equal(t, int32(8), gotLimit)
}

// TestLookupSymbolsByName_ClampsNonPositiveLimitToDefaultThenFetchesOneMore
// mirrors Dependents' identical clampLimit/fetchLimit composition test.
func TestLookupSymbolsByName_ClampsNonPositiveLimitToDefaultThenFetchesOneMore(t *testing.T) {
	t.Parallel()
	var gotLimit int32
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			gotLimit = arg.Limit
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.LookupSymbolsByName(t.Context(), []uuid.UUID{uuid.Must(uuid.NewV7())}, "main", "Login", "", 0)
	require.NoError(t, err)
	assert.Equal(t, int32(defaultLimit+1), gotLimit)
}

// TestLookupSymbolsByName_TrimsToLimitAndReportsTruncated proves the
// truncation contract this bead adds: given more rows than the caller's
// limit, LookupSymbolsByName must trim to exactly limit and report
// truncated=true -- the same signal docs/cli-spec.md:537-539 requires for
// every graph subquery's capped response, not only Dependents/Deps/History.
func TestLookupSymbolsByName_TrimsToLimitAndReportsTruncated(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV7())
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			assert.Equal(t, int32(3), arg.Limit, "must request limit+1 = 3 for a caller limit of 2")
			return []gen.Symbol{
				genSymbol(uuid.Must(uuid.NewV7()), repoID, "a.go", "Login", lineNumber(1)),
				genSymbol(uuid.Must(uuid.NewV7()), repoID, "b.go", "Login", lineNumber(2)),
				genSymbol(uuid.Must(uuid.NewV7()), repoID, "c.go", "Login", lineNumber(3)),
			}, nil
		},
	}
	store := New(mock, testLogger())
	symbols, truncated, err := store.LookupSymbolsByName(t.Context(), []uuid.UUID{repoID}, "main", "Login", "", 2)
	require.NoError(t, err)
	assert.True(t, truncated, "3 rows fetched for a limit of 2 must report truncated=true")
	require.Len(t, symbols, 2, "the result must be trimmed back down to the caller's limit")
}

// TestLookupSymbolsByName_ExactlyLimitRows_NotTruncated is the negative
// case: exactly limit rows must not be reported as truncated.
func TestLookupSymbolsByName_ExactlyLimitRows_NotTruncated(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV7())
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			return []gen.Symbol{
				genSymbol(uuid.Must(uuid.NewV7()), repoID, "a.go", "Login", lineNumber(1)),
				genSymbol(uuid.Must(uuid.NewV7()), repoID, "b.go", "Login", lineNumber(2)),
			}, nil
		},
	}
	store := New(mock, testLogger())
	symbols, truncated, err := store.LookupSymbolsByName(t.Context(), []uuid.UUID{repoID}, "main", "Login", "", 2)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Len(t, symbols, 2)
}

// TestLookupSymbolsByName_ZeroRowsIsNotAnError is this bead's central
// contract: a genuine "no such symbol" must come back as an empty,
// non-error result -- the authoritative not-found signal a handler maps to
// exit 3 (docs/cli-spec.md), never a sentinel error.
func TestLookupSymbolsByName_ZeroRowsIsNotAnError(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	symbols, truncated, err := store.LookupSymbolsByName(t.Context(), []uuid.UUID{uuid.Must(uuid.NewV7())}, "main", "NoSuchSymbol", "", 10)
	require.NoError(t, err, "zero matches must not be an error")
	assert.Empty(t, symbols)
	assert.False(t, truncated)
}

// TestLookupSymbolsByName_MultipleMatchesReturnedAsData proves ambiguity
// (several distinct symbols sharing a name) comes back as multiple rows,
// not an error -- docs/cli-spec.md:531-535's "ambiguous target is data,
// not an error".
func TestLookupSymbolsByName_MultipleMatchesReturnedAsData(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV7())
	idA, idB, idC := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			return []gen.Symbol{
				genSymbol(idA, repoID, "a.go", "Login", lineNumber(1)),
				genSymbol(idB, repoID, "b.go", "Login", lineNumber(2)),
				genSymbol(idC, repoID, "c.go", "Login", lineNumber(3)),
			}, nil
		},
	}
	store := New(mock, testLogger())
	symbols, truncated, err := store.LookupSymbolsByName(t.Context(), []uuid.UUID{repoID}, "main", "Login", "", 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, symbols, 3, "an ambiguous name must return every match, not fail or pick one")
}

// TestLookupSymbolsByName_MapsRowsToSymbols proves field-by-field
// conversion from the sqlc gen.Symbol row (including a nil Line for a
// file-level symbol) into this package's exported Symbol type.
func TestLookupSymbolsByName_MapsRowsToSymbols(t *testing.T) {
	t.Parallel()
	symID := uuid.Must(uuid.NewV7())
	repoID := uuid.Must(uuid.NewV7())
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			return []gen.Symbol{
				genSymbol(symID, repoID, "a.go", "Login", nil),
			}, nil
		},
	}
	store := New(mock, testLogger())
	symbols, truncated, err := store.LookupSymbolsByName(t.Context(), []uuid.UUID{repoID}, "main", "Login", "", 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, symbols, 1)
	assert.Equal(t, symID, symbols[0].ID)
	assert.Equal(t, repoID, symbols[0].RepoID)
	assert.Equal(t, "main", symbols[0].TargetBranch)
	assert.Equal(t, "a.go", symbols[0].File)
	assert.Nil(t, symbols[0].Line, "a file-level symbol's Line must decode to nil, not a zero value")
	assert.Equal(t, "Login", symbols[0].Name)
	assert.Equal(t, "function", symbols[0].Kind)
}

// TestLookupSymbolsByName_QueryErrorWraps mirrors the identity-preserving
// error-wrap tests for Dependents/Deps/History.
func TestLookupSymbolsByName_QueryErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		LookupSymbolsByNameFunc: func(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error) {
			return nil, errBoom
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.LookupSymbolsByName(t.Context(), []uuid.UUID{uuid.Must(uuid.NewV7())}, "main", "Login", "", 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
}

func TestDependents_QueryErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		DependentsFunc: func(ctx context.Context, arg gen.DependentsParams) ([]gen.DependentsRow, error) {
			return nil, errBoom
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.Dependents(t.Context(), uuid.Must(uuid.NewV7()), "main", uuid.Must(uuid.NewV7()), 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
}

func TestDeps_QueryErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		DepsFunc: func(ctx context.Context, arg gen.DepsParams) ([]gen.DepsRow, error) {
			return nil, errBoom
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.Deps(t.Context(), uuid.Must(uuid.NewV7()), "main", uuid.Must(uuid.NewV7()), 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
}

// TestDependents_FetchesLimitPlusOne is FIX 1's core Go-side contract: the
// Store must ask the query layer for one MORE row than the caller wants to
// keep, since that is the only way to detect "there were more" without a
// second round-trip. Before FIX 1 this test fails outright, because
// Dependents passed the caller's limit straight through unmodified.
func TestDependents_FetchesLimitPlusOne(t *testing.T) {
	t.Parallel()
	var gotLimit int32
	mock := &querierMock{
		DependentsFunc: func(ctx context.Context, arg gen.DependentsParams) ([]gen.DependentsRow, error) {
			gotLimit = arg.Limit
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.Dependents(t.Context(), uuid.Must(uuid.NewV7()), "main", uuid.Must(uuid.NewV7()), 7)
	require.NoError(t, err)
	assert.Equal(t, int32(8), gotLimit, "Dependents must fetch limit+1 rows internally so it can detect truncation")
}

// TestDependents_ClampsNonPositiveLimitToDefaultThenFetchesOneMore proves
// clampLimit's defaultLimit substitution and fetchLimit's +1 compose
// correctly: a non-positive caller limit must still result in
// defaultLimit+1 rows requested, not defaultLimit (which would silently
// lose the truncation signal exactly in the "caller passed no limit"
// case FIX 1 calls out by name).
func TestDependents_ClampsNonPositiveLimitToDefaultThenFetchesOneMore(t *testing.T) {
	t.Parallel()
	var gotLimit int32
	mock := &querierMock{
		DependentsFunc: func(ctx context.Context, arg gen.DependentsParams) ([]gen.DependentsRow, error) {
			gotLimit = arg.Limit
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.Dependents(t.Context(), uuid.Must(uuid.NewV7()), "main", uuid.Must(uuid.NewV7()), 0)
	require.NoError(t, err)
	assert.Equal(t, int32(defaultLimit+1), gotLimit)
}

func dependentsRow(id uuid.UUID, depth int32) gen.DependentsRow {
	return gen.DependentsRow{
		ID:           pgUUID(id),
		RepoID:       pgUUID(uuid.Must(uuid.NewV7())),
		TargetBranch: "main",
		File:         "a.go",
		Line:         pgtype.Int4{Int32: 1, Valid: true},
		Name:         "Foo",
		Kind:         "function",
		Depth:        depth,
	}
}

// TestDependents_TrimsToLimitAndReportsTruncated is FIX 1's end-to-end
// proof at the Store layer: given more rows than the caller's limit (the
// query layer having honored the limit+1 fetch), Dependents must trim
// back down to exactly limit and report truncated=true. Before FIX 1,
// Dependents had no truncated return value at all, so any caller receiving
// exactly `limit` rows back could never tell that from "this really is
// the whole set" -- this test fails immediately (compile-level) against
// the pre-fix two-return-value signature, and would fail by assertion
// against a fixed signature that forgot to actually trim or flip the flag.
func TestDependents_TrimsToLimitAndReportsTruncated(t *testing.T) {
	t.Parallel()
	ids := []uuid.UUID{uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())}
	mock := &querierMock{
		DependentsFunc: func(ctx context.Context, arg gen.DependentsParams) ([]gen.DependentsRow, error) {
			assert.Equal(t, int32(3), arg.Limit, "must request limit+1 = 3 for a caller limit of 2")
			return []gen.DependentsRow{
				dependentsRow(ids[0], 1),
				dependentsRow(ids[1], 1),
				dependentsRow(ids[2], 2),
			}, nil
		},
	}
	store := New(mock, testLogger())
	deps, truncated, err := store.Dependents(t.Context(), uuid.Must(uuid.NewV7()), "main", uuid.Must(uuid.NewV7()), 2)
	require.NoError(t, err)
	assert.True(t, truncated, "3 rows fetched for a limit of 2 must report truncated=true")
	require.Len(t, deps, 2, "the result must be trimmed back down to the caller's limit, not the raw limit+1 fetch")
	assert.Equal(t, ids[0], deps[0].Symbol.ID)
	assert.Equal(t, ids[1], deps[1].Symbol.ID)
}

// TestDependents_ExactlyLimitRows_NotTruncated is the negative case: when
// the query layer returns exactly `limit` rows (not limit+1), Dependents
// must report truncated=false -- proving truncated isn't hardcoded true,
// and that the "did we get more than we asked to keep" check is a strict
// greater-than, not off-by-one in the other direction.
func TestDependents_ExactlyLimitRows_NotTruncated(t *testing.T) {
	t.Parallel()
	ids := []uuid.UUID{uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())}
	mock := &querierMock{
		DependentsFunc: func(ctx context.Context, arg gen.DependentsParams) ([]gen.DependentsRow, error) {
			return []gen.DependentsRow{
				dependentsRow(ids[0], 1),
				dependentsRow(ids[1], 1),
			}, nil
		},
	}
	store := New(mock, testLogger())
	deps, truncated, err := store.Dependents(t.Context(), uuid.Must(uuid.NewV7()), "main", uuid.Must(uuid.NewV7()), 2)
	require.NoError(t, err)
	assert.False(t, truncated, "exactly limit rows must not be reported as truncated")
	assert.Len(t, deps, 2)
}

func depsRow(id uuid.UUID, depth int32) gen.DepsRow {
	return gen.DepsRow{
		ID:           pgUUID(id),
		RepoID:       pgUUID(uuid.Must(uuid.NewV7())),
		TargetBranch: "main",
		File:         "a.go",
		Line:         pgtype.Int4{Int32: 1, Valid: true},
		Name:         "Foo",
		Kind:         "function",
		Depth:        depth,
	}
}

// TestDeps_TrimsToLimitAndReportsTruncated mirrors
// TestDependents_TrimsToLimitAndReportsTruncated for Deps, which converts
// its rows inline rather than sharing Dependents' conversion helper.
func TestDeps_TrimsToLimitAndReportsTruncated(t *testing.T) {
	t.Parallel()
	ids := []uuid.UUID{uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())}
	mock := &querierMock{
		DepsFunc: func(ctx context.Context, arg gen.DepsParams) ([]gen.DepsRow, error) {
			assert.Equal(t, int32(2), arg.Limit, "must request limit+1 = 2 for a caller limit of 1")
			return []gen.DepsRow{
				depsRow(ids[0], 1),
				depsRow(ids[1], 2),
			}, nil
		},
	}
	store := New(mock, testLogger())
	deps, truncated, err := store.Deps(t.Context(), uuid.Must(uuid.NewV7()), "main", uuid.Must(uuid.NewV7()), 1)
	require.NoError(t, err)
	assert.True(t, truncated)
	require.Len(t, deps, 1)
	assert.Equal(t, ids[0], deps[0].Symbol.ID)
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
	deps, truncated, err := store.Dependents(t.Context(), repoID, "main", uuid.Must(uuid.NewV7()), 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, deps, 1)
	assert.Equal(t, symID, deps[0].Symbol.ID)
	assert.Equal(t, "Foo", deps[0].Symbol.Name)
	require.NotNil(t, deps[0].Symbol.Line)
	assert.Equal(t, int32(5), *deps[0].Symbol.Line)
	assert.Equal(t, int32(2), deps[0].Depth)
}

func TestHistory_ClampsNonPositiveLimitToDefaultThenFetchesOneMore(t *testing.T) {
	t.Parallel()
	var gotLimit int32
	mock := &querierMock{
		SymbolHistoryFunc: func(ctx context.Context, arg gen.SymbolHistoryParams) ([]gen.SymbolHistory, error) {
			gotLimit = arg.Limit
			return nil, nil
		},
	}
	store := New(mock, testLogger())
	_, truncated, err := store.History(t.Context(), uuid.Must(uuid.NewV7()), -1)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Equal(t, int32(defaultLimit+1), gotLimit)
}

// TestHistory_TrimsToLimitAndReportsTruncated is FIX 1's proof for
// History: the third table this bead's truncation contract covers.
func TestHistory_TrimsToLimitAndReportsTruncated(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		SymbolHistoryFunc: func(ctx context.Context, arg gen.SymbolHistoryParams) ([]gen.SymbolHistory, error) {
			assert.Equal(t, int32(2), arg.Limit)
			return []gen.SymbolHistory{
				{ID: pgUUID(uuid.Must(uuid.NewV7())), SymbolID: pgUUID(uuid.Must(uuid.NewV7())), Commit: "c2", Ref: "main", Message: "second"},
				{ID: pgUUID(uuid.Must(uuid.NewV7())), SymbolID: pgUUID(uuid.Must(uuid.NewV7())), Commit: "c1", Ref: "main", Message: "first"},
			}, nil
		},
	}
	store := New(mock, testLogger())
	entries, truncated, err := store.History(t.Context(), uuid.Must(uuid.NewV7()), 1)
	require.NoError(t, err)
	assert.True(t, truncated)
	require.Len(t, entries, 1)
	assert.Equal(t, "c2", entries[0].Commit)
}

func TestHistory_QueryErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		SymbolHistoryFunc: func(ctx context.Context, arg gen.SymbolHistoryParams) ([]gen.SymbolHistory, error) {
			return nil, errBoom
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.History(t.Context(), uuid.Must(uuid.NewV7()), 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
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

func TestAppendSymbolHistory_InsertErrorWraps(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		InsertSymbolHistoryFunc: func(ctx context.Context, arg []gen.InsertSymbolHistoryParams) (int64, error) {
			return 0, errBoom
		},
	}
	store := New(mock, testLogger())
	_, err := store.AppendSymbolHistory(t.Context(), []HistoryEntryInput{{SymbolID: uuid.Must(uuid.NewV7()), Commit: "c1", Ref: "main", Message: "m"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, errBoom)
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

func TestFetchLimit_SaturatesAtInt32Max(t *testing.T) {
	t.Parallel()
	assert.Equal(t, int32(math.MaxInt32), fetchLimit(math.MaxInt32), "fetchLimit must saturate instead of overflowing negative at the int32 ceiling")
	assert.Equal(t, int32(11), fetchLimit(10))
}
