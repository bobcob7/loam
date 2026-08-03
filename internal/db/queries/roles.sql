-- name: GetRoleByName :one
-- The single indexed lookup (roles_name_key UNIQUE (name), 0001_init.up.sql)
-- every caller resolving a trusted Loam-Agent-Role header value uses.
SELECT * FROM roles WHERE name = $1;

-- name: ListRoleOperations :many
-- The operations granted to a role (docs/persistence-spec.md
-- "role_operations"), ordered for a stable, deterministic result --
-- role_operations has no natural ordering column of its own.
SELECT operation FROM role_operations WHERE role_id = $1 ORDER BY operation;

-- name: ListRoles :many
-- Every role, built-in and admin-defined, ordered by name for a stable
-- list (loam.admin.v1.RoleService.ListRoles). Unpaginated on purpose: the
-- proto's ListRolesRequest carries no Page (unlike ListRepos), because the
-- role set is operator-authored configuration a handful of rows deep, not
-- unbounded user data.
SELECT * FROM roles ORDER BY name;

-- name: ListAllRoleOperations :many
-- Every (role_id, operation) pair in one round trip, so ListRoles can
-- attach operations to every role without a per-role ListRoleOperations
-- query. Ordered by role then operation so the caller can group by role_id
-- and still get each role's operations in the same deterministic order
-- ListRoleOperations gives for a single role.
SELECT role_id, operation FROM role_operations ORDER BY role_id, operation;

-- name: CreateRole :one
-- Creates an admin-defined role. builtin is NOT a parameter and is left to
-- the column default (false): only migration 0001_init seeds a builtin
-- role, and there is deliberately no statement in this file through which
-- an RPC could mint one.
INSERT INTO roles (id, name, instructions)
VALUES ($1, $2, $3)
RETURNING *;

-- name: UpdateRoleInstructions :one
-- Rewrites a role's instruction text (the body
-- loam.v1.MetaService.GetInstructions returns) by name, the natural key
-- every RoleService RPC carries. Applies to built-in roles too: a built-in
-- originally shipped with instructions set to the empty string
-- (0001_init.up.sql), so refusing this here would have left the
-- author/reviewer instruction text permanently empty and made
-- features/roles.feature's "A role's instructions reach its agents"
-- unimplementable for the very roles it names. Migration
-- 0006_role_instructions_seed (loam-0pj.17) now fills that empty default
-- with shipped policy text, but ONLY where instructions is still the
-- empty string (that migration's own coalesce-empty guard) -- it
-- deliberately will not touch a built-in that already carries
-- operator-written text. That makes
-- this statement, run through RoleService's UpdateRole, the ONLY route by
-- which already-non-empty built-in text (e.g. a stale or filler value an
-- admin typed before 0006 landed) can ever be replaced; a rerun of 0006 or
-- any future migration guarded the same way will not touch it either.
-- name itself is not updatable -- RoleService has no rename RPC and
-- UpdateRoleRequest carries one name, which identifies the role rather
-- than renaming it.
UPDATE roles
SET instructions = $2, updated_at = now()
WHERE name = $1
RETURNING *;

-- name: DeleteRoleOperations :exec
-- Clears a role's granted operations, the first half of the
-- delete-then-insert UpdateRole performs inside one transaction. Never run
-- outside that transaction: on its own it strips a role to zero
-- capabilities.
DELETE FROM role_operations WHERE role_id = $1;

-- name: InsertRoleOperation :exec
-- Grants one operation to a role. The value is CHECK-constrained by
-- role_operations_operation_check (0001_init.up.sql) to the fixed
-- vocabulary, so an operation outside it is refused by the database even
-- if a caller skipped internal/handler's own validation -- and PRIMARY KEY
-- (role_id, operation) refuses a duplicate grant.
INSERT INTO role_operations (role_id, operation) VALUES ($1, $2);

-- name: DeleteRole :execrows
-- Unenrolls an admin-defined role by name, cascading to its
-- role_operations rows (role_operations.role_id REFERENCES roles (id) ON
-- DELETE CASCADE, 0001_init.up.sql).
--
-- `AND NOT builtin` is defence in depth, not the primary gate: the handler
-- reads the role first and refuses a built-in with failed_precondition, so
-- that the caller learns WHY (a bare zero-rows result here cannot tell
-- "built-in" from "no such role" apart). This predicate exists so that a
-- regression above it cannot delete author or reviewer -- the worst it can
-- do is report the wrong reason.
DELETE FROM roles WHERE name = $1 AND NOT builtin;
