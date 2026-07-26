package state

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/mirrorsync"
)

// testRepoID is a name-shaped RepoID -- "<group>/<repo_name>", the only
// identifier the proto surface and the rest of mirrorsync's collaborators
// ever key a repo by (docs/persistence-spec.md's repos.name, unique).
const testRepoID = mirrorsync.RepoID("bobcob7/doc-server")

// updateOne is the CommandTag a single-row UPDATE reports, the shape every
// happy-path test in this file expects Exec to return.
func updateOne() (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// updateZero is the CommandTag Postgres reports when the WHERE clause
// matched no row, the shape the not-found tests expect Exec to return.
func updateZero() (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 0"), nil
}

func TestReportSyncing_WritesSyncingState(t *testing.T) {
	t.Parallel()
	var gotSQL string
	var gotArgs []any
	mock := &execerMock{
		ExecFunc: func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			gotSQL = sql
			gotArgs = args
			return updateOne()
		},
	}
	r := New(mock)
	err := r.ReportSyncing(t.Context(), testRepoID)
	require.NoError(t, err)
	require.Len(t, mock.ExecCalls(), 1)
	assert.Contains(t, gotSQL, "sync_state = 'syncing'")
	assert.Contains(t, gotSQL, "WHERE name = $1")
	require.Len(t, gotArgs, 1)
	assert.Equal(t, string(testRepoID), gotArgs[0])
}

func TestReportIdle_EnqueuedIngestFalse_WritesIdleAndClearsError(t *testing.T) {
	t.Parallel()
	var gotSQL string
	var gotArgs []any
	mock := &execerMock{
		ExecFunc: func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			gotSQL = sql
			gotArgs = args
			return updateOne()
		},
	}
	r := New(mock)
	err := r.ReportIdle(t.Context(), testRepoID, false)
	require.NoError(t, err)
	require.Len(t, mock.ExecCalls(), 1)
	assert.Contains(t, gotSQL, "sync_state = 'idle'")
	assert.Contains(t, gotSQL, "last_synced_at = now()")
	assert.Contains(t, gotSQL, "sync_error = NULL")
	assert.Contains(t, gotSQL, "WHERE name = $1")
	require.Len(t, gotArgs, 1)
	assert.Equal(t, string(testRepoID), gotArgs[0])
}

// TestReportIdle_EnqueuedIngestTrue_DoesNotWrite is the exact race this bead
// exists to prevent: once step 4 enqueues an ingest job, ownership of
// sync_state passes to the ingest worker (loam-c94.13), so ReportIdle must
// not write the terminal state itself. A mutation that drops the
// enqueuedIngest guard (always writing idle) makes this test fail, because
// the mock has no ExecFunc configured and panics on any call.
func TestReportIdle_EnqueuedIngestTrue_DoesNotWrite(t *testing.T) {
	t.Parallel()
	mock := &execerMock{}
	r := New(mock)
	err := r.ReportIdle(t.Context(), testRepoID, true)
	require.NoError(t, err)
	assert.Empty(t, mock.ExecCalls(), "ReportIdle must not write when ownership has transferred to the ingest worker")
}

func TestReportIdle_ExecFails_WrapsError(t *testing.T) {
	t.Parallel()
	execErr := errors.New("connection reset")
	mock := &execerMock{
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	r := New(mock)
	err := r.ReportIdle(t.Context(), testRepoID, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, execErr)
}

func TestReportError_EnqueuedIngestFalse_WritesErrorMessage(t *testing.T) {
	t.Parallel()
	var gotSQL string
	var gotArgs []any
	mock := &execerMock{
		ExecFunc: func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			gotSQL = sql
			gotArgs = args
			return updateOne()
		},
	}
	r := New(mock)
	syncErr := errors.New("fetching repo x: connection refused")
	err := r.ReportError(t.Context(), testRepoID, syncErr, false)
	require.NoError(t, err)
	require.Len(t, mock.ExecCalls(), 1)
	assert.Contains(t, gotSQL, "sync_state = 'error'")
	assert.Contains(t, gotSQL, "WHERE name = $2")
	require.Len(t, gotArgs, 2)
	assert.Equal(t, "fetching repo x: connection refused", gotArgs[0])
	assert.Equal(t, string(testRepoID), gotArgs[1])
}

// TestReportError_NilSyncErr_WritesEmptyMessage exercises the defensive nil
// fallback: the scheduler never calls ReportError with a nil error (it only
// does so from its own error path), but Reporter must not panic if it ever
// did -- it writes sync_state = 'error' with an empty sync_error rather than
// crash the caller.
func TestReportError_NilSyncErr_WritesEmptyMessage(t *testing.T) {
	t.Parallel()
	var gotArgs []any
	mock := &execerMock{
		ExecFunc: func(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
			gotArgs = args
			return updateOne()
		},
	}
	r := New(mock)
	err := r.ReportError(t.Context(), testRepoID, nil, false)
	require.NoError(t, err)
	require.Len(t, gotArgs, 2)
	assert.Equal(t, "", gotArgs[0])
}

// TestReportError_EnqueuedIngestTrue_DoesNotWrite mirrors the idle case: step
// 4 can succeed (handing off ownership) while step 5 subsequently fails, so
// ReportError also honors the hand-off and must not write.
func TestReportError_EnqueuedIngestTrue_DoesNotWrite(t *testing.T) {
	t.Parallel()
	mock := &execerMock{}
	r := New(mock)
	err := r.ReportError(t.Context(), testRepoID, errors.New("polling PRs: timeout"), true)
	require.NoError(t, err)
	assert.Empty(t, mock.ExecCalls(), "ReportError must not write when ownership has transferred to the ingest worker")
}

func TestReportError_ExecFails_WrapsError(t *testing.T) {
	t.Parallel()
	execErr := errors.New("connection reset")
	mock := &execerMock{
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	r := New(mock)
	err := r.ReportError(t.Context(), testRepoID, errors.New("boom"), false)
	require.Error(t, err)
	assert.ErrorIs(t, err, execErr)
}

func TestReportSyncing_RepoNotFound_ReturnsErrRepoNotFound(t *testing.T) {
	t.Parallel()
	mock := &execerMock{
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return updateZero()
		},
	}
	r := New(mock)
	err := r.ReportSyncing(t.Context(), testRepoID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errRepoNotFound)
}

func TestReportIdle_RepoNotFound_ReturnsErrRepoNotFound(t *testing.T) {
	t.Parallel()
	mock := &execerMock{
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return updateZero()
		},
	}
	r := New(mock)
	err := r.ReportIdle(t.Context(), testRepoID, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, errRepoNotFound)
}

func TestReportError_RepoNotFound_ReturnsErrRepoNotFound(t *testing.T) {
	t.Parallel()
	mock := &execerMock{
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return updateZero()
		},
	}
	r := New(mock)
	err := r.ReportError(t.Context(), testRepoID, errors.New("boom"), false)
	require.Error(t, err)
	assert.ErrorIs(t, err, errRepoNotFound)
}

func TestReportSyncing_ExecFails_WrapsError(t *testing.T) {
	t.Parallel()
	execErr := errors.New("connection reset")
	mock := &execerMock{
		ExecFunc: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	r := New(mock)
	err := r.ReportSyncing(t.Context(), testRepoID)
	require.Error(t, err)
	assert.ErrorIs(t, err, execErr)
}
