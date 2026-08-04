package workbranchstore

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound is returned when an id passed to Get, GetByName, or any
// transition method does not name an existing work_branches row --
// distinguished from ErrIllegalTransition (the id exists, but its current
// state disqualifies the requested move) so a caller can tell the two
// apart with errors.Is instead of guessing from a bare pgx.ErrNoRows.
var ErrNotFound = errors.New("work branch not found")

// ErrIllegalTransition is returned when a transition method's guarded
// UPDATE matches zero rows because the work branch's CURRENT state (or
// conflict value) does not permit the requested move -- e.g. calling
// UpdateState to jump straight from draft to reviewed, or MarkConflicted
// on a branch that is already complete/closed. The guard and the write
// are one atomic SQL statement (internal/db/queries/work_branches.sql),
// so this is never a race: the row was actually read and rejected by
// Postgres in the same statement that would have written it.
var ErrIllegalTransition = errors.New("illegal work branch state transition")

// ErrPRAlreadyRecorded is returned by RecordUpstreamPR when the row
// already carries an upstream_pr_number, so the guarded UPDATE
// (RecordWorkBranchUpstreamPR, internal/db/queries/work_branches.sql)
// matched zero rows without the row itself being missing. It is
// deliberately NOT folded into ErrIllegalTransition: the two call for
// opposite reactions. An illegal transition is a precondition failure the
// caller got wrong; this one means another actor recorded the PR first,
// which is the exact outcome proposal acceptance is trying to reach, so
// its caller re-reads the row and returns the PR that won rather than
// failing the accept.
//
// Exported because that caller lives in another package
// (internal/mirrorsync's StoreProposalAccepter) and must match it with
// errors.Is.
var ErrPRAlreadyRecorded = errors.New("work branch already has a recorded upstream pull request")

// errInvalidUpstreamPR is returned by RecordUpstreamPR for a PR number,
// URL, or accepted tip that cannot identify a real pull request or a real
// commit (a non-positive number, an empty URL, an empty tip), and by
// RecordAcceptedTip for an empty tip. It is a rejection at the store, one
// layer below the accept engine's own validation of what the forge
// answered, because this column pair is the sole input to
// internal/mirrorsync's PR poller: a recorded PR #0 would put a work
// branch permanently into a poll set whose every GetPRState call must
// fail, and -- worse -- would consume the row's one-shot idempotency
// guard, so the accept that should have recorded the real PR could never
// write it afterwards. An empty accepted_tip is the same shape of mistake
// for loam-cgg's ListProposals comparison: it would either compare against
// nothing or, if ever read back as "no tip recorded", flip a just-accepted
// row's over-inclusion into a false "up to date".
var errInvalidUpstreamPR = errors.New("upstream pull request identity is not usable")

// errInvalidUpstreamDrift is returned by SetUpstreamDrift for a value
// outside the two the work_branches_upstream_drift_check CHECK constraint
// allows. It is rejected in Go, ahead of the statement, rather than left to
// surface as a constraint violation: the CHECK is the durable backstop, but
// a caller reaching it has a code bug (there are exactly two legal values
// and both are named constants in this package), and a wrapped SQLSTATE
// 23514 from pgconn is a materially worse thing to read in a sync cycle's
// error log than a sentence naming the value that was passed.
var errInvalidUpstreamDrift = errors.New("upstream drift value is not one of none/diverged")

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
