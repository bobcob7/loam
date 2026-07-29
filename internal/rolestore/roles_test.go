package rolestore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestGetRole_NoRows_ReturnsErrNotFound proves an unrecognized role name
// (roles.name has no match) surfaces the distinguishable ErrNotFound
// rather than a bare pgx.ErrNoRows a caller has to know to check for
// separately.
func TestGetRole_NoRows_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	// QueryFunc is configured too, even though correct code never reaches
	// it here: a mutation that ignored GetRoleByName's error and fell
	// through to list operations anyway must surface as a clean assertion
	// failure below, not a nil-func panic.
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
		QueryFunc: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return nil, errors.New("must not be called: role lookup already failed")
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.GetRole(t.Context(), "not-a-role")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestGetRole_QueryRowOtherError_NotMappedToNotFound proves an unrelated
// database failure (e.g. a connection error) is NOT mislabeled as
// ErrNotFound -- only pgx.ErrNoRows maps to it.
func TestGetRole_QueryRowOtherError_NotMappedToNotFound(t *testing.T) {
	t.Parallel()
	connErr := pgx.ErrTxClosed
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: connErr}
		},
		QueryFunc: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return nil, errors.New("must not be called: role lookup already failed")
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.GetRole(t.Context(), "author")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, err, connErr)
}

// TestGetRole_ListOperationsError_Propagates proves a failure listing
// role_operations after a successful role lookup surfaces, wrapped with
// context, rather than being swallowed or silently returning a Role with
// no operations.
func TestGetRole_ListOperationsError_Propagates(t *testing.T) {
	t.Parallel()
	queryErr := errors.New("role_operations query failed")
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: nil}
		},
		QueryFunc: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.GetRole(t.Context(), "author")
	require.Error(t, err)
	assert.ErrorIs(t, err, queryErr)
}

// TestListRoles_QueryError_Propagates proves a failure listing roles is
// returned rather than swallowed into an empty list -- "the database is
// down" and "there are no roles" must never look alike to the admin API.
func TestListRoles_QueryError_Propagates(t *testing.T) {
	t.Parallel()
	queryErr := errors.New("roles query failed")
	mock := &querierMock{
		QueryFunc: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	s := NewStore(mock, testLogger())
	roles, err := s.ListRoles(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, queryErr)
	assert.Nil(t, roles)
}

// TestDeleteRole_NoRowsDeleted_ReturnsErrNotFound proves a DELETE that
// matched nothing -- an unknown name, or a built-in refused by the
// statement's own `AND NOT builtin` predicate -- is reported as
// ErrNotFound rather than silently succeeding.
func TestDeleteRole_NoRowsDeleted_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("DELETE 0"), nil
		},
	}
	s := NewStore(mock, testLogger())
	err := s.DeleteRole(t.Context(), "author")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestDeleteRole_RowDeleted_Succeeds is the positive control for the test
// above: the same code path with one affected row must NOT report
// not-found.
func TestDeleteRole_RowDeleted_Succeeds(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("DELETE 1"), nil
		},
	}
	s := NewStore(mock, testLogger())
	assert.NoError(t, s.DeleteRole(t.Context(), "release-captain"))
}

// TestDeleteRole_ExecError_IsNotMappedToNotFound proves a database failure
// is not misreported as "no such role", which would tell an admin their
// role is already gone when it is not.
func TestDeleteRole_ExecError_IsNotMappedToNotFound(t *testing.T) {
	t.Parallel()
	execErr := errors.New("connection refused")
	mock := &querierMock{
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	s := NewStore(mock, testLogger())
	err := s.DeleteRole(t.Context(), "release-captain")
	require.Error(t, err)
	assert.ErrorIs(t, err, execErr)
	assert.NotErrorIs(t, err, ErrNotFound)
}

// TestCreateRole_BeginError_Propagates proves a failure to OPEN the
// transaction fails the create, rather than falling through to a
// non-transactional write of the role and its operations.
func TestCreateRole_BeginError_Propagates(t *testing.T) {
	t.Parallel()
	beginErr := errors.New("connection refused")
	// Exec and QueryRow are configured, though correct code never reaches
	// them: a mutation that ignored the Begin error and wrote anyway must
	// fail the assertion below rather than panic on a nil mock func.
	mock := &querierMock{
		BeginFunc: func(context.Context) (pgx.Tx, error) { return nil, beginErr },
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: errors.New("must not be called: the transaction never opened")}
		},
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, errors.New("must not be called: the transaction never opened")
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.CreateRole(t.Context(), RoleParams{Name: "release-captain", Operations: []string{"search"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, beginErr)
}

// TestUpdateRole_BeginError_Propagates mirrors the create case: an update
// that could not open a transaction must not proceed to clear a role's
// operations outside one.
func TestUpdateRole_BeginError_Propagates(t *testing.T) {
	t.Parallel()
	beginErr := errors.New("connection refused")
	mock := &querierMock{
		BeginFunc: func(context.Context) (pgx.Tx, error) { return nil, beginErr },
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, errors.New("must not be called: the transaction never opened")
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.UpdateRole(t.Context(), RoleParams{Name: "release-captain"})
	require.Error(t, err)
	assert.ErrorIs(t, err, beginErr)
}

// TestIsRoleNameCollision_OnlyMatchesTheRolesNameConstraint proves
// ErrAlreadyExists is reserved for a duplicate ROLE NAME. role_operations'
// PRIMARY KEY raises the same SQLSTATE, and reporting that to an admin as
// "role already exists" would send them looking for a role that is not
// there.
func TestIsRoleNameCollision_OnlyMatchesTheRolesNameConstraint(t *testing.T) {
	t.Parallel()
	assert.True(t, isRoleNameCollision(&pgconn.PgError{Code: "23505", ConstraintName: "roles_name_key"}))
	assert.False(t, isRoleNameCollision(&pgconn.PgError{Code: "23505", ConstraintName: "role_operations_pkey"}),
		"a duplicate operation is not a duplicate role name")
	assert.False(t, isRoleNameCollision(&pgconn.PgError{Code: "23503", ConstraintName: "roles_name_key"}),
		"only a unique violation is a name collision")
	assert.False(t, isRoleNameCollision(errors.New("connection refused")))
}
