package reviewstore

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// errUniqueViolation is the Postgres SQLSTATE for a unique-constraint hit.
const errUniqueViolation = "23505"

// isUniqueViolation reports whether err is a Postgres unique_violation
// against the named constraint, so a caller can map one specific UNIQUE
// constraint hit to a distinguishable sentinel instead of leaking
// pgconn's raw error text or conflating it with an unrelated failure.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == errUniqueViolation && pgErr.ConstraintName == constraint
}
