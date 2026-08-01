package mirrorsync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

func TestStoreRepoResolverResolveRepoReturnsHostURLAndWorkBranchNames(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			assert.Equal(t, "acme/widgets", name)
			return reposstore.Repo{ID: repoID, ForgeHost: "forge.example.com", UpstreamURL: "https://forge.example.com/acme/widgets.git"}, nil
		},
	}
	calls := 0
	branches := &workBranchNameListerMock{
		ListByCursorFunc: func(_ context.Context, filter workbranchstore.ListFilter, _ int32, after *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			require.NotNil(t, filter.RepoID)
			assert.Equal(t, repoID, *filter.RepoID)
			calls++
			if calls == 1 {
				assert.Nil(t, after, "the first page must carry no cursor")
				return []workbranchstore.WorkBranch{{Name: "wb-1"}, {Name: "wb-2"}}, nil
			}
			return nil, nil
		},
	}
	resolver := NewStoreRepoResolver(repos, branches)
	host, upstreamURL, names, err := resolver.ResolveRepo(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	assert.Equal(t, "forge.example.com", host)
	assert.Equal(t, "https://forge.example.com/acme/widgets.git", upstreamURL)
	assert.Equal(t, []string{"wb-1", "wb-2"}, names)
}

// TestStoreRepoResolverResolveRepoPagesThroughAllWorkBranches proves the
// keyset paging loop resumes each call from the LAST ROW of the previous
// page (its CreatedAt and ID), not from a row count -- the property that
// makes it immune to the OFFSET shift a concurrent insert used to cause
// (loam-coj). The loop keeps calling until a page comes back empty, even
// though every real page here is far short of workBranchListPageSize.
func TestStoreRepoResolverResolveRepoPagesThroughAllWorkBranches(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: repoID}, nil
		},
	}
	wb1 := workbranchstore.WorkBranch{Name: "wb-1", ID: uuid.New(), CreatedAt: time.Unix(300, 0)}
	wb2 := workbranchstore.WorkBranch{Name: "wb-2", ID: uuid.New(), CreatedAt: time.Unix(200, 0)}
	wb3 := workbranchstore.WorkBranch{Name: "wb-3", ID: uuid.New(), CreatedAt: time.Unix(100, 0)}
	var seenCursors []*workbranchstore.Cursor
	calls := 0
	branches := &workBranchNameListerMock{
		ListByCursorFunc: func(_ context.Context, _ workbranchstore.ListFilter, _ int32, after *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			seenCursors = append(seenCursors, after)
			calls++
			switch calls {
			case 1:
				return []workbranchstore.WorkBranch{wb1, wb2}, nil
			case 2:
				return []workbranchstore.WorkBranch{wb3}, nil
			default:
				return nil, nil
			}
		},
	}
	resolver := NewStoreRepoResolver(repos, branches)
	_, _, names, err := resolver.ResolveRepo(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	assert.Equal(t, []string{"wb-1", "wb-2", "wb-3"}, names)
	require.Len(t, seenCursors, 3, "a third call, on the terminating empty page, must still happen")
	assert.Nil(t, seenCursors[0], "the first call carries no cursor")
	require.NotNil(t, seenCursors[1])
	assert.Equal(t, wb2.CreatedAt, seenCursors[1].CreatedAt, "the second call resumes from the LAST row of page one")
	assert.Equal(t, wb2.ID, seenCursors[1].ID)
	require.NotNil(t, seenCursors[2])
	assert.Equal(t, wb3.CreatedAt, seenCursors[2].CreatedAt, "the third call resumes from the last row of page two")
	assert.Equal(t, wb3.ID, seenCursors[2].ID)
}

func TestStoreRepoResolverResolveRepoNoWorkBranchesReturnsEmptySlice(t *testing.T) {
	t.Parallel()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: uuid.New()}, nil
		},
	}
	branches := &workBranchNameListerMock{
		ListByCursorFunc: func(context.Context, workbranchstore.ListFilter, int32, *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			return nil, nil
		},
	}
	resolver := NewStoreRepoResolver(repos, branches)
	_, _, names, err := resolver.ResolveRepo(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	assert.Empty(t, names)
}

func TestStoreRepoResolverResolveRepoPropagatesGetRepoByNameError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom: repo not found")
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{}, wantErr
		},
	}
	branches := &workBranchNameListerMock{
		ListByCursorFunc: func(context.Context, workbranchstore.ListFilter, int32, *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			t.Fatal("ListByCursor must not be called when the repo lookup already failed")
			return nil, nil
		},
	}
	resolver := NewStoreRepoResolver(repos, branches)
	_, _, _, err := resolver.ResolveRepo(t.Context(), RepoID("acme/widgets"))
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestStoreRepoResolverResolveRepoPropagatesListError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom: list failed")
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: uuid.New()}, nil
		},
	}
	branches := &workBranchNameListerMock{
		ListByCursorFunc: func(context.Context, workbranchstore.ListFilter, int32, *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			return nil, wantErr
		},
	}
	resolver := NewStoreRepoResolver(repos, branches)
	_, _, _, err := resolver.ResolveRepo(t.Context(), RepoID("acme/widgets"))
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}
