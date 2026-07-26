-- name: GetRoleByName :one
-- The single indexed lookup (roles_name_key UNIQUE (name), 0001_init.up.sql)
-- every caller resolving a trusted Loam-Agent-Role header value uses.
SELECT * FROM roles WHERE name = $1;

-- name: ListRoleOperations :many
-- The operations granted to a role (docs/persistence-spec.md
-- "role_operations"), ordered for a stable, deterministic result --
-- role_operations has no natural ordering column of its own.
SELECT operation FROM role_operations WHERE role_id = $1 ORDER BY operation;
