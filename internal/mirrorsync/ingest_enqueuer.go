package mirrorsync

import (
	"context"
	"fmt"

	"github.com/bobcob7/loam/internal/ingest"
)

// StoreIngestEnqueuer is the production IngestEnqueuer (docs/sync-spec.md ->
// Mirror Sync step 4; docs/ingestion-spec.md "Trigger & Scheduling"; owned by
// bead loam-c94.2). For each Advance whose Branch is repo's indexed_branch
// (docs/ingestion-spec.md "Indexed Scope": "Loam indexes each repo's
// designated indexed branch only" -- MVP never enqueues for any other listed
// or work-branch target loam-giq.4's wider union also reports), it compares
// repo_target_branches.ingested_ref against the advance and enqueues via
// ingest.Enqueuer.
//
// Kind selection: repo_target_branches.ingested_ref IS NULL means this
// branch has never been ingested -- first enrollment, or a branch just
// designated indexed_branch that was never the indexed branch before -- so
// Kind is ingest.KindFull; otherwise ingest.KindIncremental.
//
// Where the incremental-vs-full decision actually lives: this bead's
// DESCRIPTION says "choose kind: full for first ingest or no valid diff
// base (force-push, history rewrite, shallow/reset ref)". Only the first
// half is this package's call to make -- "no valid diff base" requires
// running `git merge-base` against the mirror (diffplan.Planner.
// checkMergeBase), plumbing this package has no seam for and must not
// duplicate. diffplan.Planner (loam-c94.3) already re-derives BOTH triggers
// independently and authoritatively at run time, from the exact same
// repo_target_branches.ingested_ref this method reads (internal/ingest.Job's
// own doc comment: the Orchestrator resolves old_ref/new_ref itself from
// RepoID+TargetBranch, never from anything this method could pass through --
// ingest.Enqueue's signature carries no ref parameters at all, so "passing
// old_ref and new_ref for the planner to diff" in this bead's DESIGN
// predates diffplan.Planner landing and no longer describes a call this
// package can make). Concretely: diffplan.Request.OldRef == "" escalates to
// full exactly when repo_target_branches.ingested_ref is NULL -- the same
// condition this method checks -- so the Kind requested here can never make
// Planner's final Plan.Kind wrong; it only affects the ingest_jobs.kind
// column's value between enqueue and the job actually running (an audit/
// bookkeeping nicety: a first-ingest row reads "full" immediately rather
// than "incremental" until Planner corrects it) and, in principle, the
// coalescing key ingest.Enqueue dedupes on. The "no valid diff base" trigger
// itself is never computed here at all -- Planner alone detects it, on
// every request regardless of the Kind this method chose.
type StoreIngestEnqueuer struct {
	repos    repoByNameLookup
	targets  ingestedRefLookup
	enqueuer ingestJobEnqueuer
}

// NewStoreIngestEnqueuer builds a StoreIngestEnqueuer resolving repo.ID and
// IndexedBranch through repos (typically *reposstore.Store), the indexed
// branch's ingested_ref through targets (typically *reposstore.Store), and
// queuing through enqueuer (typically *ingest.Pool).
func NewStoreIngestEnqueuer(repos repoByNameLookup, targets ingestedRefLookup, enqueuer ingestJobEnqueuer) *StoreIngestEnqueuer {
	return &StoreIngestEnqueuer{repos: repos, targets: targets, enqueuer: enqueuer}
}

// EnqueueIngest satisfies IngestEnqueuer. See the type doc comment for Kind
// selection and why this method never resolves or passes a ref pair.
// advanced entries for any branch other than repo's indexed_branch are
// skipped outright -- MVP indexes only the default target branch (this
// bead's DESCRIPTION: "other enrolled target branches are Future Work"),
// even though loam-giq.4 reports advances across a wider union (every listed
// target branch plus the recorded target of every open work branch) for
// mergeability checking's own, unrelated purposes.
//
// An advance with an empty NewSHA is a deleted ref (Advance's own doc
// comment) and is skipped, never enqueued: there is nothing to ingest for a
// branch that no longer exists. In production this is normally unreachable
// for repo's indexed_branch specifically -- StoreAdvanceDetector aborts the
// whole cycle with errMissingTargetBranch before producing any Advance at
// all when a *listed* target branch's ref disappears upstream, and
// indexed_branch is always validated to be one of the listed target
// branches at enrollment -- but this method does not lean on that upstream
// invariant holding; it checks explicitly.
//
// An advance whose NewSHA already equals the recorded ingested_ref is a
// no-op (the branch's current content is already what was last ingested,
// e.g. a force-push back to a previously-ingested commit) and is skipped
// without calling Enqueue -- deliberately not leaning on ingest.Enqueue's
// own (repoID, targetBranch, kind) coalescing as the only thing preventing a
// redundant job here.
func (e *StoreIngestEnqueuer) EnqueueIngest(ctx context.Context, repo RepoID, advanced []Advance) (bool, error) {
	row, err := e.repos.GetRepoByName(ctx, string(repo))
	if err != nil {
		return false, fmt.Errorf("resolving repo %s for ingest enqueue: %w", repo, err)
	}
	var enqueued bool
	for _, adv := range advanced {
		if adv.Branch != row.IndexedBranch {
			continue
		}
		if adv.NewSHA == "" {
			continue
		}
		ingestedRef, err := e.targets.IngestedRef(ctx, row.ID, adv.Branch)
		if err != nil {
			return enqueued, fmt.Errorf("reading ingested ref for repo %s branch %s: %w", repo, adv.Branch, err)
		}
		kind := ingest.KindIncremental
		switch {
		case !ingestedRef.Ok:
			kind = ingest.KindFull
		case ingestedRef.Ref == adv.NewSHA:
			continue
		}
		if err := e.enqueuer.Enqueue(ctx, row.ID, adv.Branch, kind); err != nil {
			return enqueued, fmt.Errorf("enqueuing %s ingest for repo %s branch %s: %w", kind, repo, adv.Branch, err)
		}
		enqueued = true
	}
	return enqueued, nil
}
