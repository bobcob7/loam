package repoadmin

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

func removeReq(name string) *connect.Request[adminv1.RemoveRepoRequest] {
	return connect.NewRequest(&adminv1.RemoveRepoRequest{Repo: name})
}

// TestRemoveRepo_NoWorkBranches_DeletesAndSucceeds is the happy path: no
// non-terminal work branch exists, so the guard clears and DeleteRepo
// runs.
func TestRemoveRepo_NoWorkBranches_DeletesAndSucceeds(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	h := d.handler(t, "/data")
	resp, err := h.RemoveRepo(t.Context(), removeReq("acme/widgets"))
	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Len(t, d.deleter.DeleteRepoCalls(), 1)
}

// TestRemoveRepo_NonTerminalWorkBranch_FailedPreconditionWithDetail is the
// central "guarded removal" mutation-kill: an open (non-terminal) work
// branch must block removal with CodeFailedPrecondition, carrying a
// typed RemovalBlocked detail (docs/web-spec.md: "travels as a typed
// Connect error detail so the UI renders it structurally, never by
// parsing the message"), and DeleteRepo must never run.
func TestRemoveRepo_NonTerminalWorkBranch_FailedPreconditionWithDetail(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	title := "Add feature X"
	d.workBranch.ListFunc = func(_ context.Context, filter workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
		return []workbranchstore.WorkBranch{
			{Name: "wb-open1", Title: &title, State: workbranchstore.StateReviewable},
		}, 1, nil
	}
	h := d.handler(t, "/data")
	_, err := h.RemoveRepo(t.Context(), removeReq("acme/widgets"))
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Equal(t, connect.CodeFailedPrecondition, connErr.Code())
	assert.Empty(t, d.deleter.DeleteRepoCalls(), "removal must never proceed while a non-terminal work branch exists")
	require.Len(t, connErr.Details(), 1, "the blocker set must travel as a typed error detail, not just an error message")
	detailMsg, decodeErr := connErr.Details()[0].Value()
	require.NoError(t, decodeErr)
	blocked, ok := detailMsg.(*adminv1.RemovalBlocked)
	require.True(t, ok, "the detail must decode as RemovalBlocked")
	require.Len(t, blocked.GetBlockers(), 1)
	assert.Equal(t, "wb-open1", blocked.GetBlockers()[0].GetName())
	assert.Equal(t, "Add feature X", blocked.GetBlockers()[0].GetTitle())
}

// TestRemoveRepo_OnlyTerminalWorkBranches_NotBlocked proves complete/
// closed work branches never count as blockers -- the removal must
// proceed.
func TestRemoveRepo_OnlyTerminalWorkBranches_NotBlocked(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.workBranch.ListFunc = func(_ context.Context, _ workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
		return []workbranchstore.WorkBranch{
			{Name: "wb-done", State: workbranchstore.StateComplete},
			{Name: "wb-closed", State: workbranchstore.StateClosed},
		}, 2, nil
	}
	h := d.handler(t, "/data")
	_, err := h.RemoveRepo(t.Context(), removeReq("acme/widgets"))
	require.NoError(t, err)
	assert.Len(t, d.deleter.DeleteRepoCalls(), 1)
}

// TestRemoveRepo_EveryNonTerminalStateBlocks proves draft, reviewable,
// and reviewed all count as blockers, not merely one hand-picked state.
func TestRemoveRepo_EveryNonTerminalStateBlocks(t *testing.T) {
	t.Parallel()
	for _, state := range []workbranchstore.State{workbranchstore.StateDraft, workbranchstore.StateReviewable, workbranchstore.StateReviewed} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			d.workBranch.ListFunc = func(_ context.Context, _ workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
				return []workbranchstore.WorkBranch{{Name: "wb-x", State: state}}, 1, nil
			}
			h := d.handler(t, "/data")
			_, err := h.RemoveRepo(t.Context(), removeReq("acme/widgets"))
			require.Error(t, err)
			assert.Empty(t, d.deleter.DeleteRepoCalls())
		})
	}
}

// TestRemoveRepo_UnenrolledRepo_NotFound proves an unenrolled repo maps
// to CodeNotFound before the work-branch guard or deleter ever run.
func TestRemoveRepo_UnenrolledRepo_NotFound(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.GetRepoByNameFunc = func(_ context.Context, name string) (reposstore.Repo, error) {
		return reposstore.Repo{}, reposstore.ErrNotFound
	}
	h := d.handler(t, "/data")
	_, err := h.RemoveRepo(t.Context(), removeReq("acme/ghost"))
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Equal(t, connect.CodeNotFound, connErr.Code())
	assert.Empty(t, d.workBranch.ListCalls())
	assert.Empty(t, d.deleter.DeleteRepoCalls())
}

// TestRemoveRepo_EmptyRepo_InvalidArgument proves an empty repo
// identifier is rejected before any store call.
func TestRemoveRepo_EmptyRepo_InvalidArgument(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	h := d.handler(t, "/data")
	_, err := h.RemoveRepo(t.Context(), removeReq(""))
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Equal(t, connect.CodeInvalidArgument, connErr.Code())
	assert.Empty(t, d.store.GetRepoByNameCalls())
}

// TestRemoveRepo_DeleteRepoNotImplemented_MapsToInternalAndLogs proves
// cmd/server/main.go's notImplementedRepoDeleter stand-in (wired until
// loam-cwb lands a real cross-table delete) surfaces as a real, logged
// error -- never a silent success -- when the guard clears but no repo
// row can actually be dropped yet.
func TestRemoveRepo_DeleteRepoNotImplemented_MapsToInternalAndLogs(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	wantErr := "repo delete path not implemented (loam-cwb)"
	d.deleter.DeleteRepoFunc = func(_ context.Context, _ uuid.UUID) error {
		return errors.New(wantErr)
	}
	h := d.handler(t, "/data")
	_, err := h.RemoveRepo(t.Context(), removeReq("acme/widgets"))
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Equal(t, connect.CodeInternal, connErr.Code())
	assert.Contains(t, d.buf.String(), wantErr)
}
