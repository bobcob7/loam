package reviewstore

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// TestIsUniqueViolation_MatchingConstraint proves the happy path: a real
// pgconn.PgError with the unique_violation code and the exact constraint
// name is recognized, even wrapped by fmt.Errorf (errors.As must unwrap).
func TestIsUniqueViolation_MatchingConstraint(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: "23505", ConstraintName: "verdicts_round_id_reviewer_key"}
	wrapped := fmt.Errorf("submitting verdict: %w", pgErr)
	assert.True(t, isUniqueViolation(wrapped, "verdicts_round_id_reviewer_key"))
}

// TestIsUniqueViolation_WrongConstraint proves a unique_violation on a
// DIFFERENT constraint is not conflated with the one the caller asked
// about -- a mutation that drops the ConstraintName check would make this
// fail.
func TestIsUniqueViolation_WrongConstraint(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: "23505", ConstraintName: "some_other_constraint"}
	assert.False(t, isUniqueViolation(pgErr, "verdicts_round_id_reviewer_key"))
}

// TestIsUniqueViolation_WrongCode proves a non-unique-violation pg error
// (even against the right constraint name, which would not normally
// happen, but exercises the Code check independently) is not misreported.
func TestIsUniqueViolation_WrongCode(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: "23503", ConstraintName: "verdicts_round_id_reviewer_key"}
	assert.False(t, isUniqueViolation(pgErr, "verdicts_round_id_reviewer_key"))
}

// TestIsUniqueViolation_NotAPgError proves a plain error (e.g. a
// connection failure) never matches -- isUniqueViolation must not panic
// or false-positive on an error that isn't even from Postgres.
func TestIsUniqueViolation_NotAPgError(t *testing.T) {
	t.Parallel()
	assert.False(t, isUniqueViolation(errors.New("connection reset"), "verdicts_round_id_reviewer_key"))
}
