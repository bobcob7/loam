-- name: CreateRepo :one
INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetRepoByID :one
SELECT * FROM repos WHERE id = $1;

-- name: UpdateRepoSyncState :one
-- Exercises sqlc's handling of a text+CHECK enum-like column
-- (repos.sync_state): it must surface as a plain Go string, per
-- docs/persistence-spec.md "Conventions" and sqlc.yaml's overrides comment.
UPDATE repos
SET sync_state = $2, last_synced_at = $3, sync_error = $4, updated_at = now()
WHERE id = $1
RETURNING *;
