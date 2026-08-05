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
-- CORRECTNESS OF THAT GUARD DOES NOT COME FROM THIS STATEMENT. It comes
-- from ingest_jobs_one_running_per_repo (migration
-- 0008_ingest_jobs_running_guard), a partial UNIQUE INDEX on (repo_id)
-- WHERE status = 'running'. A prior version of this query tried to
-- establish the guard entirely within one statement, by locking the
-- candidate's repos row (FOR UPDATE OF rp SKIP LOCKED) so a second
-- concurrent claim against the same repo would contend for that lock --
-- and this bead's own concurrency integration tests, run at -count=200+,
-- proved that construction wrong: two concurrent claims against the SAME
-- repo's two DIFFERENT queued jobs can each compute their candidate from
-- a read-committed statement snapshot that has not yet observed the
-- other's not-yet-committed claim. Once each has picked a DIFFERENT row,
-- no recheck on the row actually being written (the AND status =
-- 'queued' below) can catch it -- that recheck only defends a collision
-- on the SAME row, and this failure mode is across two different rows of
-- the same repo. No single SQL statement running under READ COMMITTED
-- isolation can close that window by itself; only a constraint Postgres
-- enforces against the committed table state, independent of any
-- transaction's snapshot, can. See the migration's own comment for the
-- full argument, and bd remember guard-design-ask-dont-model-2026-08 for
-- why a guard that EXECUTES a rule (a constraint) cannot drift the way
-- one that RESTATES it (an application-level check) can.
--
-- Given that, what THIS statement does is reduce how often two claims
-- collide and have to retry, not guarantee they never do:
--   - NOT EXISTS(... running ...) skips repos this transaction's own
--     snapshot already believes are busy -- a best-effort filter, since
--     that snapshot can be stale, not the source of correctness.
--   - FOR UPDATE OF j SKIP LOCKED locks the candidate JOB row itself
--     (not a repos row -- there is no cross-row entity left to lock once
--     the constraint carries the invariant), so two claims racing for
--     the IDENTICAL row never both proceed to the write; the loser skips
--     to the next candidate instead.
--   - AND ingest_jobs.status = 'queued' on the outer UPDATE is now
--     redundant most of the time (this transaction already holds j's row
--     lock from the CTE by the time it writes), but is kept as the same
--     defense-in-depth every other guarded UPDATE in this codebase uses
--     (CompleteIngestJob, FailIngestJob, RequeueIngestJob below;
--     work_branches.sql's own transitions).
--
-- A write this statement attempts can still lose to
-- ingest_jobs_one_running_per_repo -- e.g. this transaction's NOT EXISTS
-- snapshot missed a just-committed running job for the same repo.
-- internal/ingestjobs.Store.Claim catches that unique_violation and
-- retries the whole statement, which is an ordinary, expected outcome
-- ("someone beat me"), not a failure: a fresh attempt gets a fresh
-- snapshot that does see the collision and picks a different candidate
-- (or correctly finds none).
--
-- Returns zero rows (pgx.ErrNoRows) when nothing is claimable right now --
-- every repo either has no queued job or already has one running --
-- distinct from a real failure; internal/ingestjobs maps this to its own
-- errNoJobAvailable sentinel.
--
-- ORDER BY j.queued_at, j.id: the id tiebreak matches ListIngestJobs'
-- own (queued_at, id) convention below, so two rows that land on the
-- same queued_at (the column's stored precision is microseconds; a
-- forced tie is achievable, though back-to-back Enqueue calls in
-- practice do not produce one -- see TestClaim_QueuedAtTie_BreaksOnID in
-- store_integration_test.go) still sort deterministically rather than in
-- an order Postgres is free to pick arbitrarily.
WITH candidate AS (
    SELECT j.id
    FROM ingest_jobs j
    WHERE j.status = 'queued'
      AND NOT EXISTS (
          SELECT 1 FROM ingest_jobs running
          WHERE running.repo_id = j.repo_id AND running.status = 'running'
      )
    ORDER BY j.queued_at, j.id
    FOR UPDATE OF j SKIP LOCKED
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
