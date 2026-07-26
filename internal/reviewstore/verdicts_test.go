package reviewstore

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testWorkBranchID is a fixed work-branch id these unit tests pass
// through to the (mocked) store methods; its value is never inspected by
// the mock, only forwarded.
var testWorkBranchID = uuid.MustParse("019a0000-0000-7000-8000-000000000001")

// TestSubmit_UniqueViolation_ReturnsErrDuplicateVerdict proves the
// defensive error-identity mapping: IF a unique_violation on
// verdicts_round_id_reviewer_key ever reaches Submit (e.g. the ON
// CONFLICT clause is bypassed or removed), it surfaces as the stable
// errDuplicateVerdict rather than raw pgconn text. Exercised directly
// here via a mocked querier, since Submit's own upsert makes this path
// unreachable through a live database in normal operation -- verified by
// hand-running this exact mutation (temporarily dropping ON CONFLICT
// from SubmitVerdict's SQL, regenerating, and confirming
// TestSubmit_Resubmission_ReplacesNotDuplicates then fails with this
// same errDuplicateVerdict identity) rather than by a committed test,
// since Submit's normal codepath can never trigger it live.
func TestSubmit_UniqueViolation_ReturnsErrDuplicateVerdict(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: "23505", ConstraintName: "verdicts_round_id_reviewer_key"}
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: pgErr}
		},
	}
	s := NewVerdictStore(mock, testLogger())
	_, err := s.Submit(t.Context(), uuid.New(), "ada-lovelace-7-reviewer", OutcomeApprove)
	require.Error(t, err)
	assert.ErrorIs(t, err, errDuplicateVerdict)
}

// TestSubmit_OtherError_NotMappedToDuplicate proves an unrelated failure
// is not mislabeled as a duplicate-verdict conflict.
func TestSubmit_OtherError_NotMappedToDuplicate(t *testing.T) {
	t.Parallel()
	connErr := pgx.ErrTxClosed
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: connErr}
		},
	}
	s := NewVerdictStore(mock, testLogger())
	_, err := s.Submit(t.Context(), uuid.New(), "ada-lovelace-7-reviewer", OutcomeApprove)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errDuplicateVerdict)
	assert.ErrorIs(t, err, connErr)
}

// TestCurrentRoundApproveCount_QueryError_Wrapped proves a plain query
// failure is wrapped with work-branch context rather than swallowed or
// passed through bare.
func TestCurrentRoundApproveCount_QueryError_Wrapped(t *testing.T) {
	t.Parallel()
	connErr := pgx.ErrTxClosed
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: connErr}
		},
	}
	s := NewVerdictStore(mock, testLogger())
	_, err := s.CurrentRoundApproveCount(t.Context(), testWorkBranchID)
	require.Error(t, err)
	assert.ErrorIs(t, err, connErr)
	assert.Contains(t, err.Error(), testWorkBranchID.String())
}
