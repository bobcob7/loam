package reviewstore

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestOpenRound_UniqueViolation_ReturnsErrRoundNumberConflict proves the
// error-identity mapping: when Postgres reports a unique_violation on
// review_rounds_work_branch_id_number_key -- the shape a genuine
// concurrent-open race produces -- OpenRound surfaces errRoundNumberConflict,
// not the raw pgconn error, so a caller can match it with errors.Is.
func TestOpenRound_UniqueViolation_ReturnsErrRoundNumberConflict(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: "23505", ConstraintName: "review_rounds_work_branch_id_number_key"}
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: pgErr}
		},
	}
	s := NewRoundStore(mock, testLogger())
	_, err := s.OpenRound(t.Context(), testWorkBranchID, "grace-hopper-3-author")
	require.Error(t, err)
	assert.ErrorIs(t, err, errRoundNumberConflict)
}

// TestOpenRound_OtherError_NotMappedToRoundConflict proves an unrelated
// database failure is NOT mislabeled as a round-number conflict -- only
// the specific constraint violation maps to errRoundNumberConflict.
func TestOpenRound_OtherError_NotMappedToRoundConflict(t *testing.T) {
	t.Parallel()
	connErr := pgx.ErrTxClosed
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: connErr}
		},
	}
	s := NewRoundStore(mock, testLogger())
	_, err := s.OpenRound(t.Context(), testWorkBranchID, "grace-hopper-3-author")
	require.Error(t, err)
	assert.NotErrorIs(t, err, errRoundNumberConflict)
	assert.ErrorIs(t, err, connErr)
}

// TestCurrentRound_NoRows_ReturnsErrNoCurrentRound proves a work branch
// with no review_rounds row yet reports the distinguishable
// ErrNoCurrentRound rather than a bare pgx.ErrNoRows a caller has to know
// to check for separately.
func TestCurrentRound_NoRows_ReturnsErrNoCurrentRound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
	}
	s := NewRoundStore(mock, testLogger())
	_, err := s.CurrentRound(t.Context(), testWorkBranchID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoCurrentRound)
}
