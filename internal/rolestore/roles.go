package rolestore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// Store is the roles + role_operations read seam (see package doc for why
// this is deliberately read-only). Construct with NewStore, passing the
// real *gen.Queries surface in production (a *pgxpool.Pool, which
// satisfies querier directly) or a querier mock in tests.
type Store struct {
	q      *gen.Queries
	logger *slog.Logger
}

// NewStore builds a Store over db, typically a *pgxpool.Pool.
func NewStore(db querier, logger *slog.Logger) *Store {
	return &Store{q: gen.New(db), logger: logger}
}

// GetRole resolves name -- the value a trusted Loam-Agent-Role header
// carries -- to its full Role, including every operation
// role_operations grants it. Returns a wrapped errNotFound if name does
// not exist (roles.name is UNIQUE, roles_name_key, 0001_init.up.sql).
func (s *Store) GetRole(ctx context.Context, name string) (Role, error) {
	row, err := s.q.GetRoleByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Role{}, fmt.Errorf("getting role %s: %w", name, errNotFound)
		}
		return Role{}, fmt.Errorf("getting role %s: %w", name, err)
	}
	operations, err := s.q.ListRoleOperations(ctx, row.ID)
	if err != nil {
		return Role{}, fmt.Errorf("listing operations for role %s: %w", name, err)
	}
	return Role{
		ID:           uuidFromPG(row.ID),
		Name:         row.Name,
		Instructions: row.Instructions,
		Builtin:      row.Builtin,
		Operations:   operations,
		CreatedAt:    row.CreatedAt.Time,
		UpdatedAt:    row.UpdatedAt.Time,
	}, nil
}
