package handler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/reposstore"
)

// TestScopeResolver_Resolve_EmptyNames_ExpandsToAllEnrolledRepos proves an
// empty QueryScope.repos ("--all" or the CLI's own no-flag default,
// docs/cli-spec.md) expands to every enrolled repo via ListAllRepoNames,
// not to an empty slice -- the exact mistake this bead's NOTES call out as
// "a bug on your side" for whichever store consumes the result.
func TestScopeResolver_Resolve_EmptyNames_ExpandsToAllEnrolledRepos(t *testing.T) {
	t.Parallel()
	repoA := uuid.New()
	repoB := uuid.New()
	store := &handler.ScopeStoreMock{
		ListAllRepoNamesFunc: func(context.Context) ([]string, error) {
			return []string{"bobcob7/alpha", "bobcob7/beta"}, nil
		},
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			switch name {
			case "bobcob7/alpha":
				return reposstore.Repo{ID: repoA, Name: name, IndexedBranch: "main"}, nil
			case "bobcob7/beta":
				return reposstore.Repo{ID: repoB, Name: name, IndexedBranch: "trunk"}, nil
			default:
				t.Fatalf("unexpected repo name %q", name)
				return reposstore.Repo{}, nil
			}
		},
	}
	resolver := handler.NewScopeResolver(store)
	scoped, err := resolver.Resolve(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, scoped, 2)
	assert.Equal(t, handler.ScopedRepo{ID: repoA, Name: "bobcob7/alpha", IndexedBranch: "main"}, scoped[0])
	assert.Equal(t, handler.ScopedRepo{ID: repoB, Name: "bobcob7/beta", IndexedBranch: "trunk"}, scoped[1])
}

// TestScopeResolver_Resolve_NonEmptyNames_DoesNotListAllRepos proves an
// explicit, non-empty scope resolves each named repo directly and never
// consults ListAllRepoNames -- a caller naming one repo must not silently
// fan out to every enrolled repo.
func TestScopeResolver_Resolve_NonEmptyNames_DoesNotListAllRepos(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	listCalled := false
	store := &handler.ScopeStoreMock{
		ListAllRepoNamesFunc: func(context.Context) ([]string, error) {
			listCalled = true
			return nil, nil
		},
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: repoID, Name: name, IndexedBranch: "main"}, nil
		},
	}
	resolver := handler.NewScopeResolver(store)
	scoped, err := resolver.Resolve(t.Context(), []string{"bobcob7/doc-server"})
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	assert.Equal(t, repoID, scoped[0].ID)
	assert.False(t, listCalled, "an explicit scope must not consult ListAllRepoNames")
}

// TestScopeResolver_Resolve_UnknownRepo_ReturnsInvalidArgumentNotNotFound
// proves an unresolvable scope maps to ErrInvalidArgument (docs/cli-spec.md's
// exit-2 "unresolvable scope" case), distinct from ErrNotFound, which this
// package's callers reserve for a target/symbol absent INSIDE an
// already-resolved scope.
func TestScopeResolver_Resolve_UnknownRepo_ReturnsInvalidArgumentNotNotFound(t *testing.T) {
	t.Parallel()
	store := &handler.ScopeStoreMock{
		ListAllRepoNamesFunc: func(context.Context) ([]string, error) { return nil, nil },
		GetRepoByNameFunc: func(_ context.Context, _ string) (reposstore.Repo, error) {
			return reposstore.Repo{}, reposstore.ErrNotFound
		},
	}
	resolver := handler.NewScopeResolver(store)
	_, err := resolver.Resolve(t.Context(), []string{"bobcob7/ghost-repo"})
	require.Error(t, err)
	assert.ErrorIs(t, err, handler.ErrInvalidArgument)
	assert.NotErrorIs(t, err, handler.ErrNotFound)
}

// TestScopeResolver_Resolve_ListAllRepoNamesFailure_Wraps proves an
// infrastructure failure listing enrolled repos is surfaced, not silently
// treated as an empty scope.
func TestScopeResolver_Resolve_ListAllRepoNamesFailure_Wraps(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection reset")
	store := &handler.ScopeStoreMock{
		ListAllRepoNamesFunc: func(context.Context) ([]string, error) { return nil, dbErr },
	}
	resolver := handler.NewScopeResolver(store)
	_, err := resolver.Resolve(t.Context(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, dbErr)
}

// TestScopeResolver_Resolve_ZeroEnrolledRepos_ReturnsEmptyNotError proves an
// enrollment with zero repos resolves to an empty, non-error ScopedRepo
// slice -- not an error -- so a caller's subsequent store calls correctly
// see an empty scope and return no results.
func TestScopeResolver_Resolve_ZeroEnrolledRepos_ReturnsEmptyNotError(t *testing.T) {
	t.Parallel()
	store := &handler.ScopeStoreMock{
		ListAllRepoNamesFunc: func(context.Context) ([]string, error) { return nil, nil },
	}
	resolver := handler.NewScopeResolver(store)
	scoped, err := resolver.Resolve(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, scoped)
}

// TestScopeResolver_Ingested_MatchesIndexedBranchAndReportsProvenance proves
// Ingested finds the repo_target_branches row matching the repo's OWN
// indexed_branch (not just any enrolled target branch) and reports its ref/
// at.
func TestScopeResolver_Ingested_MatchesIndexedBranchAndReportsProvenance(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	ingestedAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := &handler.ScopeStoreMock{
		ListTargetBranchesFunc: func(_ context.Context, id uuid.UUID) ([]reposstore.TargetBranch, error) {
			require.Equal(t, repoID, id)
			return []reposstore.TargetBranch{
				{RepoID: id, Branch: "release", IngestedRef: reposstore.IngestedRef{Ref: "deadbeef", Ok: true}},
				{RepoID: id, Branch: "main", IngestedRef: reposstore.IngestedRef{Ref: "a1b2c3d", Ok: true}, IngestedAt: &ingestedAt},
			}, nil
		},
	}
	resolver := handler.NewScopeResolver(store)
	entries, err := resolver.Ingested(t.Context(), []handler.ScopedRepo{{ID: repoID, Name: "bobcob7/doc-server", IndexedBranch: "main"}})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, handler.Ingested{Repo: "bobcob7/doc-server", Target: "main", Ref: "a1b2c3d", At: "2026-07-25T12:00:00Z"}, entries[0])
}

// TestScopeResolver_Ingested_NeverIngested_LeavesRefAndAtEmpty proves a
// target branch that has never completed an ingest (ingested_ref/
// ingested_at both NULL, reposstore.IngestedRef.Ok false) reports an empty
// Ref and At rather than a zero-valued lie.
func TestScopeResolver_Ingested_NeverIngested_LeavesRefAndAtEmpty(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	store := &handler.ScopeStoreMock{
		ListTargetBranchesFunc: func(context.Context, uuid.UUID) ([]reposstore.TargetBranch, error) {
			return []reposstore.TargetBranch{{RepoID: repoID, Branch: "main"}}, nil
		},
	}
	resolver := handler.NewScopeResolver(store)
	entries, err := resolver.Ingested(t.Context(), []handler.ScopedRepo{{ID: repoID, Name: "bobcob7/doc-server", IndexedBranch: "main"}})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Empty(t, entries[0].Ref)
	assert.Empty(t, entries[0].At)
}
