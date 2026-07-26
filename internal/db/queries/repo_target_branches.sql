-- name: AddTargetBranch :one
-- Idempotent enrollment of a branch as a work-branch target
-- (docs/persistence-spec.md "repo_target_branches"). ON CONFLICT DO UPDATE
-- with a self-assignment (rather than DO NOTHING) so RETURNING always
-- yields the current row, whether this call created it or the branch was
-- already a target -- it never touches ingested_ref/ingested_at/
-- ingested_versions, so re-adding an already-ingested branch can never
-- reset its diff base back to NULL.
INSERT INTO repo_target_branches (repo_id, branch)
VALUES ($1, $2)
ON CONFLICT (repo_id, branch) DO UPDATE SET branch = EXCLUDED.branch
RETURNING *;

-- name: ListTargetBranches :many
SELECT * FROM repo_target_branches WHERE repo_id = $1 ORDER BY branch;

-- name: GetTargetBranch :one
SELECT * FROM repo_target_branches WHERE repo_id = $1 AND branch = $2;

-- name: RemoveTargetBranch :execrows
-- :execrows (rather than :exec) so the store layer can tell a no-op
-- delete (branch was never a target) apart from an actual removal and map
-- the former to errNotFound, instead of silently succeeding either way.
DELETE FROM repo_target_branches WHERE repo_id = $1 AND branch = $2;

-- name: AdvanceIngestedRef :one
-- Advances the incremental-ingest diff base after a successful ingest
-- (docs/persistence-spec.md "repo_target_branches"; loam-c94.2's design).
-- ref is a required, non-null parameter -- this query has no path that
-- writes ingested_ref back to NULL, so the only way a branch's diff base
-- is ever NULL is that it has never been through here (first enrollment,
-- before any ingest has completed).
UPDATE repo_target_branches
SET ingested_ref = $3, ingested_at = $4, ingested_versions = $5
WHERE repo_id = $1 AND branch = $2
RETURNING *;
