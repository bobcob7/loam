package rolestore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestGetRole_NoRows_ReturnsErrNotFound proves an unrecognized role name
// (roles.name has no match) surfaces the distinguishable errNotFound
// rather than a bare pgx.ErrNoRows a caller has to know to check for
// separately.
func TestGetRole_NoRows_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.GetRole(t.Context(), "not-a-role")
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

// TestGetRole_QueryRowOtherError_NotMappedToNotFound proves an unrelated
// database failure (e.g. a connection error) is NOT mislabeled as
// errNotFound -- only pgx.ErrNoRows maps to it.
func TestGetRole_QueryRowOtherError_NotMappedToNotFound(t *testing.T) {
	t.Parallel()
	connErr := pgx.ErrTxClosed
	mock := &querierMock{
		QueryRowFunc: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return fakeRow{err: connErr}
		},
	}
	s := NewStore(mock, testLogger())
	_, err := s.GetRole(t.Context(), "author")
	require.Error(t, err)
	assert.NotErrorIs(t, err, errNotFound)
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
