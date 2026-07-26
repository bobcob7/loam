-- name: CreateRepo :one
INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetRepoByID :one
SELECT * FROM repos WHERE id = $1;

-- name: GetRepoByName :one
-- The cheap, indexed name->id resolution path (repos_name_key UNIQUE
-- (name), 0001_init.up.sql): callers hold repos.name as the natural key
-- (docs/persistence-spec.md "repos"; loam-54o.7 NOTES on the settled RepoID
-- decision) and resolve to the FK id other tables reference through this
-- single indexed lookup.
SELECT * FROM repos WHERE name = $1;

-- name: ListRepos :many
-- Offset pagination (docs/persistence-spec.md "Conventions"): paired with
-- CountRepos for PageInfo.total. Ordered by name for a stable page
-- boundary across calls.
SELECT * FROM repos ORDER BY name LIMIT $1 OFFSET $2;

-- name: CountRepos :one
SELECT count(*) FROM repos;

-- name: UpdateRepo :one
-- Updates the enrollment-config fields an admin can change post-enroll
-- (RepoAdminService.SetTargetBranches's indexed_branch change; a future
-- re-point of upstream_url/forge_host). name is deliberately not
-- updatable here: loam-54o.7 NOTES settled that there is no rename path
-- anywhere in the proto surface, so repos.name is immutable once created.
UPDATE repos
SET upstream_url = $2, forge_host = $3, indexed_branch = $4, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateRepoSyncState :one
-- Exercises sqlc's handling of a text+CHECK enum-like column
-- (repos.sync_state): it must surface as a plain Go string, per
-- docs/persistence-spec.md "Conventions" and sqlc.yaml's overrides comment.
UPDATE repos
SET sync_state = $2, last_synced_at = $3, sync_error = $4, updated_at = now()
WHERE id = $1
RETURNING *;
