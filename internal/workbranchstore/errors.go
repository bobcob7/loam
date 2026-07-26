package workbranchstore

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// errNotFound is returned when an id passed to Get, GetByName, or any
// transition method does not name an existing work_branches row --
// distinguished from errIllegalTransition (the id exists, but its current
// state disqualifies the requested move) so a caller can tell the two
// apart with errors.Is instead of guessing from a bare pgx.ErrNoRows.
var errNotFound = errors.New("work branch not found")

// errIllegalTransition is returned when a transition method's guarded
// UPDATE matches zero rows because the work branch's CURRENT state (or
// conflict value) does not permit the requested move -- e.g. calling
// UpdateState to jump straight from draft to reviewed, or FlagConflict on
// a branch that is not draft/conflict-free. The guard and the write are
// one atomic SQL statement (internal/db/queries/work_branches.sql), so
// this is never a race: the row was actually read and rejected by
// Postgres in the same statement that would have written it.
var errIllegalTransition = errors.New("illegal work branch state transition")

// errDuplicateName is returned when Create hits
// work_branches_repo_id_name_key (UNIQUE(repo_id, name),
// docs/persistence-spec.md "work_branches") -- identity is (repo, name),
// so a caller can tell "this name is already taken in this repo" apart
// from any other insert failure.
var errDuplicateName = errors.New("work branch name already exists for this repo")

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
