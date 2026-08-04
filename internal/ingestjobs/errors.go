package ingestjobs

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// errNotFound is returned when an id passed to Get does not name an
// existing ingest_jobs row. Unexported: nothing outside this package
// consumes it yet (nothing in this tree calls this package at all -- see
// the package doc comment), and the house rule is to export a sentinel
// only once a real caller needs errors.Is across a package boundary.
var errNotFound = errors.New("ingestjobs: not found")

// errIllegalTransition is returned by Complete, Fail, and Requeue when
// their guarded UPDATE matches zero rows because the job's CURRENT status
// disqualifies the requested move (e.g. completing a job that is not
// running, or requeuing one that is not failed) -- distinguished from
// errNotFound so a caller can tell "no such job" apart from "that job
// exists but is not in the right state", the same split
// internal/workbranchstore.ErrIllegalTransition draws from ErrNotFound.
// The guard and the write are one atomic SQL statement
// (internal/db/queries/ingest_jobs.sql), so this is never a race: the row
// was actually read and rejected by Postgres in the same statement that
// would have written it.
var errIllegalTransition = errors.New("ingestjobs: illegal status transition")

// errNoJobAvailable is returned by Claim when no ingest_jobs row is
// claimable right now -- every repo either has no queued job or already
// has one running. This is Claim's ordinary "nothing to do" outcome, not a
// failure: a worker polling this store is expected to see it constantly
// between jobs.
var errNoJobAvailable = errors.New("ingestjobs: no job available to claim")

// errUniqueViolation is the Postgres SQLSTATE for a unique-constraint hit,
// matching internal/workbranchstore and internal/rolestore's identical
// constants.
const errUniqueViolation = "23505"

// runningPerRepoConstraint is the partial unique index on (repo_id) WHERE
// status = 'running' (migration 0008_ingest_jobs_running_guard) that is
// the actual, database-enforced source of the "at most one running job
// per repo" guarantee -- see ClaimIngestJob's doc comment
// (internal/db/queries/ingest_jobs.sql) for why the claim statement
// itself cannot establish that guarantee alone under READ COMMITTED.
const runningPerRepoConstraint = "ingest_jobs_one_running_per_repo"

// isRunningPerRepoViolation reports whether err is the unique violation
// runningPerRepoConstraint raises: this transaction's own claim attempt
// picked a job whose repo, per the fresh committed state Postgres checked
// at write time (not this transaction's possibly-stale read snapshot),
// already has a running job. Store.Claim treats this as an ordinary
// "someone beat me to it" outcome and retries with a fresh statement,
// never as a failure to report.
func isRunningPerRepoViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == errUniqueViolation && pgErr.ConstraintName == runningPerRepoConstraint
}
