-- name: ListChunkRejections :many
-- Every ledgered path for one repo+branch, in path order (loam-qj21). Read
-- BEFORE the swap transaction opens, because its result is an input to the
-- plan: the pending paths are unioned into diffplan.Plan.ReparseFiles so a
-- rejected file is retried even though it did not change and therefore
-- cannot appear in `git diff ingested_ref..tip`.
--
-- It returns exhausted rows too. They are excluded from the retry union by
-- the caller, not by this query, because the same read is what an operator
-- surface reports on and dropping them here would hide precisely the paths
-- that need attention most.
SELECT * FROM chunk_rejections
WHERE repo_id = $1 AND target_branch = $2
ORDER BY file;

-- name: RecordChunkRejection :exec
-- Upserts one path's rejection, run INSIDE the swap transaction so the
-- ledger commits with (or rolls back with) the index it describes and can
-- never disagree with what actually landed.
--
-- The attempt count and the retry decision are computed in ONE statement
-- rather than read-modify-written in Go. Serialization per repo
-- (0008_ingest_jobs_running_guard's partial unique index) already means no
-- two ingests race here, so this is not a concurrency fix; it is that
-- `attempts + 1` and the state derived from it must not be able to
-- disagree, which they can if two statements compute them separately.
--
-- $4 is max_attempts, the ceiling past which a path stops being retried
-- automatically. It is a PARAMETER rather than a literal so the bound
-- lives with the code that has to explain it
-- (internal/chunkstore.MaxRejectionAttempts) instead of in a migration
-- nobody reads when tuning it. The CASE is applied on the insert path as
-- well as the update path so a ceiling of 1 exhausts immediately rather
-- than off-by-one into a second attempt.
--
-- first_rejected_at is deliberately NOT touched on conflict: it dates the
-- onset of the problem, which is what tells an operator whether they are
-- looking at something that started this morning or has been rotting for a
-- month. Everything else is overwritten, because a fresh rejection is a
-- better description of the current state than the one it replaces --
-- including chunks_state, which can legitimately change from 'stale' to
-- 'absent' when a full rebuild drops the prior chunks the earlier
-- rejection had left intact.
INSERT INTO chunk_rejections (
    repo_id, target_branch, file,
    attempts, state, chunks_state, sqlstate, error, job_id, rejected_ref,
    first_rejected_at, last_rejected_at
)
VALUES (
    $1, $2, $3,
    1,
    CASE WHEN 1 >= $4::integer THEN 'exhausted' ELSE 'pending' END,
    $5, $6, $7, $8, $9,
    now(), now()
)
ON CONFLICT (repo_id, target_branch, file) DO UPDATE
SET attempts = chunk_rejections.attempts + 1,
    state = CASE WHEN chunk_rejections.attempts + 1 >= $4::integer THEN 'exhausted' ELSE 'pending' END,
    chunks_state = excluded.chunks_state,
    sqlstate = excluded.sqlstate,
    error = excluded.error,
    job_id = excluded.job_id,
    rejected_ref = excluded.rejected_ref,
    last_rejected_at = now();

-- name: DeleteChunkRejections :exec
-- Clears the ledger for named paths, run inside the swap transaction for
-- the same reason RecordChunkRejection is. A path is cleared when this
-- ingest wrote its chunks successfully, and when the plan DROPPED it (the
-- file was deleted or renamed away, so there is nothing missing any more).
--
-- Deleting rather than marking resolved is deliberate: "the ledger is
-- empty" is the whole health signal, and a resolved row would make
-- emptiness a query over a status column instead of a row count -- one
-- more place for the signal and the reality to drift apart.
DELETE FROM chunk_rejections
WHERE repo_id = $1 AND target_branch = $2 AND file = ANY($3::text[]);

-- name: DeleteChunkRejectionsForRepoBranch :exec
-- Clears the whole ledger for a repo+branch, run by the full-rebuild path
-- alongside its repo-scoped drop of every derived row. A KindFull ingest
-- re-reads and re-chunks EVERY file in the tree, so every ledger row it
-- inherits is about to be re-decided; keeping them would leave rows for
-- paths the new tree no longer contains at all, which no per-file clear
-- keyed on the new tree could ever name -- the same argument that makes
-- chunks' own full-rebuild drop repo-scoped (chunkstore.DropRepoBranch).
--
-- Anything that rejects again during that rebuild is re-recorded in the
-- same transaction, so this never empties the ledger for a path that is
-- still broken. It does RESET that path's attempt count, and that is
-- correct rather than a leak: the ceiling exists to stop a hopeless file
-- from adding work to INCREMENTAL ingests, and a full rebuild re-chunks
-- every file whether or not the ledger asked it to, so retrying under
-- KindFull costs nothing the rebuild was not already paying.
DELETE FROM chunk_rejections
WHERE repo_id = $1 AND target_branch = $2;
