package workbranchstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/db/gen"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

var errBoom = errors.New("boom")

func validGenRow(id uuid.UUID) gen.WorkBranch {
	return gen.WorkBranch{ID: pgUUID(id), RepoID: pgUUID(uuid.Must(uuid.NewV7())), Name: "wb-test", Target: "main", State: "draft", Author: "grace-hopper-3-author", Conflict: "none"}
}

// TestCreate_UniqueViolation_ReturnsErrDuplicateName proves Create maps a
// work_branches_repo_id_name_key hit to the distinguishable
// errDuplicateName, not raw pgconn text -- the identity this bead's
// ACCEPTANCE CRITERIA names ("UNIQUE(repo_id,name) enforced").
func TestCreate_UniqueViolation_ReturnsErrDuplicateName(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: "23505", ConstraintName: "work_branches_repo_id_name_key"}
	mock := &querierMock{
		CreateWorkBranchFunc: func(_ context.Context, _ gen.CreateWorkBranchParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, pgErr
		},
	}
	store := New(mock, testLogger())
	_, err := store.Create(t.Context(), uuid.Must(uuid.NewV7()), "wb-dup", "main", "grace-hopper-3-author")
	require.Error(t, err)
	assert.ErrorIs(t, err, errDuplicateName)
}

// TestCreate_OtherError_NotMappedToDuplicateName proves an unrelated
// database failure is not mislabeled as a duplicate-name conflict.
func TestCreate_OtherError_NotMappedToDuplicateName(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		CreateWorkBranchFunc: func(_ context.Context, _ gen.CreateWorkBranchParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, errBoom
		},
	}
	store := New(mock, testLogger())
	_, err := store.Create(t.Context(), uuid.Must(uuid.NewV7()), "wb-x", "main", "grace-hopper-3-author")
	require.Error(t, err)
	assert.NotErrorIs(t, err, errDuplicateName)
	assert.ErrorIs(t, err, errBoom)
}

// TestCreate_Success_GeneratesIDAndReturnsRow proves a successful Create
// generates a fresh id (passed to CreateWorkBranch, not left zero) and
// surfaces the returned row through the package's own WorkBranch type.
func TestCreate_Success_GeneratesIDAndReturnsRow(t *testing.T) {
	t.Parallel()
	var gotID pgtype.UUID
	mock := &querierMock{
		CreateWorkBranchFunc: func(_ context.Context, arg gen.CreateWorkBranchParams) (gen.WorkBranch, error) {
			gotID = arg.ID
			assert.Equal(t, "wb-fresh", arg.Name)
			assert.Equal(t, "main", arg.Target)
			assert.Equal(t, "grace-hopper-3-author", arg.Author)
			return validGenRow(uuidFromPg(arg.ID)), nil
		},
	}
	store := New(mock, testLogger())
	wb, err := store.Create(t.Context(), uuid.Must(uuid.NewV7()), "wb-fresh", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	assert.True(t, gotID.Valid, "a generated id must be passed to CreateWorkBranch")
	assert.NotEqual(t, uuid.UUID{}, wb.ID)
	assert.Equal(t, StateDraft, wb.State)
	assert.Equal(t, ConflictNone, wb.Conflict)
}

// TestGet_NoRows_ReturnsErrNotFound proves Get maps pgx.ErrNoRows to the
// distinguishable errNotFound rather than a bare pgx sentinel a caller
// would otherwise have to import pgx just to check for.
func TestGet_NoRows_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		GetWorkBranchByIDFunc: func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, pgx.ErrNoRows
		},
	}
	store := New(mock, testLogger())
	_, err := store.Get(t.Context(), uuid.Must(uuid.NewV7()))
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

// TestGetByName_NoRows_ReturnsErrNotFound is GetByName's mirror of the
// above.
func TestGetByName_NoRows_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		GetWorkBranchByNameFunc: func(_ context.Context, _ gen.GetWorkBranchByNameParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, pgx.ErrNoRows
		},
	}
	store := New(mock, testLogger())
	_, err := store.GetByName(t.Context(), uuid.Must(uuid.NewV7()), "wb-missing")
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

// TestList_NilRepoIDFilter_PassesInvalidUUID proves a nil ListFilter.RepoID
// reaches ListWorkBranches/CountWorkBranches as an invalid (SQL NULL)
// pgtype.UUID, matching the "no filter" convention this package's SQL
// relies on -- a mutation that instead passed a zero-valued-but-Valid
// UUID would filter to (almost certainly) zero rows instead of every repo,
// which is exactly the class of bug this test guards against.
func TestList_NilRepoIDFilter_PassesInvalidUUID(t *testing.T) {
	t.Parallel()
	var listRepoID, countRepoID pgtype.UUID
	mock := &querierMock{
		ListWorkBranchesFunc: func(_ context.Context, arg gen.ListWorkBranchesParams) ([]gen.WorkBranch, error) {
			listRepoID = arg.Column1
			return nil, nil
		},
		CountWorkBranchesFunc: func(_ context.Context, arg gen.CountWorkBranchesParams) (int64, error) {
			countRepoID = arg.Column1
			return 0, nil
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.List(t.Context(), ListFilter{}, 100, 0)
	require.NoError(t, err)
	assert.False(t, listRepoID.Valid, "nil RepoID filter must reach ListWorkBranches as SQL NULL")
	assert.False(t, countRepoID.Valid, "nil RepoID filter must reach CountWorkBranches as SQL NULL")
}

// TestList_RepoIDFilter_PassesValidUUID proves a non-nil ListFilter.RepoID
// reaches both queries as the same valid UUID.
func TestList_RepoIDFilter_PassesValidUUID(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV7())
	var listRepoID, countRepoID pgtype.UUID
	mock := &querierMock{
		ListWorkBranchesFunc: func(_ context.Context, arg gen.ListWorkBranchesParams) ([]gen.WorkBranch, error) {
			listRepoID = arg.Column1
			return nil, nil
		},
		CountWorkBranchesFunc: func(_ context.Context, arg gen.CountWorkBranchesParams) (int64, error) {
			countRepoID = arg.Column1
			return 0, nil
		},
	}
	store := New(mock, testLogger())
	_, _, err := store.List(t.Context(), ListFilter{RepoID: &repoID}, 100, 0)
	require.NoError(t, err)
	assert.Equal(t, pgUUID(repoID), listRepoID)
	assert.Equal(t, pgUUID(repoID), countRepoID)
}

// TestList_ReturnsRowsAndTotal proves List surfaces both the paginated
// rows and CountWorkBranches' total, not just one or the other.
func TestList_ReturnsRowsAndTotal(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		ListWorkBranchesFunc: func(_ context.Context, _ gen.ListWorkBranchesParams) ([]gen.WorkBranch, error) {
			return []gen.WorkBranch{validGenRow(uuid.Must(uuid.NewV7()))}, nil
		},
		CountWorkBranchesFunc: func(_ context.Context, _ gen.CountWorkBranchesParams) (int64, error) {
			return int64(42), nil
		},
	}
	store := New(mock, testLogger())
	rows, total, err := store.List(t.Context(), ListFilter{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, int64(42), total, "total must be CountWorkBranches' value, not len(rows)")
}

// transitionCase names one (method, guard-miss) scenario shared by every
// transition method's not-found vs illegal-transition mapping tests below.
type transitionCase struct {
	name string
	call func(ctx context.Context, store *Store) (WorkBranch, error)
}

var testID = uuid.Must(uuid.NewV7())

var transitionCases = []transitionCase{
	{name: "SetTitleDescription", call: func(ctx context.Context, s *Store) (WorkBranch, error) {
		return s.SetTitleDescription(ctx, testID, "t", "d")
	}},
	{name: "UpdateState", call: func(ctx context.Context, s *Store) (WorkBranch, error) {
		return s.UpdateState(ctx, testID, StateReviewable)
	}},
	{name: "Close", call: func(ctx context.Context, s *Store) (WorkBranch, error) {
		return s.Close(ctx, testID, "abandoned")
	}},
	{name: "Complete", call: func(ctx context.Context, s *Store) (WorkBranch, error) {
		return s.Complete(ctx, testID)
	}},
	{name: "FlagConflict", call: func(ctx context.Context, s *Store) (WorkBranch, error) {
		return s.FlagConflict(ctx, testID)
	}},
	{name: "DemoteOnConflict", call: func(ctx context.Context, s *Store) (WorkBranch, error) {
		return s.DemoteOnConflict(ctx, testID)
	}},
	{name: "ClearConflict", call: func(ctx context.Context, s *Store) (WorkBranch, error) {
		return s.ClearConflict(ctx, testID)
	}},
}

// TestTransitionMethods_NotFound_ReturnErrNotFound proves every transition
// method maps a zero-row guarded UPDATE to errNotFound when the id truly
// does not exist (the transitionErr follow-up Get also finds nothing).
func TestTransitionMethods_NotFound_ReturnErrNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range transitionCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := allTransitionsNoRows()
			mock.GetWorkBranchByIDFunc = func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
				return gen.WorkBranch{}, pgx.ErrNoRows
			}
			store := New(mock, testLogger())
			_, err := tc.call(t.Context(), store)
			require.Error(t, err)
			assert.ErrorIs(t, err, errNotFound)
			assert.NotErrorIs(t, err, errIllegalTransition)
		})
	}
}

// TestTransitionMethods_IllegalTransition_ReturnErrIllegalTransition proves
// every transition method maps a zero-row guarded UPDATE to
// errIllegalTransition when the id DOES exist (the transitionErr
// follow-up Get finds a row) -- the illegal-transition state machine this
// bead exists to add.
func TestTransitionMethods_IllegalTransition_ReturnErrIllegalTransition(t *testing.T) {
	t.Parallel()
	for _, tc := range transitionCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := allTransitionsNoRows()
			mock.GetWorkBranchByIDFunc = func(_ context.Context, id pgtype.UUID) (gen.WorkBranch, error) {
				return validGenRow(uuidFromPg(id)), nil
			}
			store := New(mock, testLogger())
			_, err := tc.call(t.Context(), store)
			require.Error(t, err)
			assert.ErrorIs(t, err, errIllegalTransition)
			assert.NotErrorIs(t, err, errNotFound)
		})
	}
}

// TestTransitionMethods_OtherError_NotMapped proves a genuine transport
// failure (not a zero-row guard miss) is neither errNotFound nor
// errIllegalTransition -- only a real pgx.ErrNoRows triggers the
// existence-check follow-up.
func TestTransitionMethods_OtherError_NotMapped(t *testing.T) {
	t.Parallel()
	for _, tc := range transitionCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := allTransitionsError(errBoom)
			store := New(mock, testLogger())
			_, err := tc.call(t.Context(), store)
			require.Error(t, err)
			assert.ErrorIs(t, err, errBoom)
			assert.NotErrorIs(t, err, errNotFound)
			assert.NotErrorIs(t, err, errIllegalTransition)
		})
	}
}

// allTransitionsNoRows builds a querierMock whose every transition method
// returns pgx.ErrNoRows, the shape a guarded UPDATE produces when its
// WHERE clause matches nothing.
func allTransitionsNoRows() *querierMock {
	return allTransitionsError(pgx.ErrNoRows)
}

// allTransitionsError builds a querierMock whose every transition method
// returns err.
func allTransitionsError(err error) *querierMock {
	return &querierMock{
		SetWorkBranchTitleDescriptionFunc: func(_ context.Context, _ gen.SetWorkBranchTitleDescriptionParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
		UpdateWorkBranchStateFunc: func(_ context.Context, _ gen.UpdateWorkBranchStateParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
		CloseWorkBranchFunc: func(_ context.Context, _ gen.CloseWorkBranchParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
		CompleteWorkBranchFunc: func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
		FlagWorkBranchConflictFunc: func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
		DemoteWorkBranchOnConflictFunc: func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
		ClearWorkBranchConflictFunc: func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
		GetWorkBranchByIDFunc: func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
	}
}
