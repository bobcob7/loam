package ingestjobs

import "errors"

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
