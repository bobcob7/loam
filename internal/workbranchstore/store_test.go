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
	return gen.WorkBranch{ID: pgUUID(id), RepoID: pgUUID(uuid.Must(uuid.NewV7())), Name: "wb-test", Target: "main", State: "draft", Author: "grace-hopper-3-author", Conflict: "none", UpstreamDrift: "none"}
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
// distinguishable ErrNotFound rather than a bare pgx sentinel a caller
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
	assert.ErrorIs(t, err, ErrNotFound)
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
	assert.ErrorIs(t, err, ErrNotFound)
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

// TestList_CountParamsMatchListParams_AllFilters proves List builds the
// SAME five filter columns for both the ListWorkBranches and
// CountWorkBranches calls, for every filter field populated at once.
// Without this, a code change that only touches one of the two call sites
// (e.g. dropping Column4 from CountWorkBranches alone) can silently pass
// every other test: a mismatch here would make Count disagree with what
// List actually returned, but nothing else in this package's unit suite
// compares the two calls' params directly.
func TestList_CountParamsMatchListParams_AllFilters(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV7())
	var listParams gen.ListWorkBranchesParams
	var countParams gen.CountWorkBranchesParams
	mock := &querierMock{
		ListWorkBranchesFunc: func(_ context.Context, arg gen.ListWorkBranchesParams) ([]gen.WorkBranch, error) {
			listParams = arg
			return nil, nil
		},
		CountWorkBranchesFunc: func(_ context.Context, arg gen.CountWorkBranchesParams) (int64, error) {
			countParams = arg
			return 0, nil
		},
	}
	store := New(mock, testLogger())
	filter := ListFilter{RepoID: &repoID, Target: "main", Author: "grace-hopper-3-author", State: StateReviewable, AwaitingVerdictReviewer: "ada-lovelace-7-reviewer"}
	_, _, err := store.List(t.Context(), filter, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, listParams.Column1, countParams.Column1, "repo_id filter must match between List and Count")
	assert.Equal(t, listParams.Column2, countParams.Column2, "target filter must match between List and Count")
	assert.Equal(t, listParams.Column3, countParams.Column3, "author filter must match between List and Count")
	assert.Equal(t, listParams.Column4, countParams.Column4, "state filter must match between List and Count")
	assert.Equal(t, listParams.Column5, countParams.Column5, "awaiting-verdict filter must match between List and Count")
	assert.Equal(t, "main", listParams.Column2, "sanity: the filter actually reached the mock")
	assert.Equal(t, string(StateReviewable), listParams.Column4)
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
	{name: "MarkConflicted", call: func(ctx context.Context, s *Store) (WorkBranch, error) {
		return s.MarkConflicted(ctx, testID)
	}},
	{name: "ClearConflict", call: func(ctx context.Context, s *Store) (WorkBranch, error) {
		return s.ClearConflict(ctx, testID)
	}},
	{name: "SetUpstreamDrift", call: func(ctx context.Context, s *Store) (WorkBranch, error) {
		return s.SetUpstreamDrift(ctx, testID, DriftDiverged)
	}},
}

// TestTransitionMethods_NotFound_ReturnErrNotFound proves every transition
// method maps a zero-row guarded UPDATE to ErrNotFound when the id truly
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
			assert.ErrorIs(t, err, ErrNotFound)
			assert.NotErrorIs(t, err, ErrIllegalTransition)
		})
	}
}

// TestTransitionMethods_IllegalTransition_ReturnErrIllegalTransition proves
// every transition method maps a zero-row guarded UPDATE to
// ErrIllegalTransition when the id DOES exist (the transitionErr
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
			assert.ErrorIs(t, err, ErrIllegalTransition)
			assert.NotErrorIs(t, err, ErrNotFound)
		})
	}
}

// TestTransitionMethods_OtherError_NotMapped proves a genuine transport
// failure (not a zero-row guard miss) is neither ErrNotFound nor
// ErrIllegalTransition -- only a real pgx.ErrNoRows triggers the
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
			assert.NotErrorIs(t, err, ErrNotFound)
			assert.NotErrorIs(t, err, ErrIllegalTransition)
		})
	}
}

// TestTransitionMethods_ClassificationGetFails_WrapsTransportError proves
// transitionErr's follow-up Get is itself fallible in a THIRD way beyond
// "row exists" (ErrIllegalTransition) and "no row" (ErrNotFound): a
// genuine transport failure (dropped connection, cancelled context) during
// that classification Get must be reported as its own wrapped error, not
// misreported as ErrIllegalTransition -- errors.Is(getErr, pgx.ErrNoRows)
// is false for both a real row AND a transport failure, so the two must be
// told apart explicitly rather than by falling through.
func TestTransitionMethods_ClassificationGetFails_WrapsTransportError(t *testing.T) {
	t.Parallel()
	for _, tc := range transitionCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := allTransitionsNoRows()
			mock.GetWorkBranchByIDFunc = func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
				return gen.WorkBranch{}, errBoom
			}
			store := New(mock, testLogger())
			_, err := tc.call(t.Context(), store)
			require.Error(t, err)
			assert.ErrorIs(t, err, errBoom, "a transport failure classifying the transition must not be swallowed")
			assert.NotErrorIs(t, err, ErrIllegalTransition, "a failed classification must not default to illegal-transition")
			assert.NotErrorIs(t, err, ErrNotFound)
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
		MarkWorkBranchConflictedFunc: func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
		ClearWorkBranchConflictFunc: func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
		SetWorkBranchUpstreamDriftFunc: func(_ context.Context, _ gen.SetWorkBranchUpstreamDriftParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
		GetWorkBranchByIDFunc: func(_ context.Context, _ pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, err
		},
	}
}

// TestRecordUpstreamPR_WritesAllThreeColumns proves the happy path passes
// the url, the number, AND the accepted tip through in one statement -- a
// row carrying a number with no URL (or no tip) would be a half-accepted
// proposal no reader in the tree knows how to interpret, and loam-cgg's
// ListProposals has nothing to compare against a NULL tip but "not proven
// up to date".
func TestRecordUpstreamPR_WritesAllThreeColumns(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV7())
	var got gen.RecordWorkBranchUpstreamPRParams
	mock := &querierMock{
		RecordWorkBranchUpstreamPRFunc: func(_ context.Context, arg gen.RecordWorkBranchUpstreamPRParams) (gen.WorkBranch, error) {
			got = arg
			row := validGenRow(id)
			row.UpstreamPrUrl, row.UpstreamPrNumber, row.AcceptedTip = arg.UpstreamPrUrl, arg.UpstreamPrNumber, arg.AcceptedTip
			return row, nil
		},
	}
	wb, err := New(mock, testLogger()).RecordUpstreamPR(t.Context(), id, "https://forge.example.com/g/r/pulls/7", 7, "deadbeef")
	require.NoError(t, err)
	assert.Equal(t, "https://forge.example.com/g/r/pulls/7", got.UpstreamPrUrl.String)
	assert.True(t, got.UpstreamPrUrl.Valid)
	assert.Equal(t, int32(7), got.UpstreamPrNumber.Int32)
	assert.True(t, got.UpstreamPrNumber.Valid)
	assert.Equal(t, "deadbeef", got.AcceptedTip.String)
	assert.True(t, got.AcceptedTip.Valid)
	require.NotNil(t, wb.UpstreamPRNumber)
	assert.Equal(t, int32(7), *wb.UpstreamPRNumber)
	require.NotNil(t, wb.AcceptedTip)
	assert.Equal(t, "deadbeef", *wb.AcceptedTip)
}

// TestRecordUpstreamPR_RejectsAnUnusableIdentity proves a PR number, URL,
// or accepted tip that cannot name a real pull request/commit never
// reaches the database at all. The column pair has a one-shot guard, so
// writing #0 would BOTH park the branch in the PR poller's poll set
// forever and consume the row's single chance to record the real PR; an
// empty tip would leave loam-cgg's ListProposals comparison with nothing
// to compare against.
func TestRecordUpstreamPR_RejectsAnUnusableIdentity(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		url    string
		number int32
		tip    string
	}{
		"zero number":     {url: "https://forge.example.com/g/r/pulls/1", number: 0, tip: "deadbeef"},
		"negative number": {url: "https://forge.example.com/g/r/pulls/1", number: -3, tip: "deadbeef"},
		"empty url":       {url: "", number: 7, tip: "deadbeef"},
		"empty tip":       {url: "https://forge.example.com/g/r/pulls/1", number: 7, tip: ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			mock := &querierMock{
				RecordWorkBranchUpstreamPRFunc: func(context.Context, gen.RecordWorkBranchUpstreamPRParams) (gen.WorkBranch, error) {
					calls++
					return validGenRow(uuid.Must(uuid.NewV7())), nil
				},
			}
			_, err := New(mock, testLogger()).RecordUpstreamPR(t.Context(), uuid.Must(uuid.NewV7()), tc.url, tc.number, tc.tip)
			require.ErrorIs(t, err, errInvalidUpstreamPR)
			assert.Zero(t, calls, "an unusable identity must never reach the guarded UPDATE")
		})
	}
}

// TestRecordUpstreamPR_ZeroRowsWithARecordedNumberIsTheRace proves the
// guarded UPDATE's zero-row outcome is reported as ErrPRAlreadyRecorded
// when the row already carries a number -- the concurrent-accept case its
// caller adopts, rather than a failure it retries.
func TestRecordUpstreamPR_ZeroRowsWithARecordedNumberIsTheRace(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV7())
	existing := validGenRow(id)
	existing.UpstreamPrNumber = pgtype.Int4{Int32: 42, Valid: true}
	mock := &querierMock{
		RecordWorkBranchUpstreamPRFunc: func(context.Context, gen.RecordWorkBranchUpstreamPRParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, pgx.ErrNoRows
		},
		GetWorkBranchByIDFunc: func(context.Context, pgtype.UUID) (gen.WorkBranch, error) { return existing, nil },
	}
	_, err := New(mock, testLogger()).RecordUpstreamPR(t.Context(), id, "https://forge.example.com/g/r/pulls/7", 7, "deadbeef")
	require.ErrorIs(t, err, ErrPRAlreadyRecorded)
	assert.NotErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "42", "the error must name the number that actually won the column")
}

// TestRecordUpstreamPR_ZeroRowsWithNoSuchRowIsNotFound proves the OTHER
// cause of zero rows is kept distinct: a missing row is ErrNotFound, never
// the race sentinel a caller would happily proceed on.
func TestRecordUpstreamPR_ZeroRowsWithNoSuchRowIsNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		RecordWorkBranchUpstreamPRFunc: func(context.Context, gen.RecordWorkBranchUpstreamPRParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, pgx.ErrNoRows
		},
		GetWorkBranchByIDFunc: func(context.Context, pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, pgx.ErrNoRows
		},
	}
	_, err := New(mock, testLogger()).RecordUpstreamPR(t.Context(), uuid.Must(uuid.NewV7()), "https://forge.example.com/g/r/pulls/7", 7, "deadbeef")
	require.ErrorIs(t, err, ErrNotFound)
	assert.NotErrorIs(t, err, ErrPRAlreadyRecorded)
}

// TestRecordUpstreamPR_TransportFailureIsNotDowngraded proves a dropped
// connection is reported as itself rather than misread as either
// precondition sentinel -- the same rule transitionErr follows, and the
// conflation class this repo has been bitten by before.
func TestRecordUpstreamPR_TransportFailureIsNotDowngraded(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		RecordWorkBranchUpstreamPRFunc: func(context.Context, gen.RecordWorkBranchUpstreamPRParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, errBoom
		},
		GetWorkBranchByIDFunc: func(context.Context, pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, errBoom
		},
	}
	_, err := New(mock, testLogger()).RecordUpstreamPR(t.Context(), uuid.Must(uuid.NewV7()), "https://forge.example.com/g/r/pulls/7", 7, "deadbeef")
	require.ErrorIs(t, err, errBoom)
	assert.NotErrorIs(t, err, ErrPRAlreadyRecorded)
	assert.NotErrorIs(t, err, ErrNotFound)
}

// TestRecordUpstreamPR_ClassificationFailureIsNotTheRace proves a
// classification read that fails for a THIRD reason (a dropped connection
// while looking the row up) is not reported as either sentinel -- a
// "not found" reading there would be a guess.
func TestRecordUpstreamPR_ClassificationFailureIsNotTheRace(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		RecordWorkBranchUpstreamPRFunc: func(context.Context, gen.RecordWorkBranchUpstreamPRParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, pgx.ErrNoRows
		},
		GetWorkBranchByIDFunc: func(context.Context, pgtype.UUID) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, errBoom
		},
	}
	_, err := New(mock, testLogger()).RecordUpstreamPR(t.Context(), uuid.Must(uuid.NewV7()), "https://forge.example.com/g/r/pulls/7", 7, "deadbeef")
	require.ErrorIs(t, err, errBoom)
	assert.NotErrorIs(t, err, ErrPRAlreadyRecorded)
	assert.NotErrorIs(t, err, ErrNotFound)
}

// TestRecordAcceptedTip_WritesTheColumnUnconditionally proves a re-accept's
// tip refresh reaches the database with no guard -- unlike
// RecordUpstreamPR's one-shot url/number pair, this column is expected to
// be overwritten on every re-accept (loam-cgg).
func TestRecordAcceptedTip_WritesTheColumnUnconditionally(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV7())
	var got gen.RecordWorkBranchAcceptedTipParams
	mock := &querierMock{
		RecordWorkBranchAcceptedTipFunc: func(_ context.Context, arg gen.RecordWorkBranchAcceptedTipParams) (gen.WorkBranch, error) {
			got = arg
			row := validGenRow(id)
			row.AcceptedTip = arg.AcceptedTip
			return row, nil
		},
	}
	wb, err := New(mock, testLogger()).RecordAcceptedTip(t.Context(), id, "cafef00d")
	require.NoError(t, err)
	assert.Equal(t, pgUUID(id), got.ID)
	assert.Equal(t, "cafef00d", got.AcceptedTip.String)
	assert.True(t, got.AcceptedTip.Valid)
	require.NotNil(t, wb.AcceptedTip)
	assert.Equal(t, "cafef00d", *wb.AcceptedTip)
}

// TestRecordAcceptedTip_RejectsAnEmptyTip proves an empty tip never reaches
// the database -- it would leave loam-cgg's ListProposals comparison with
// nothing to compare against.
func TestRecordAcceptedTip_RejectsAnEmptyTip(t *testing.T) {
	t.Parallel()
	calls := 0
	mock := &querierMock{
		RecordWorkBranchAcceptedTipFunc: func(context.Context, gen.RecordWorkBranchAcceptedTipParams) (gen.WorkBranch, error) {
			calls++
			return validGenRow(uuid.Must(uuid.NewV7())), nil
		},
	}
	_, err := New(mock, testLogger()).RecordAcceptedTip(t.Context(), uuid.Must(uuid.NewV7()), "")
	require.ErrorIs(t, err, errInvalidUpstreamPR)
	assert.Zero(t, calls, "an empty tip must never reach the UPDATE")
}

// TestRecordAcceptedTip_NoSuchRowIsNotFound proves a missing id maps to
// ErrNotFound rather than a bare pgx sentinel.
func TestRecordAcceptedTip_NoSuchRowIsNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		RecordWorkBranchAcceptedTipFunc: func(context.Context, gen.RecordWorkBranchAcceptedTipParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, pgx.ErrNoRows
		},
	}
	_, err := New(mock, testLogger()).RecordAcceptedTip(t.Context(), uuid.Must(uuid.NewV7()), "cafef00d")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestRecordAcceptedTip_TransportFailureIsNotDowngraded proves a dropped
// connection is reported as itself, not misread as ErrNotFound.
func TestRecordAcceptedTip_TransportFailureIsNotDowngraded(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		RecordWorkBranchAcceptedTipFunc: func(context.Context, gen.RecordWorkBranchAcceptedTipParams) (gen.WorkBranch, error) {
			return gen.WorkBranch{}, errBoom
		},
	}
	_, err := New(mock, testLogger()).RecordAcceptedTip(t.Context(), uuid.Must(uuid.NewV7()), "cafef00d")
	require.ErrorIs(t, err, errBoom)
	assert.NotErrorIs(t, err, ErrNotFound)
}

// TestFromGenWorkBranch_AcceptedTipNullReadsAsNilPointer proves a NULL
// accepted_tip column (every row that predates loam-cgg, or a reviewed
// branch never yet accepted) reads back as a nil pointer, never as an
// empty-but-non-nil string that a caller could mistake for "resolved to
// nothing" -- the distinction ListProposals's over-inclusion rule depends
// on (see internal/handler/proposal).
func TestFromGenWorkBranch_AcceptedTipNullReadsAsNilPointer(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV7())
	mock := &querierMock{
		GetWorkBranchByIDFunc: func(context.Context, pgtype.UUID) (gen.WorkBranch, error) {
			return validGenRow(id), nil
		},
	}
	wb, err := New(mock, testLogger()).Get(t.Context(), id)
	require.NoError(t, err)
	assert.Nil(t, wb.AcceptedTip, "a NULL accepted_tip column must read back as a nil pointer")
}

// TestSetUpstreamDrift_WritesTheObservedValue proves the value the caller
// observed is the value that reaches the statement -- both of them, since
// the whole point of this column is that it goes back to 'none' when the
// operator reconciles upstream by hand and no other write path can do that.
func TestSetUpstreamDrift_WritesTheObservedValue(t *testing.T) {
	t.Parallel()
	for _, want := range []UpstreamDrift{DriftDiverged, DriftNone} {
		t.Run(string(want), func(t *testing.T) {
			t.Parallel()
			id := uuid.Must(uuid.NewV7())
			var got gen.SetWorkBranchUpstreamDriftParams
			mock := &querierMock{
				SetWorkBranchUpstreamDriftFunc: func(_ context.Context, arg gen.SetWorkBranchUpstreamDriftParams) (gen.WorkBranch, error) {
					got = arg
					row := validGenRow(id)
					row.UpstreamDrift = arg.UpstreamDrift
					return row, nil
				},
			}
			wb, err := New(mock, testLogger()).SetUpstreamDrift(t.Context(), id, want)
			require.NoError(t, err)
			assert.Equal(t, string(want), got.UpstreamDrift)
			assert.Equal(t, pgUUID(id), got.ID)
			assert.Equal(t, want, wb.UpstreamDrift, "the returned row must carry the value the statement wrote back")
		})
	}
}

// TestSetUpstreamDrift_RejectsAValueOutsideTheCheckConstraint proves a
// third value never reaches Postgres to fail as SQLSTATE 23514: the guard
// is in Go, ahead of the statement, so the sync cycle's log names the bad
// value rather than quoting a constraint.
func TestSetUpstreamDrift_RejectsAValueOutsideTheCheckConstraint(t *testing.T) {
	t.Parallel()
	called := false
	mock := &querierMock{
		SetWorkBranchUpstreamDriftFunc: func(_ context.Context, _ gen.SetWorkBranchUpstreamDriftParams) (gen.WorkBranch, error) {
			called = true
			return gen.WorkBranch{}, nil
		},
	}
	_, err := New(mock, testLogger()).SetUpstreamDrift(t.Context(), uuid.Must(uuid.NewV7()), UpstreamDrift("fast_forward"))
	require.Error(t, err)
	assert.ErrorIs(t, err, errInvalidUpstreamDrift)
	assert.False(t, called, "an illegal value must not reach the database at all")
}

// TestFromGenWorkBranch_UpstreamDriftIsCarriedThrough proves the column is
// actually read back onto the struct. Without it every consumer -- the
// accept preconditions, the proposal queue's exclusion, the proto surface
// -- would silently see the Go zero value on every row.
func TestFromGenWorkBranch_UpstreamDriftIsCarriedThrough(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV7())
	mock := &querierMock{
		GetWorkBranchByIDFunc: func(context.Context, pgtype.UUID) (gen.WorkBranch, error) {
			row := validGenRow(id)
			row.UpstreamDrift = string(DriftDiverged)
			return row, nil
		},
	}
	wb, err := New(mock, testLogger()).Get(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, DriftDiverged, wb.UpstreamDrift)
}
