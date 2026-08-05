package rolestore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bobcob7/loam/internal/db/gen"
)

// Role is a roles row plus its granted operations from role_operations
// (docs/persistence-spec.md "roles", "role_operations"). Operations holds
// the raw operation strings role_operations.operation stores -- each one
// CHECK-constrained (role_operations_operation_check, 0001_init.up.sql) to
// the fixed capability vocabulary -- not internal/handler.Capability: this
// package has no dependency on internal/handler (a store package importing
// an RPC-boundary package would invert the layering that package's own
// "interfaces defined at the consumer" convention establishes), so the
// conversion to handler.Capability is the consumer's job, at the point
// that consumer wires a Store into its own RoleStore interface.
type Role struct {
	ID           uuid.UUID
	Name         string
	Instructions string
	Builtin      bool
	Operations   []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RoleParams is the writable half of a Role: everything CreateRole and
// UpdateRole accept. It deliberately omits ID (assigned here), Builtin
// (assigned only by migration 0001_init -- see CreateRole), and the
// timestamps (assigned by the database).
type RoleParams struct {
	Name         string
	Instructions string
	// Operations must already be members of the fixed capability
	// vocabulary. This package does NOT validate them -- the vocabulary
	// lives in internal/handler (Capability, AllCapabilities) and importing
	// that RPC-boundary package here would invert the layering, exactly as
	// the Role.Operations doc above records for the read direction.
	// internal/handler/role validates before calling; the CHECK constraint
	// role_operations_operation_check is the backstop underneath, and an
	// operation outside the vocabulary reaching here surfaces as its raw
	// SQLSTATE 23514 -- an unmapped, logged CodeInternal, which is the
	// loud failure a skipped validation deserves.
	Operations []string
}

// Store is the roles + role_operations store. Construct with NewStore,
// passing a *pgxpool.Pool in production (it satisfies querier directly)
// or a querier mock in tests.
type Store struct {
	db     querier
	q      *gen.Queries
	logger *slog.Logger
}

// NewStore builds a Store over db, typically a *pgxpool.Pool.
func NewStore(db querier, logger *slog.Logger) *Store {
	return &Store{db: db, q: gen.New(db), logger: logger}
}

// GetRole resolves name -- the value a trusted Loam-Agent-Role header
// carries, and the key every RoleService RPC takes -- to its full Role,
// including every operation role_operations grants it. Returns a wrapped
// ErrNotFound if name does not exist (roles.name is UNIQUE, roles_name_key,
// 0001_init.up.sql).
func (s *Store) GetRole(ctx context.Context, name string) (Role, error) {
	row, err := s.q.GetRoleByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Role{}, fmt.Errorf("getting role %s: %w", name, ErrNotFound)
		}
		return Role{}, fmt.Errorf("getting role %s: %w", name, err)
	}
	operations, err := s.q.ListRoleOperations(ctx, row.ID)
	if err != nil {
		return Role{}, fmt.Errorf("listing operations for role %s: %w", name, err)
	}
	return roleFrom(row, operations), nil
}

// ListRoles returns every role -- built-in and admin-defined -- ordered by
// name, each with its granted operations attached
// (loam.admin.v1.RoleService.ListRoles). It issues exactly two queries
// regardless of how many roles exist: the roles themselves, and every
// role_operations row in one sweep, grouped here. The obvious alternative
// (GetRole per name) is an N+1 over a table whose whole point is being
// read on every capability check.
func (s *Store) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.q.ListRoles(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing roles: %w", err)
	}
	grants, err := s.q.ListAllRoleOperations(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing role operations: %w", err)
	}
	byRole := make(map[uuid.UUID][]string, len(rows))
	for _, grant := range grants {
		id := uuidFromPG(grant.RoleID)
		byRole[id] = append(byRole[id], grant.Operation)
	}
	roles := make([]Role, 0, len(rows))
	for _, row := range rows {
		roles = append(roles, roleFrom(row, byRole[uuidFromPG(row.ID)]))
	}
	return roles, nil
}

// CreateRole creates an admin-defined role with a fresh UUIDv7 id and its
// granted operations, in ONE transaction: a role that exists with only
// some of the operations the admin asked for is a silently
// under-privileged (or, on a partial rollback of the other ordering,
// over-privileged) authorization record, which is not a state any later
// call can detect as wrong.
//
// The created role is never builtin. The builtin flag marks the roles the
// migrations seed -- author and reviewer from 0001_init, orchestrator from
// 0009_orchestrator_role -- and is the only thing standing between
// DeleteRole and them, so nothing reachable from an RPC may set it -- the
// CreateRole statement does not even accept it as a parameter (see
// queries/roles.sql).
//
// Returns a wrapped ErrAlreadyExists if params.Name is taken.
func (s *Store) CreateRole(ctx context.Context, params RoleParams) (Role, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Role{}, fmt.Errorf("generating role id: %w", err)
	}
	var role Role
	err = s.inTx(ctx, func(q *gen.Queries) error {
		row, err := q.CreateRole(ctx, gen.CreateRoleParams{
			ID:           pgUUID(id),
			Name:         params.Name,
			Instructions: params.Instructions,
		})
		if err != nil {
			if isRoleNameCollision(err) {
				return fmt.Errorf("creating role %s: %w", params.Name, ErrAlreadyExists)
			}
			return fmt.Errorf("creating role %s: %w", params.Name, err)
		}
		operations, err := grantOperations(ctx, q, row.ID, params.Operations)
		if err != nil {
			return fmt.Errorf("granting operations to role %s: %w", params.Name, err)
		}
		role = roleFrom(row, operations)
		return nil
	})
	if err != nil {
		return Role{}, err
	}
	s.logger.InfoContext(ctx, "created role", "role", role.Name, "operations", role.Operations)
	return role, nil
}

// UpdateRole rewrites params.Name's instructions and REPLACES its granted
// operations with params.Operations, in one transaction. Replace, not
// merge: the proto's UpdateRole carries the whole Role, so the operations
// it names are the operations the role is to have -- a merge would make
// revoking one impossible through this surface.
//
// It applies to built-in roles as well as custom ones, deliberately. Only
// DELETION is refused for a built-in (docs/web-spec.md -> RoleService:
// "built-in roles cannot be deleted", and nothing more). Built-ins
// originally shipped with instructions set to the empty string
// (0001_init.up.sql); refusing to update them would have left the author
// and reviewer instruction text permanently empty -- the exact thing
// features/roles.feature's "A role's instructions reach its agents"
// configures on the *reviewer*, a built-in. Migration
// 0006_role_instructions_seed (loam-0pj.17) now fills that empty default
// on a fresh database, but only where it is still empty, so this method is
// still the only way to replace a built-in's already-non-empty text (e.g.
// operator-written filler predating 0006). builtin itself is never
// written by this method.
//
// Returns a wrapped ErrNotFound if params.Name does not exist.
func (s *Store) UpdateRole(ctx context.Context, params RoleParams) (Role, error) {
	var role Role
	err := s.inTx(ctx, func(q *gen.Queries) error {
		row, err := q.UpdateRoleInstructions(ctx, gen.UpdateRoleInstructionsParams{
			Name:         params.Name,
			Instructions: params.Instructions,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("updating role %s: %w", params.Name, ErrNotFound)
			}
			return fmt.Errorf("updating role %s: %w", params.Name, err)
		}
		if err := q.DeleteRoleOperations(ctx, row.ID); err != nil {
			return fmt.Errorf("clearing operations for role %s: %w", params.Name, err)
		}
		operations, err := grantOperations(ctx, q, row.ID, params.Operations)
		if err != nil {
			return fmt.Errorf("granting operations to role %s: %w", params.Name, err)
		}
		role = roleFrom(row, operations)
		return nil
	})
	if err != nil {
		return Role{}, err
	}
	s.logger.InfoContext(ctx, "updated role", "role", role.Name, "operations", role.Operations)
	return role, nil
}

// DeleteRole removes an admin-defined role by name, cascading to its
// role_operations rows. It is a single statement, so no transaction is
// needed: the cascade runs inside the DELETE's own.
//
// The statement itself refuses a built-in (WHERE ... AND NOT builtin), so a
// built-in reports as ErrNotFound here. That is deliberately NOT the answer
// an admin sees: internal/handler/role reads the role first and returns
// failed_precondition with a reason, because "author is built-in" and
// "there is no role called author" are different facts. This predicate is
// the backstop under that check, not a substitute for it -- see
// queries/roles.sql.
func (s *Store) DeleteRole(ctx context.Context, name string) error {
	deleted, err := s.q.DeleteRole(ctx, name)
	if err != nil {
		return fmt.Errorf("deleting role %s: %w", name, err)
	}
	if deleted == 0 {
		return fmt.Errorf("deleting role %s: %w", name, ErrNotFound)
	}
	s.logger.InfoContext(ctx, "deleted role", "role", name)
	return nil
}

// inTx runs fn inside a transaction on s.db, committing if it returns nil
// and rolling back otherwise. fn receives a *gen.Queries bound to the
// transaction; it must use that one and never s.q, which is bound to the
// pool and would escape the transaction.
func (s *Store) inTx(ctx context.Context, fn func(*gen.Queries) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning role transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(gen.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing role transaction: %w", err)
	}
	return nil
}

// grantOperations inserts one role_operations row per operation and returns
// them sorted, matching the ORDER BY operation every read path in this
// package uses -- so a Role returned from a write is byte-identical to the
// same Role read back, and a caller comparing the two sees no spurious
// difference.
//
// Duplicates are NOT filtered here: PRIMARY KEY (role_id, operation) raises
// a unique violation on the second copy, which fails the whole transaction.
// internal/handler/role de-duplicates its request before calling, so a
// duplicate arriving here means a caller skipped that, and failing loudly
// is the correct answer to it.
func grantOperations(ctx context.Context, q *gen.Queries, roleID pgtype.UUID, operations []string) ([]string, error) {
	granted := slices.Sorted(slices.Values(operations))
	for _, operation := range granted {
		if err := q.InsertRoleOperation(ctx, gen.InsertRoleOperationParams{RoleID: roleID, Operation: operation}); err != nil {
			return nil, fmt.Errorf("granting operation %s: %w", operation, err)
		}
	}
	return granted, nil
}

// roleFrom assembles a Role from its roles row and its already-read
// operations, the one place the column-to-field mapping lives.
func roleFrom(row gen.Role, operations []string) Role {
	return Role{
		ID:           uuidFromPG(row.ID),
		Name:         row.Name,
		Instructions: row.Instructions,
		Builtin:      row.Builtin,
		Operations:   operations,
		CreatedAt:    row.CreatedAt.Time,
		UpdatedAt:    row.UpdatedAt.Time,
	}
}
