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

-- name: ListRepoNames :many
-- Unpaginated enumeration of every enrolled repo's name, ordered by name.
-- Backs mirrorsync.RepoLister (loam-13z): the scheduler re-lists every
-- enrolled repo on every tick, so it needs the full enrollment, not one
-- page of full Repo rows plus a total count -- LIMIT/OFFSET pagination
-- (ListRepos, above) is the admin API's list view's primitive, the only
-- caller rendering a bounded page for a human.
SELECT name FROM repos ORDER BY name;

-- name: DeleteRepo :one
-- Unenrollment (loam-cwb; RepoAdminService.RemoveRepo, docs/web-spec.md:
-- "drop the mirror, the derived indexes, and the repo's metadata (work
-- branches, rounds, verdicts, threads -- unenrollment removes history;
-- re-enrolling starts fresh). Queued/running ingest jobs are deleted.").
--
-- This ONE statement is the whole database half of that contract: every
-- table that holds repo-scoped data reaches repos.id through a chain of
-- ON DELETE CASCADE foreign keys, so Postgres removes all of them in this
-- statement's own transaction, atomically. Directly:
-- repo_target_branches, work_branches, ingest_jobs (0001_init.up.sql);
-- symbols, symbol_references, graph_edges, chunks (0002_code_intel.up.sql,
-- chunks carrying the pgvector embeddings). Transitively: review_rounds,
-- threads, comments (via work_branches), verdicts (via review_rounds), and
-- symbol_history (via symbols). Nothing enumerates those tables in Go --
-- adding a new repo-scoped table with the same FK gets swept up here for
-- free, and one added WITHOUT it fails this DELETE loudly on a foreign-key
-- violation rather than silently orphaning rows.
--
-- credentials is deliberately NOT in that set: it is keyed by forge HOST
-- (credentials_host_key UNIQUE (host)) and shared by every repo on that
-- host, so it has no repos FK and unenrolling one repo must not revoke the
-- token its siblings still use.
--
-- RETURNING * rather than :execrows: the caller needs the deleted row's
-- name to derive the bare mirror's on-disk path (internal/mirrorpath.Dir)
-- AFTER the row is gone, and reading it back out of the same statement
-- avoids both a second round trip and the window between a separate SELECT
-- and this DELETE. A no-rows result is the store's ErrNotFound signal.
DELETE FROM repos WHERE id = $1 RETURNING *;

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
