-- Ingest jobs queries (loam-54o.13): the ingest_jobs worker queue --
-- enqueue, claim (guarded to enforce at most one running job per repo),
-- advance to a terminal status (succeeded/failed), requeue a failed job
-- for a retry, get, and list by repo/status (docs/persistence-spec.md
-- "ingest_jobs"; docs/ingestion-spec.md "Trigger & Scheduling"). IDs are
-- generated in Go (uuid.NewV7), never in SQL, so EnqueueIngestJob takes id
-- as a bound parameter, matching every other store in this codebase.
--
-- kind and status are text + CHECK (ingest_jobs_kind_check,
-- ingest_jobs_status_check, 0001_init.up.sql): the fixed vocabulary lives
-- in the constraint, not restated as a second list here or trusted to
-- match one in Go without proof -- internal/ingestjobs pins its Kind/
-- Status constants against the real constraint with an integration test
-- (store_integration_test.go) that inserts each Go-known value (must
-- succeed) and one deliberately unknown value (must be rejected by name,
-- matching the constraint), rather than a Go-side allowlist that could
-- silently drift from the SQL.

-- name: EnqueueIngestJob :one
-- status/attempts take their column defaults ('queued', 0); queued_at is
-- stamped now(), never caller-supplied, so ordering by it always reflects
-- server receipt time.
INSERT INTO ingest_jobs (id, repo_id, target_branch, kind, status, attempts, queued_at)
VALUES ($1, $2, $3, $4, 'queued', 0, now())
RETURNING *;

-- name: GetIngestJob :one
SELECT * FROM ingest_jobs WHERE id = $1;

-- name: ClaimIngestJob :one
-- Claims the oldest queued job belonging to a repo with NO currently
-- running job -- the "at most one running job per repo" guard the design
-- calls for -- and, in the SAME statement, flips it to running.
--
-- The guard cannot be enforced by locking the candidate ingest_jobs row
-- alone (FOR UPDATE OF j): two DIFFERENT queued rows for the SAME repo are
-- two different rows, so locking one does not block a concurrent claim
-- from locking the other. What must be serialized is "is there already a
-- running job for THIS repo", a fact that, for a repo with no running job
-- yet, has no row of its own to lock. This statement instead locks the
-- CANDIDATE'S repos ROW (FOR UPDATE OF rp SKIP LOCKED) -- every queued job
-- for the same repo shares that one row, so a second concurrent claim
-- attempting a job for the same repo contends for the identical lock and,
-- under SKIP LOCKED, is excluded from the candidate set for THIS attempt
-- rather than blocking, and falls through to a different repo's job (or to
-- no candidate at all) instead of double-claiming. Locking is scoped to
-- this one statement (no explicit BEGIN here), so ordinary reads and
-- writes against repos elsewhere are never blocked by it.
--
-- Returns zero rows (pgx.ErrNoRows) when nothing is claimable right now --
-- every repo either has no queued job or already has one running --
-- distinct from a real failure; internal/ingestjobs maps this to its own
-- errNoJobAvailable sentinel.
--
-- ORDER BY j.queued_at, j.id: the id tiebreak matches ListIngestJobs'
-- own (queued_at, id) convention below. Without it, two jobs enqueued
-- close enough together to land on the same timestamptz value (the
-- column's actual stored precision is microseconds, but two Enqueue
-- calls back-to-back with no intervening work can still tie) sort in an
-- UNSPECIFIED order -- proven by this bead's own test suite, which
-- caught it directly: a fixture enqueuing two same-repo jobs with no
-- delay between them claimed the wrong one first often enough to fail.
--
-- AND ingest_jobs.status = 'queued' on the OUTER UPDATE (not just inside
-- the candidate CTE) closes a real double-claim race the concurrency
-- integration tests caught: FOR UPDATE OF rp SKIP LOCKED only locks the
-- CANDIDATE'S repos row, never the ingest_jobs row itself, so the
-- candidate CTE's own snapshot of j.status can still be pre-claim-stale
-- by the time the outer UPDATE runs (e.g. two claims for DIFFERENT repos
-- racing: one blocks briefly acquiring a different repo's rp lock, and by
-- the time it proceeds, the row it picked as "the globally oldest queued
-- job" across BOTH repos has already been claimed by the other). Without
-- this repeated check, that UPDATE would silently re-run against an
-- already-running row (RETURNING it a second time -- observed directly as
-- two concurrent Claim calls for two different repos both getting the
-- SAME job back) instead of matching zero rows the way every other
-- guarded UPDATE in this codebase is written to (see CompleteIngestJob,
-- FailIngestJob, RequeueIngestJob below, and work_branches.sql's own
-- transitions): the precondition and the write must be re-checked
-- together, atomically, at the statement that actually writes.
WITH candidate AS (
    SELECT j.id
    FROM ingest_jobs j
    JOIN repos rp ON rp.id = j.repo_id
    WHERE j.status = 'queued'
      AND NOT EXISTS (
          SELECT 1 FROM ingest_jobs running
          WHERE running.repo_id = j.repo_id AND running.status = 'running'
      )
    ORDER BY j.queued_at, j.id
    FOR UPDATE OF rp SKIP LOCKED
    LIMIT 1
)
UPDATE ingest_jobs
SET status = 'running', started_at = now()
FROM candidate
WHERE ingest_jobs.id = candidate.id AND ingest_jobs.status = 'queued'
RETURNING ingest_jobs.*;

-- name: CompleteIngestJob :one
-- Records a successful ingest: stats persisted as jsonb, finished_at set.
-- Guarded to status = 'running' -- like every guarded transition elsewhere
-- in this codebase (see internal/db/queries/work_branches.sql), the
-- precondition and the write are one atomic statement, so completing a job
-- that is not actually running (already terminal, or someone else's stale
-- reference) returns zero rows rather than silently overwriting a
-- terminal outcome.
UPDATE ingest_jobs
SET status = 'succeeded', stats = $2, finished_at = now()
WHERE id = $1 AND status = 'running'
RETURNING *;

-- name: FailIngestJob :one
-- Records a failed ingest: the error, attempts incremented (the "attempts
-- incremented on retry" acceptance criterion -- attempts counts completed
-- failed attempts, incremented here rather than at claim time, so a job
-- that succeeds on its first claim never touches attempts at all), and
-- finished_at set. Guarded to status = 'running', matching
-- CompleteIngestJob above.
UPDATE ingest_jobs
SET status = 'failed', error = $2, attempts = attempts + 1, finished_at = now()
WHERE id = $1 AND status = 'running'
RETURNING *;

-- name: RequeueIngestJob :one
-- Returns a failed job to queued for another claim attempt -- the backward
-- edge of the queued -> running -> succeeded|failed lifecycle a retry
-- needs, distinct from FailIngestJob (which records the failed outcome and
-- its attempts count) and deliberately never touching attempts itself.
-- queued_at is re-stamped to now() so the job re-enters ClaimIngestJob's
-- queued_at ordering as of the retry, not its original enqueue time.
-- Guarded to status = 'failed': requeuing a job that is not actually
-- failed (already running, already requeued, or terminal-succeeded)
-- returns zero rows instead of a silent no-op.
UPDATE ingest_jobs
SET status = 'queued', queued_at = now()
WHERE id = $1 AND status = 'failed'
RETURNING *;

-- name: ListIngestJobs :many
-- Offset pagination (docs/persistence-spec.md "Conventions"), paired with
-- CountIngestJobs for PageInfo.total -- the store-layer backing for
-- RepoAdminService.ListIngestJobs's repo/status filters
-- (docs/web-spec.md). repo_id ($1) is NULL for "no filter", never a real
-- filter value of the zero UUID; status ($2) empty means "no filter on
-- status" (matching every other optional string filter in this codebase --
-- e.g. ListWorkBranches -- since status is never legitimately empty).
-- Newest-queued first, matching ListWorkBranches' newest-created-first
-- convention for the same kind of bounded admin screen.
SELECT * FROM ingest_jobs
WHERE ($1::uuid IS NULL OR repo_id = $1)
  AND ($2::text = '' OR status = $2)
ORDER BY queued_at DESC, id
LIMIT $3 OFFSET $4;

-- name: CountIngestJobs :one
-- Same filter predicate as ListIngestJobs, minus LIMIT/OFFSET, for
-- PageInfo.total.
SELECT COUNT(*) FROM ingest_jobs
WHERE ($1::uuid IS NULL OR repo_id = $1)
  AND ($2::text = '' OR status = $2);
