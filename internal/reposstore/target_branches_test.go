package reposstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/db/gen"
)

func genTargetBranchFixture() gen.RepoTargetBranch {
	return gen.RepoTargetBranch{
		RepoID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		Branch: "main",
	}
}

func TestAddTargetBranchIsIdempotent(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		AddTargetBranchFunc: func(ctx context.Context, arg gen.AddTargetBranchParams) (gen.RepoTargetBranch, error) {
			assert.Equal(t, "main", arg.Branch)
			return genTargetBranchFixture(), nil
		},
	}
	store := NewStore(mock, testLogger())
	branch, err := store.AddTargetBranch(t.Context(), [16]byte{1}, "main")
	require.NoError(t, err)
	assert.Equal(t, "main", branch.Branch)
	assert.False(t, branch.IngestedRef.Ok, "a freshly-added target branch must report no ingested ref")
}

func TestListTargetBranchesConvertsRows(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		ListTargetBranchesFunc: func(ctx context.Context, repoID pgtype.UUID) ([]gen.RepoTargetBranch, error) {
			return []gen.RepoTargetBranch{genTargetBranchFixture()}, nil
		},
	}
	store := NewStore(mock, testLogger())
	branches, err := store.ListTargetBranches(t.Context(), [16]byte{1})
	require.NoError(t, err)
	require.Len(t, branches, 1)
	assert.Equal(t, "main", branches[0].Branch)
}

func TestRemoveTargetBranchNotFoundWhenZeroRowsAffected(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		RemoveTargetBranchFunc: func(ctx context.Context, arg gen.RemoveTargetBranchParams) (int64, error) {
			return 0, nil
		},
	}
	store := NewStore(mock, testLogger())
	err := store.RemoveTargetBranch(t.Context(), [16]byte{1}, "main")
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound, "a no-op delete (branch never enrolled) must map to errNotFound, not succeed silently")
}

func TestRemoveTargetBranchSucceedsWhenARowIsAffected(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		RemoveTargetBranchFunc: func(ctx context.Context, arg gen.RemoveTargetBranchParams) (int64, error) {
			return 1, nil
		},
	}
	store := NewStore(mock, testLogger())
	err := store.RemoveTargetBranch(t.Context(), [16]byte{1}, "main")
	assert.NoError(t, err)
}

func TestIngestedRefReportsNotOkWhenColumnIsNull(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		GetTargetBranchFunc: func(ctx context.Context, arg gen.GetTargetBranchParams) (gen.RepoTargetBranch, error) {
			row := genTargetBranchFixture()
			row.IngestedRef = pgtype.Text{Valid: false}
			return row, nil
		},
	}
	store := NewStore(mock, testLogger())
	ref, err := store.IngestedRef(t.Context(), [16]byte{1}, "main")
	require.NoError(t, err)
	assert.False(t, ref.Ok, "a NULL ingested_ref must report Ok=false, the full-rebuild signal")
	assert.Empty(t, ref.Ref)
}

func TestIngestedRefReportsOkWhenColumnIsSet(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		GetTargetBranchFunc: func(ctx context.Context, arg gen.GetTargetBranchParams) (gen.RepoTargetBranch, error) {
			row := genTargetBranchFixture()
			row.IngestedRef = pgtype.Text{String: "deadbeef", Valid: true}
			return row, nil
		},
	}
	store := NewStore(mock, testLogger())
	ref, err := store.IngestedRef(t.Context(), [16]byte{1}, "main")
	require.NoError(t, err)
	require.True(t, ref.Ok)
	assert.Equal(t, "deadbeef", ref.Ref)
}

func TestIngestedRefNotFoundWhenBranchNotEnrolled(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		GetTargetBranchFunc: func(ctx context.Context, arg gen.GetTargetBranchParams) (gen.RepoTargetBranch, error) {
			return gen.RepoTargetBranch{}, pgx.ErrNoRows
		},
	}
	store := NewStore(mock, testLogger())
	_, err := store.IngestedRef(t.Context(), [16]byte{1}, "main")
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

func TestAdvanceIngestedRefRejectsEmptyRefWithoutCallingTheDB(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		AdvanceIngestedRefFunc: func(ctx context.Context, arg gen.AdvanceIngestedRefParams) (gen.RepoTargetBranch, error) {
			t.Fatal("AdvanceIngestedRef must not be called with an empty ref")
			return gen.RepoTargetBranch{}, nil
		},
	}
	store := NewStore(mock, testLogger())
	_, err := store.AdvanceIngestedRef(t.Context(), [16]byte{1}, "main", "", time.Now(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errEmptyRef)
	assert.Empty(t, mock.AdvanceIngestedRefCalls())
}

func TestAdvanceIngestedRefWritesNonNullRef(t *testing.T) {
	t.Parallel()
	when := time.Unix(5000, 0)
	mock := &querierMock{
		AdvanceIngestedRefFunc: func(ctx context.Context, arg gen.AdvanceIngestedRefParams) (gen.RepoTargetBranch, error) {
			require.True(t, arg.IngestedRef.Valid, "AdvanceIngestedRef must never write ingested_ref as NULL")
			assert.Equal(t, "cafef00d", arg.IngestedRef.String)
			assert.True(t, arg.IngestedAt.Valid)
			row := genTargetBranchFixture()
			row.IngestedRef = arg.IngestedRef
			row.IngestedAt = arg.IngestedAt
			return row, nil
		},
	}
	store := NewStore(mock, testLogger())
	branch, err := store.AdvanceIngestedRef(t.Context(), [16]byte{1}, "main", "cafef00d", when, []byte(`{"grammar":1}`))
	require.NoError(t, err)
	require.True(t, branch.IngestedRef.Ok)
	assert.Equal(t, "cafef00d", branch.IngestedRef.Ref)
}

func TestAdvanceIngestedRefNotFoundWhenBranchNotEnrolled(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		AdvanceIngestedRefFunc: func(ctx context.Context, arg gen.AdvanceIngestedRefParams) (gen.RepoTargetBranch, error) {
			return gen.RepoTargetBranch{}, pgx.ErrNoRows
		},
	}
	store := NewStore(mock, testLogger())
	_, err := store.AdvanceIngestedRef(t.Context(), [16]byte{1}, "main", "cafef00d", time.Now(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}
