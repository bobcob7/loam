package ingestjobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestClaim_NoRows_ReturnsErrNoJobAvailable proves Claim maps an empty
// result set to its own sentinel, not a bare pgx.ErrNoRows a caller has to
// separately know to check for.
func TestClaim_NoRows_ReturnsErrNoJobAvailable(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.Claim(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, errNoJobAvailable)
}

// TestClaim_OtherError_NotMappedToNoJobAvailable proves a genuine database
// failure (e.g. a dropped connection) surfaces as itself, never
// downgraded into the ordinary "nothing to claim" outcome -- a worker
// logging that error must be able to tell the two apart.
func TestClaim_OtherError_NotMappedToNoJobAvailable(t *testing.T) {
	t.Parallel()
	connErr := errors.New("connection refused")
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: connErr}
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.Claim(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, connErr)
	assert.NotErrorIs(t, err, errNoJobAvailable)
}

// TestComplete_ZeroRowsButJobExists_ReturnsErrIllegalTransition drives
// transitionErr's classification: the guarded UPDATE matches zero rows,
// but a follow-up Get finds the row, so the job exists in a status that
// disqualified the write.
func TestComplete_ZeroRowsButJobExists_ReturnsErrIllegalTransition(t *testing.T) {
	t.Parallel()
	calls := 0
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			calls++
			if calls == 1 {
				return fakeRow{err: pgx.ErrNoRows} // the guarded UPDATE itself
			}
			return fakeRow{err: nil} // the classification Get
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.Complete(t.Context(), uuid.Must(uuid.NewV7()), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errIllegalTransition)
	assert.NotErrorIs(t, err, errNotFound)
}

// TestComplete_ZeroRowsAndJobMissing_ReturnsErrNotFound is the other half
// of the same classification: both the guarded UPDATE and the follow-up
// Get find nothing, so the id names no row at all.
func TestComplete_ZeroRowsAndJobMissing_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.Complete(t.Context(), uuid.Must(uuid.NewV7()), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
	assert.NotErrorIs(t, err, errIllegalTransition)
}

// TestComplete_OtherError_PropagatesWithoutClassifying proves a genuine
// failure on the guarded UPDATE itself (not a zero-row result) is
// returned as-is, without spending a second round trip on a
// classification Get it does not need.
func TestComplete_OtherError_PropagatesWithoutClassifying(t *testing.T) {
	t.Parallel()
	execErr := errors.New("connection refused")
	calls := 0
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			calls++
			return fakeRow{err: execErr}
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.Complete(t.Context(), uuid.Must(uuid.NewV7()), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, execErr)
	assert.Equal(t, 1, calls, "a non-zero-row failure must not trigger the classification Get")
}

// TestFail_ZeroRowsButJobExists_ReturnsErrIllegalTransition mirrors
// Complete's classification test for Fail's own guarded UPDATE.
func TestFail_ZeroRowsButJobExists_ReturnsErrIllegalTransition(t *testing.T) {
	t.Parallel()
	calls := 0
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			calls++
			if calls == 1 {
				return fakeRow{err: pgx.ErrNoRows}
			}
			return fakeRow{err: nil}
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.Fail(t.Context(), uuid.Must(uuid.NewV7()), "boom")
	require.Error(t, err)
	assert.ErrorIs(t, err, errIllegalTransition)
}

// TestRequeue_ZeroRowsButJobExists_ReturnsErrIllegalTransition mirrors the
// same classification for Requeue's own guarded UPDATE.
func TestRequeue_ZeroRowsButJobExists_ReturnsErrIllegalTransition(t *testing.T) {
	t.Parallel()
	calls := 0
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			calls++
			if calls == 1 {
				return fakeRow{err: pgx.ErrNoRows}
			}
			return fakeRow{err: nil}
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.Requeue(t.Context(), uuid.Must(uuid.NewV7()))
	require.Error(t, err)
	assert.ErrorIs(t, err, errIllegalTransition)
}

// TestGet_NoRows_ReturnsErrNotFound is Get's own unit-level check,
// mirrored from the integration suite's real-database version.
func TestGet_NoRows_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.Get(t.Context(), uuid.Must(uuid.NewV7()))
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

// TestList_NonPositiveLimit_UsesDefault proves List substitutes
// defaultListLimit for a non-positive caller limit, matching this
// codebase's "0 means use the server default" convention (e.g.
// reposstore.Store.ListRepos) -- captured from the actual arguments
// List hands to the generated query rather than asserted from behavior a
// live database would be needed to observe.
func TestList_NonPositiveLimit_UsesDefault(t *testing.T) {
	t.Parallel()
	queryErr := errors.New("stop before iterating rows; only the arguments matter here")
	var gotLimit int32
	mock := &querierMock{
		QueryFunc: func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
			gotLimit = args[2].(int32)
			return nil, queryErr
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.List(t.Context(), ListFilter{}, 0, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, queryErr)
	assert.EqualValues(t, defaultListLimit, gotLimit)
}

// TestList_PositiveLimit_PassedThroughUnchanged is the positive control
// for the default-substitution test above.
func TestList_PositiveLimit_PassedThroughUnchanged(t *testing.T) {
	t.Parallel()
	queryErr := errors.New("stop before iterating rows; only the arguments matter here")
	var gotLimit int32
	mock := &querierMock{
		QueryFunc: func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
			gotLimit = args[2].(int32)
			return nil, queryErr
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.List(t.Context(), ListFilter{}, 7, 0)
	require.Error(t, err)
	assert.EqualValues(t, 7, gotLimit)
}

// TestFilterColumns_NilRepoID_LeavesColumnInvalid proves ListFilter's "no
// filter" sentinel (RepoID nil) translates to an invalid (SQL NULL)
// pgtype.UUID, never the zero UUID -- a real filter value that would
// wrongly match nothing.
func TestFilterColumns_NilRepoID_LeavesColumnInvalid(t *testing.T) {
	t.Parallel()
	repoID, status := filterColumns(ListFilter{})
	assert.False(t, repoID.Valid)
	assert.Equal(t, "", status)
}

// TestFilterColumns_SetRepoID_ProducesValidColumn is the positive control.
func TestFilterColumns_SetRepoID_ProducesValidColumn(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV7())
	repoID, status := filterColumns(ListFilter{RepoID: &id, Status: StatusFailed})
	require.True(t, repoID.Valid)
	assert.Equal(t, id, uuid.UUID(repoID.Bytes))
	assert.Equal(t, "failed", status)
}
