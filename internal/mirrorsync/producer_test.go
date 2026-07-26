package mirrorsync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreRepoListerConvertsNamesToRepoIDs(t *testing.T) {
	t.Parallel()
	store := &repoNameListerMock{
		ListAllRepoNamesFunc: func(ctx context.Context) ([]string, error) {
			return []string{"group/a", "group/b"}, nil
		},
	}
	lister := NewStoreRepoLister(store)
	repos, err := lister.ListRepos(t.Context())
	require.NoError(t, err)
	require.Len(t, store.ListAllRepoNamesCalls(), 1)
	assert.Equal(t, []RepoID{"group/a", "group/b"}, repos)
}

func TestStoreRepoListerReturnsEmptySliceForNoEnrolledRepos(t *testing.T) {
	t.Parallel()
	store := &repoNameListerMock{
		ListAllRepoNamesFunc: func(ctx context.Context) ([]string, error) {
			return nil, nil
		},
	}
	lister := NewStoreRepoLister(store)
	repos, err := lister.ListRepos(t.Context())
	require.NoError(t, err)
	assert.Empty(t, repos)
}

func TestStoreRepoListerWrapsUnderlyingError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("connection reset")
	store := &repoNameListerMock{
		ListAllRepoNamesFunc: func(ctx context.Context) ([]string, error) {
			return nil, wantErr
		},
	}
	lister := NewStoreRepoLister(store)
	_, err := lister.ListRepos(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}
