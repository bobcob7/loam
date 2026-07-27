package mirrorsync

import (
	"context"
	"errors"
	"testing"

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
	branches := &workBranchNameListerMock{
		ListFunc: func(_ context.Context, filter workbranchstore.ListFilter, limit, offset int32) ([]workbranchstore.WorkBranch, int64, error) {
			require.NotNil(t, filter.RepoID)
			assert.Equal(t, repoID, *filter.RepoID)
			assert.Equal(t, int32(0), offset)
			return []workbranchstore.WorkBranch{{Name: "wb-1"}, {Name: "wb-2"}}, 2, nil
		},
	}
	resolver := NewStoreRepoResolver(repos, branches)
	host, upstreamURL, names, err := resolver.ResolveRepo(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	assert.Equal(t, "forge.example.com", host)
	assert.Equal(t, "https://forge.example.com/acme/widgets.git", upstreamURL)
	assert.Equal(t, []string{"wb-1", "wb-2"}, names)
}

func TestStoreRepoResolverResolveRepoPagesThroughAllWorkBranches(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: repoID}, nil
		},
	}
	var seenOffsets []int32
	branches := &workBranchNameListerMock{
		ListFunc: func(_ context.Context, _ workbranchstore.ListFilter, limit, offset int32) ([]workbranchstore.WorkBranch, int64, error) {
			seenOffsets = append(seenOffsets, offset)
			if offset == 0 {
				return []workbranchstore.WorkBranch{{Name: "wb-1"}, {Name: "wb-2"}}, 3, nil
			}
			return []workbranchstore.WorkBranch{{Name: "wb-3"}}, 3, nil
		},
	}
	resolver := NewStoreRepoResolver(repos, branches)
	_, _, names, err := resolver.ResolveRepo(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	assert.Equal(t, []string{"wb-1", "wb-2", "wb-3"}, names)
	assert.Equal(t, []int32{0, 2}, seenOffsets)
}

func TestStoreRepoResolverResolveRepoNoWorkBranchesReturnsEmptySlice(t *testing.T) {
	t.Parallel()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: uuid.New()}, nil
		},
	}
	branches := &workBranchNameListerMock{
		ListFunc: func(context.Context, workbranchstore.ListFilter, int32, int32) ([]workbranchstore.WorkBranch, int64, error) {
			return nil, 0, nil
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
		ListFunc: func(context.Context, workbranchstore.ListFilter, int32, int32) ([]workbranchstore.WorkBranch, int64, error) {
			t.Fatal("List must not be called when the repo lookup already failed")
			return nil, 0, nil
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
		ListFunc: func(context.Context, workbranchstore.ListFilter, int32, int32) ([]workbranchstore.WorkBranch, int64, error) {
			return nil, 0, wantErr
		},
	}
	resolver := NewStoreRepoResolver(repos, branches)
	_, _, _, err := resolver.ResolveRepo(t.Context(), RepoID("acme/widgets"))
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}
