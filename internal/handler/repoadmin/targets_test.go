package repoadmin

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/reposstore"
)

func setTargetsReq(name string, targets []string, indexed string) *connect.Request[adminv1.SetTargetBranchesRequest] {
	return connect.NewRequest(&adminv1.SetTargetBranchesRequest{Repo: name, TargetBranches: targets, IndexedBranch: indexed})
}

// TestSetTargetBranches_AddsAndRemovesSetDifference proves the "replace"
// semantics: a branch newly listed is added, a previously-listed branch
// dropped from the request is removed, and one present in both is
// touched by neither call.
func TestSetTargetBranches_AddsAndRemovesSetDifference(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.ListTargetBranchesFunc = func(_ context.Context, _ uuid.UUID) ([]reposstore.TargetBranch, error) {
		return []reposstore.TargetBranch{{Branch: "main"}, {Branch: "release"}}, nil
	}
	h := d.handler(t, "/data")
	_, err := h.SetTargetBranches(t.Context(), setTargetsReq("acme/widgets", []string{"main", "docs"}, "main"))
	require.NoError(t, err)
	require.Len(t, d.store.AddTargetBranchCalls(), 1, "only the genuinely new branch must be added")
	assert.Equal(t, "docs", d.store.AddTargetBranchCalls()[0].Branch)
	require.Len(t, d.store.RemoveTargetBranchCalls(), 1, "only the genuinely dropped branch must be removed")
	assert.Equal(t, "release", d.store.RemoveTargetBranchCalls()[0].Branch)
}

// TestSetTargetBranches_RemovingTargetDoesNotTouchWorkBranches proves
// docs/web-spec.md's "Removing a target branch does not end work in
// flight": SetTargetBranches never calls into workbranchstore at all,
// so an existing work branch targeting a delisted branch is left
// completely alone.
func TestSetTargetBranches_RemovingTargetDoesNotTouchWorkBranches(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.ListTargetBranchesFunc = func(_ context.Context, _ uuid.UUID) ([]reposstore.TargetBranch, error) {
		return []reposstore.TargetBranch{{Branch: "main"}, {Branch: "release"}}, nil
	}
	h := d.handler(t, "/data")
	_, err := h.SetTargetBranches(t.Context(), setTargetsReq("acme/widgets", []string{"main"}, "main"))
	require.NoError(t, err)
	assert.Empty(t, d.workBranch.ListCalls(), "SetTargetBranches must never enumerate or touch work_branches")
}

// TestSetTargetBranches_IndexedBranchNotInTargets_Rejected mirrors
// EnrollRepo's own invariant check for the update path.
func TestSetTargetBranches_IndexedBranchNotInTargets_Rejected(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	h := d.handler(t, "/data")
	_, err := h.SetTargetBranches(t.Context(), setTargetsReq("acme/widgets", []string{"main", "release"}, "docs"))
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Equal(t, connect.CodeInvalidArgument, connErr.Code())
	assert.Empty(t, d.store.UpdateRepoCalls())
}

// TestSetTargetBranches_ChangingIndexedBranch_EnqueuesFullIngest proves
// docs/web-spec.md: "Changing indexed_branch triggers a full ingest of
// the new branch."
func TestSetTargetBranches_ChangingIndexedBranch_EnqueuesFullIngest(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.ListTargetBranchesFunc = func(_ context.Context, _ uuid.UUID) ([]reposstore.TargetBranch, error) {
		return []reposstore.TargetBranch{{Branch: "main"}, {Branch: "release"}}, nil
	}
	h := d.handler(t, "/data")
	_, err := h.SetTargetBranches(t.Context(), setTargetsReq("acme/widgets", []string{"main", "release"}, "release"))
	require.NoError(t, err)
	require.Len(t, d.ingest.EnqueueCalls(), 1, "changing indexed_branch must enqueue exactly one full ingest job")
	assert.Equal(t, "release", d.ingest.EnqueueCalls()[0].TargetBranch)
	assert.Equal(t, ingest.KindFull, d.ingest.EnqueueCalls()[0].Kind)
}

// TestSetTargetBranches_UnchangedIndexedBranch_NoIngestEnqueued proves
// the enqueue is conditional on an actual change, not fired on every
// call regardless.
func TestSetTargetBranches_UnchangedIndexedBranch_NoIngestEnqueued(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.ListTargetBranchesFunc = func(_ context.Context, _ uuid.UUID) ([]reposstore.TargetBranch, error) {
		return []reposstore.TargetBranch{{Branch: "main"}}, nil
	}
	h := d.handler(t, "/data")
	_, err := h.SetTargetBranches(t.Context(), setTargetsReq("acme/widgets", []string{"main"}, "main"))
	require.NoError(t, err)
	assert.Empty(t, d.ingest.EnqueueCalls())
}
