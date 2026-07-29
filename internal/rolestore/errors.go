package rolestore

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrNotFound is returned when no role by the requested name exists.
	//
	// It was unexported while GetRole was this package's only method and
	// its only callers (MetaService, RepoService's capability gate) treated
	// an unrecognized Loam-Agent-Role header as an operator/configuration
	// anomaly they let fall through to ErrorMapper's unmapped-error branch
	// (CodeInternal, logged) rather than misreport as a normal not-found.
	// loam-ofg.13's RoleService is the "future caller [that] genuinely
	// needs to distinguish it" that comment anticipated: GetRole(name),
	// UpdateRole(name), and DeleteRole(name) on a name that does not exist
	// are ordinary CodeNotFound answers to an admin who typed a name, not
	// internal errors, and internal/handler/role cannot tell them apart
	// from a database failure without matching this sentinel.
	ErrNotFound = errors.New("rolestore: not found")
	// ErrAlreadyExists is returned when CreateRole names a role that
	// already exists (roles_name_key, 0001_init.up.sql). Exported for the
	// same reason as ErrNotFound: it is how internal/handler/role answers
	// CodeAlreadyExists rather than collapsing a name collision -- an
	// ordinary, correctable admin mistake -- into CodeInternal.
	ErrAlreadyExists = errors.New("rolestore: already exists")
)

// errUniqueViolation is the Postgres SQLSTATE for a unique-constraint hit.
const errUniqueViolation = "23505"

// rolesNameConstraint is the UNIQUE constraint on roles.name
// (0001_init.up.sql). Matching the constraint BY NAME, rather than on the
// SQLSTATE alone, is what keeps ErrAlreadyExists meaning "that role name is
// taken": role_operations' own PRIMARY KEY (role_id, operation) raises the
// same 23505, and a duplicate operation within one request is a different
// fault with a different answer (it must not be reported to an admin as
// "role already exists").
const rolesNameConstraint = "roles_name_key"

// isRoleNameCollision reports whether err is the unique violation raised by
// inserting a second role with an existing name.
func isRoleNameCollision(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == errUniqueViolation && pgErr.ConstraintName == rolesNameConstraint
}
