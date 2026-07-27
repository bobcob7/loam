package mirrorsync

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/reposstore"
)

// repoFixture returns a repoByNameLookupMock resolving RepoID("acme/widgets")
// to a repo with the given id and indexed branch. Every test below uses this
// same repo/branch pair unless it is exercising the repo-lookup failure path
// itself.
func repoFixture(t *testing.T, repoID uuid.UUID, indexedBranch string) *repoByNameLookupMock {
	t.Helper()
	return &repoByNameLookupMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			assert.Equal(t, "acme/widgets", name)
			return reposstore.Repo{ID: repoID, IndexedBranch: indexedBranch}, nil
		},
	}
}

// spyIngestedRefLookup is a fully configured ingestedRefLookupMock that
// records whether IngestedRef was ever called and returns result/err on
// every call. Using a configured spy (rather than a bare &Mock{} that
// panics on an unconfigured method) means a test asserting "never called"
// fails via a real assertion (assert.False on the recorded flag), not a
// runtime panic -- the task's explicit requirement, and it also means a
// mutation that changes what this method is called WITH (not just whether)
// is still observable if a test chooses to inspect the recorded call.
type spyIngestedRefLookup struct {
	*ingestedRefLookupMock
	called *bool
}

func newSpyIngestedRefLookup(result reposstore.IngestedRef, err error) spyIngestedRefLookup {
	called := new(bool)
	return spyIngestedRefLookup{
		called: called,
		ingestedRefLookupMock: &ingestedRefLookupMock{
			IngestedRefFunc: func(context.Context, uuid.UUID, string) (reposstore.IngestedRef, error) {
				*called = true
				return result, err
			},
		},
	}
}

// spyIngestJobEnqueuer is the same call-tracking treatment as
// spyIngestedRefLookup, for ingestJobEnqueuer.
type spyIngestJobEnqueuer struct {
	*ingestJobEnqueuerMock
	calls *[]string
}

func newSpyIngestJobEnqueuer(err error) spyIngestJobEnqueuer {
	calls := new([]string)
	return spyIngestJobEnqueuer{
		calls: calls,
		ingestJobEnqueuerMock: &ingestJobEnqueuerMock{
			EnqueueFunc: func(_ context.Context, _ uuid.UUID, targetBranch string, _ ingest.Kind) error {
				*calls = append(*calls, targetBranch)
				return err
			},
		},
	}
}

// TestEnqueueIngest_FirstEnrollment_EnqueuesFull proves a NULL
// repo_target_branches.ingested_ref (IngestedRef.Ok false) enqueues
// ingest.KindFull, not KindIncremental. Mutation killed: an implementation
// that always requests KindIncremental (ignoring ref.Ok) passes the wrong
// kind here, caught by the exact-args assertion inside EnqueueFunc, not by
// enqueued/err alone.
func TestEnqueueIngest_FirstEnrollment_EnqueuesFull(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := repoFixture(t, repoID, "main")
	targets := &ingestedRefLookupMock{
		IngestedRefFunc: func(_ context.Context, gotRepoID uuid.UUID, branch string) (reposstore.IngestedRef, error) {
			assert.Equal(t, repoID, gotRepoID)
			assert.Equal(t, "main", branch)
			return reposstore.IngestedRef{Ok: false}, nil
		},
	}
	var enqueueCalls int
	enqueuer := &ingestJobEnqueuerMock{
		EnqueueFunc: func(_ context.Context, gotRepoID uuid.UUID, targetBranch string, kind ingest.Kind) error {
			enqueueCalls++
			assert.Equal(t, repoID, gotRepoID)
			assert.Equal(t, "main", targetBranch)
			assert.Equal(t, ingest.KindFull, kind)
			return nil
		},
	}
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	advanced := []Advance{{Branch: "main", OldSHA: "", NewSHA: "bbb"}}
	enqueued, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), advanced)
	require.NoError(t, err)
	assert.True(t, enqueued)
	assert.Equal(t, 1, enqueueCalls)
}

// TestEnqueueIngest_SubsequentAdvance_EnqueuesIncremental proves a
// recorded, differing ingested_ref enqueues ingest.KindIncremental.
// Mutation killed: an implementation that always requests KindFull
// (ignoring ref.Ok/ref.Ref entirely) passes the wrong kind here.
func TestEnqueueIngest_SubsequentAdvance_EnqueuesIncremental(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := repoFixture(t, repoID, "main")
	targets := &ingestedRefLookupMock{
		IngestedRefFunc: func(context.Context, uuid.UUID, string) (reposstore.IngestedRef, error) {
			return reposstore.IngestedRef{Ok: true, Ref: "aaa"}, nil
		},
	}
	var enqueueCalls int
	enqueuer := &ingestJobEnqueuerMock{
		EnqueueFunc: func(_ context.Context, gotRepoID uuid.UUID, targetBranch string, kind ingest.Kind) error {
			enqueueCalls++
			assert.Equal(t, repoID, gotRepoID)
			assert.Equal(t, "main", targetBranch)
			assert.Equal(t, ingest.KindIncremental, kind)
			return nil
		},
	}
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	advanced := []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: "ccc"}}
	enqueued, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), advanced)
	require.NoError(t, err)
	assert.True(t, enqueued)
	assert.Equal(t, 1, enqueueCalls)
}

// TestEnqueueIngest_DeletedIndexedBranch_NeverEnqueues proves an Advance
// with an empty NewSHA (a deleted ref) never reaches IngestedRef or Enqueue.
// Mutation killed: an implementation that reads IngestedRef and/or enqueues
// for a deletion flips targets.called and/or records an Enqueue call, both
// asserted false below -- catching "enqueues for a deleted branch" via
// assertion, not a mock panic.
func TestEnqueueIngest_DeletedIndexedBranch_NeverEnqueues(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := repoFixture(t, repoID, "main")
	targets := newSpyIngestedRefLookup(reposstore.IngestedRef{Ok: true, Ref: "zzz"}, nil)
	enqueuer := newSpyIngestJobEnqueuer(nil)
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	advanced := []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: ""}}
	enqueued, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), advanced)
	require.NoError(t, err)
	assert.False(t, enqueued)
	assert.False(t, *targets.called, "IngestedRef must not be consulted for a deleted branch")
	assert.Empty(t, *enqueuer.calls, "Enqueue must not be called for a deleted branch")
}

// TestEnqueueIngest_NonIndexedBranchAdvance_NeverEnqueues proves an advance
// on a branch that is NOT repo.IndexedBranch (e.g. one of loam-giq.4's
// wider union of listed/work-branch targets) is skipped entirely. Mutation
// killed: an implementation that enqueues for every detected advance
// regardless of whether it is the indexed branch records "release" in
// enqueuer.calls, asserted empty below.
func TestEnqueueIngest_NonIndexedBranchAdvance_NeverEnqueues(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := repoFixture(t, repoID, "main")
	targets := newSpyIngestedRefLookup(reposstore.IngestedRef{Ok: false}, nil)
	enqueuer := newSpyIngestJobEnqueuer(nil)
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	advanced := []Advance{{Branch: "release", OldSHA: "aaa", NewSHA: "bbb"}}
	enqueued, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), advanced)
	require.NoError(t, err)
	assert.False(t, enqueued)
	assert.False(t, *targets.called, "IngestedRef must not be consulted for a non-indexed branch")
	assert.Empty(t, *enqueuer.calls, "Enqueue must not be called for a non-indexed branch")
}

// TestEnqueueIngest_MixedAdvances_OnlyIndexedBranchEnqueued proves that,
// given advances for both the indexed branch and another target branch in
// the same call, exactly one Enqueue call happens, for the indexed branch
// only. This exercises the filter with the non-indexed branch actually
// present alongside a real match, rather than alone (as the prior test
// does), so a filter that accidentally enqueues everything it sees would be
// caught even if it also happens to enqueue the correct branch.
func TestEnqueueIngest_MixedAdvances_OnlyIndexedBranchEnqueued(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := repoFixture(t, repoID, "main")
	targets := &ingestedRefLookupMock{
		IngestedRefFunc: func(_ context.Context, _ uuid.UUID, branch string) (reposstore.IngestedRef, error) {
			assert.Equal(t, "main", branch)
			return reposstore.IngestedRef{Ok: true, Ref: "aaa"}, nil
		},
	}
	enqueuer := newSpyIngestJobEnqueuer(nil)
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	advanced := []Advance{
		{Branch: "release", OldSHA: "xxx", NewSHA: "yyy"},
		{Branch: "main", OldSHA: "aaa", NewSHA: "ccc"},
	}
	didEnqueue, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), advanced)
	require.NoError(t, err)
	assert.True(t, didEnqueue)
	assert.Equal(t, []string{"main"}, *enqueuer.calls)
}

// TestEnqueueIngest_AlreadyAtIngestedSHA_NoOp proves the defensive
// idempotency check: when the advance's NewSHA already equals the recorded
// ingested_ref (e.g. a force-push back to a previously-ingested commit),
// nothing is enqueued. This is deliberately not relying on
// ingest.Enqueue's own coalescing (the task's instruction not to lean on it
// as the whole correctness argument) -- enqueuer.calls, asserted empty,
// proves this method itself never calls Enqueue for a no-op advance.
func TestEnqueueIngest_AlreadyAtIngestedSHA_NoOp(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := repoFixture(t, repoID, "main")
	targets := &ingestedRefLookupMock{
		IngestedRefFunc: func(context.Context, uuid.UUID, string) (reposstore.IngestedRef, error) {
			return reposstore.IngestedRef{Ok: true, Ref: "ccc"}, nil
		},
	}
	enqueuer := newSpyIngestJobEnqueuer(nil)
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	advanced := []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: "ccc"}}
	enqueued, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), advanced)
	require.NoError(t, err)
	assert.False(t, enqueued)
	assert.Empty(t, *enqueuer.calls)
}

// TestEnqueueIngest_IngestedRefNotFound_ReturnsErrorRatherThanFull proves
// that a missing repo_target_branches row (reposstore.ErrNotFound) is
// treated as a hard error, never silently coerced into "first enrollment"
// and enqueued as KindFull anyway. Mutation killed: an implementation that
// treats any IngestedRef error the same as Ok=false records an Enqueue call
// here, asserted empty, instead of the wrapped error this test asserts.
func TestEnqueueIngest_IngestedRefNotFound_ReturnsErrorRatherThanFull(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := repoFixture(t, repoID, "main")
	targets := &ingestedRefLookupMock{
		IngestedRefFunc: func(context.Context, uuid.UUID, string) (reposstore.IngestedRef, error) {
			return reposstore.IngestedRef{}, reposstore.ErrNotFound
		},
	}
	enqueuer := newSpyIngestJobEnqueuer(nil)
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	advanced := []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: "ccc"}}
	enqueued, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), advanced)
	require.Error(t, err)
	assert.True(t, errors.Is(err, reposstore.ErrNotFound))
	assert.False(t, enqueued)
	assert.Empty(t, *enqueuer.calls)
}

// TestEnqueueIngest_IngestedRefLookupFails_PropagatesError proves a plain
// (non-ErrNotFound) IngestedRef failure aborts without enqueuing.
func TestEnqueueIngest_IngestedRefLookupFails_PropagatesError(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := repoFixture(t, repoID, "main")
	sentinel := errors.New("boom")
	targets := &ingestedRefLookupMock{
		IngestedRefFunc: func(context.Context, uuid.UUID, string) (reposstore.IngestedRef, error) {
			return reposstore.IngestedRef{}, sentinel
		},
	}
	enqueuer := newSpyIngestJobEnqueuer(nil)
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	advanced := []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: "ccc"}}
	enqueued, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), advanced)
	require.Error(t, err)
	assert.True(t, errors.Is(err, sentinel))
	assert.False(t, enqueued)
	assert.Empty(t, *enqueuer.calls)
}

// TestEnqueueIngest_EnqueueFails_ReturnsWrappedErrorAndFalse proves a
// failing Enqueue call surfaces the error and reports enqueued=false when
// this was the only matching advance -- IngestEnqueuer's own doc comment:
// enqueued is true only once ownership has actually passed to the worker.
func TestEnqueueIngest_EnqueueFails_ReturnsWrappedErrorAndFalse(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := repoFixture(t, repoID, "main")
	targets := &ingestedRefLookupMock{
		IngestedRefFunc: func(context.Context, uuid.UUID, string) (reposstore.IngestedRef, error) {
			return reposstore.IngestedRef{Ok: false}, nil
		},
	}
	sentinel := errors.New("db unavailable")
	enqueuer := newSpyIngestJobEnqueuer(sentinel)
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	advanced := []Advance{{Branch: "main", OldSHA: "", NewSHA: "bbb"}}
	enqueued, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), advanced)
	require.Error(t, err)
	assert.True(t, errors.Is(err, sentinel))
	assert.False(t, enqueued)
	assert.Equal(t, []string{"main"}, *enqueuer.calls, "Enqueue must still have been attempted once")
}

// TestEnqueueIngest_RepoLookupFails_ReturnsErrorWithoutTouchingOtherCollaborators
// proves a GetRepoByName failure aborts before either the ingested-ref
// lookup or Enqueue is ever reached.
func TestEnqueueIngest_RepoLookupFails_ReturnsErrorWithoutTouchingOtherCollaborators(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("not found")
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{}, sentinel
		},
	}
	targets := newSpyIngestedRefLookup(reposstore.IngestedRef{Ok: true, Ref: "aaa"}, nil)
	enqueuer := newSpyIngestJobEnqueuer(nil)
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	advanced := []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: "bbb"}}
	enqueued, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), advanced)
	require.Error(t, err)
	assert.True(t, errors.Is(err, sentinel))
	assert.False(t, enqueued)
	assert.False(t, *targets.called)
	assert.Empty(t, *enqueuer.calls)
}

// TestEnqueueIngest_EmptyAdvances_NeverTouchesOtherCollaborators proves the
// zero-advance tick (nothing changed this fetch) is a clean no-op.
func TestEnqueueIngest_EmptyAdvances_NeverTouchesOtherCollaborators(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := repoFixture(t, repoID, "main")
	targets := newSpyIngestedRefLookup(reposstore.IngestedRef{Ok: true, Ref: "aaa"}, nil)
	enqueuer := newSpyIngestJobEnqueuer(nil)
	e := NewStoreIngestEnqueuer(repos, targets, enqueuer)
	enqueued, err := e.EnqueueIngest(t.Context(), RepoID("acme/widgets"), nil)
	require.NoError(t, err)
	assert.False(t, enqueued)
	assert.False(t, *targets.called)
	assert.Empty(t, *enqueuer.calls)
}
