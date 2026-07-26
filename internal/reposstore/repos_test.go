package reposstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/db/gen"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func genRepoFixture() gen.Repo {
	return gen.Repo{
		ID:            pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		Name:          "group/repo",
		UpstreamUrl:   "https://example.com/group/repo.git",
		ForgeHost:     "example.com",
		IndexedBranch: "main",
		SyncState:     "idle",
		CreatedAt:     pgtype.Timestamptz{Time: time.Unix(1000, 0), Valid: true},
		UpdatedAt:     pgtype.Timestamptz{Time: time.Unix(2000, 0), Valid: true},
	}
}

func TestCreateRepoAssignsUUIDv7AndConvertsResult(t *testing.T) {
	t.Parallel()
	fixture := genRepoFixture()
	mock := &querierMock{
		CreateRepoFunc: func(ctx context.Context, arg gen.CreateRepoParams) (gen.Repo, error) {
			return fixture, nil
		},
	}
	store := NewStore(mock, testLogger())
	repo, err := store.CreateRepo(t.Context(), CreateRepoParams{
		Name:          "group/repo",
		UpstreamURL:   "https://example.com/group/repo.git",
		ForgeHost:     "example.com",
		IndexedBranch: "main",
	})
	require.NoError(t, err)
	require.Len(t, mock.CreateRepoCalls(), 1)
	gotID := mock.CreateRepoCalls()[0].Arg.ID
	assert.True(t, gotID.Valid, "CreateRepo must pass a valid id, not the zero value")
	assert.Equal(t, byte(7), gotID.Bytes[6]>>4, "the assigned id must be a UUIDv7 (version nibble 7)")
	assert.Equal(t, "group/repo", repo.Name)
	assert.Equal(t, "idle", repo.SyncState)
	assert.True(t, repo.CreatedAt.Equal(time.Unix(1000, 0)))
}

func TestCreateRepoWrapsUnderlyingError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("unique violation")
	mock := &querierMock{
		CreateRepoFunc: func(ctx context.Context, arg gen.CreateRepoParams) (gen.Repo, error) {
			return gen.Repo{}, wantErr
		},
	}
	store := NewStore(mock, testLogger())
	_, err := store.CreateRepo(t.Context(), CreateRepoParams{Name: "group/repo"})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestGetRepoByIDNotFoundMapsToErrNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		GetRepoByIDFunc: func(ctx context.Context, id pgtype.UUID) (gen.Repo, error) {
			return gen.Repo{}, pgx.ErrNoRows
		},
	}
	store := NewStore(mock, testLogger())
	_, err := store.GetRepoByID(t.Context(), [16]byte{9})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetRepoByNameResolvesViaSingleIndexedLookup(t *testing.T) {
	t.Parallel()
	fixture := genRepoFixture()
	mock := &querierMock{
		GetRepoByNameFunc: func(ctx context.Context, name string) (gen.Repo, error) {
			assert.Equal(t, "group/repo", name)
			return fixture, nil
		},
	}
	store := NewStore(mock, testLogger())
	repo, err := store.GetRepoByName(t.Context(), "group/repo")
	require.NoError(t, err)
	require.Len(t, mock.GetRepoByNameCalls(), 1, "GetRepoByName must be a single lookup, not a scan-and-filter")
	assert.Equal(t, uuidFromPG(fixture.ID), repo.ID)
}

func TestGetRepoByNameNotFoundMapsToErrNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		GetRepoByNameFunc: func(ctx context.Context, name string) (gen.Repo, error) {
			return gen.Repo{}, pgx.ErrNoRows
		},
	}
	store := NewStore(mock, testLogger())
	_, err := store.GetRepoByName(t.Context(), "missing/repo")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListReposDefaultsNonPositiveLimit(t *testing.T) {
	t.Parallel()
	// CountRepos deliberately returns a value (7) that disagrees with the
	// single row ListRepos returns: if ListRepos ever ignored CountRepos
	// and derived Total from len(repos) instead (a real mutation this
	// caught), asserting Total == 1 would still pass. Asserting Total == 7
	// only holds if Store.ListRepos actually plumbs CountRepos's result
	// through, not the page length.
	mock := &querierMock{
		ListReposFunc: func(ctx context.Context, arg gen.ListReposParams) ([]gen.Repo, error) {
			return []gen.Repo{genRepoFixture()}, nil
		},
		CountReposFunc: func(ctx context.Context) (int64, error) { return 7, nil },
	}
	store := NewStore(mock, testLogger())
	result, err := store.ListRepos(t.Context(), Page{})
	require.NoError(t, err)
	require.Len(t, mock.ListReposCalls(), 1)
	assert.Equal(t, int32(defaultListLimit), mock.ListReposCalls()[0].Arg.Limit)
	assert.Equal(t, 7, result.Total, "Total must come from CountRepos, not len(result.Repos)")
	require.Len(t, result.Repos, 1)
}

func TestListReposPassesThroughExplicitLimitAndOffset(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		ListReposFunc: func(ctx context.Context, arg gen.ListReposParams) ([]gen.Repo, error) {
			return nil, nil
		},
		CountReposFunc: func(ctx context.Context) (int64, error) { return 0, nil },
	}
	store := NewStore(mock, testLogger())
	_, err := store.ListRepos(t.Context(), Page{Limit: 10, Offset: 20})
	require.NoError(t, err)
	arg := mock.ListReposCalls()[0].Arg
	assert.Equal(t, int32(10), arg.Limit)
	assert.Equal(t, int32(20), arg.Offset)
}

func TestListAllRepoNamesReturnsUnderlyingNames(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		ListRepoNamesFunc: func(ctx context.Context) ([]string, error) {
			return []string{"group/a", "group/b"}, nil
		},
	}
	store := NewStore(mock, testLogger())
	names, err := store.ListAllRepoNames(t.Context())
	require.NoError(t, err)
	require.Len(t, mock.ListRepoNamesCalls(), 1)
	assert.Equal(t, []string{"group/a", "group/b"}, names)
}

func TestListAllRepoNamesWrapsUnderlyingError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("connection reset")
	mock := &querierMock{
		ListRepoNamesFunc: func(ctx context.Context) ([]string, error) {
			return nil, wantErr
		},
	}
	store := NewStore(mock, testLogger())
	_, err := store.ListAllRepoNames(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestUpdateRepoNotFoundMapsToErrNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		UpdateRepoFunc: func(ctx context.Context, arg gen.UpdateRepoParams) (gen.Repo, error) {
			return gen.Repo{}, pgx.ErrNoRows
		},
	}
	store := NewStore(mock, testLogger())
	_, err := store.UpdateRepo(t.Context(), [16]byte{1}, UpdateRepoParams{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestUpdateRepoOmitsNameFromParams(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		UpdateRepoFunc: func(ctx context.Context, arg gen.UpdateRepoParams) (gen.Repo, error) {
			assert.Equal(t, "new-host.example.com", arg.ForgeHost)
			return genRepoFixture(), nil
		},
	}
	store := NewStore(mock, testLogger())
	_, err := store.UpdateRepo(t.Context(), [16]byte{1}, UpdateRepoParams{
		UpstreamURL:   "https://example.com/group/repo.git",
		ForgeHost:     "new-host.example.com",
		IndexedBranch: "main",
	})
	require.NoError(t, err)
	require.Len(t, mock.UpdateRepoCalls(), 1)
}
