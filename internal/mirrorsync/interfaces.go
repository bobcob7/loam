// Package mirrorsync runs the Mirror Sync cycle (docs/sync-spec.md ->
// Mirror Sync) on a fixed interval, serialized per repo. The scheduler
// owns orchestration only: every step of the cycle is a separate
// collaborator reached through one of the small interfaces below, defined
// here at the consumer per repo convention. The trigger is injected as a
// channel so production wires a real time.Ticker while tests drive
// cycles with explicit, hand-written ticks (docs/testing-spec.md ->
// Manual scheduler).
package mirrorsync

import (
	"context"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

//go:generate go tool moq -out moq_test.go . RepoLister Fetcher AdvanceDetector MergeabilityChecker IngestEnqueuer PRPoller SyncStateReporter repoNameLister upstreamRefFetcher repoResolver repoByNameLookup workBranchNameLister targetBranchLister ingestedRefLookup ingestJobEnqueuer mergeTreeRunner workBranchConflictMarker workBranchTerminator pullRequestTracker upstreamRefDeleter

// RepoID identifies an enrolled repo the scheduler cycles on each tick. It
// is repos.name (an "<group>/<repo_name>" string), not repos.id -- the
// settled RepoID decision (loam-54o.7 NOTES): the proto surface never
// exposes repos.id to any client, and every Mirror Sync collaborator here
// operates on the repo's mirror path or the forge, both keyed by name, not
// by the FK id. Where a collaborator needs the id (an FK join, e.g.
// internal/ingest.Enqueuer), it resolves this name to repos.id itself via
// a single indexed lookup on repos_name_key UNIQUE (name)
// (internal/reposstore.Store.GetRepoByName) -- this package never carries
// the id.
type RepoID string

// RepoLister supplies the set of enrolled repos to cycle on a tick. The
// scheduler re-lists on every tick, so enrollment changes take effect on
// the next cycle without a restart.
//
// internal/reposstore (loam-54o.7) owns repo enrollment but exposes only
// a paginated page of full Repo rows plus a total count
// (Store.ListRepos(ctx, Page) (ListReposResult, error), where
// ListReposResult is {Repos []Repo; Total int}), not the bare []RepoID
// this interface needs -- both the pagination parameter and the return
// type differ, not just the element type -- so Store itself never
// satisfies RepoLister. loam-13z resolved the gap not with a paging
// adapter but with a second, unpaginated query,
// Store.ListAllRepoNames(ctx) ([]string, error): the scheduler enumerates
// *every* enrolled repo on *every* tick (docs/sync-spec.md -> Mirror
// Sync), a bulk-read access pattern LIMIT/OFFSET does not fit -- offset
// pagination exists for the admin API's list view, a human paging a
// bounded screen, not a producer read wholesale on a fixed interval. A
// RepoID is a short string, so even a deployment enrolling in the tens of
// thousands of repos holds a trivial amount of memory this way, and the
// scheduler needs the entire enrollment in hand before it starts
// per-repo cycles regardless -- paging it in would only add round trips
// with no bound this caller could act on early. StoreRepoLister (this
// package) adapts Store.ListAllRepoNames to this interface, converting
// each returned name to a RepoID (RepoID is repos.name, never repos.id --
// loam-54o.7 NOTES). No production Scheduler is constructed anywhere in
// the tree yet: loam-ofg.21 wires cmd/server/main.go's startup sequence --
// it connects to Postgres, in the correct migrate-then-pool order, and
// constructs a real ingest worker pool -- but deliberately stopped short
// of building a Scheduler while most of this package's collaborators had
// no production implementation. As of loam-giq.5 exactly ONE of the
// scheduler's 7 collaborators still lacks one: PRPoller (loam-giq.8).
// Fetcher is MirrorFetcher (giq.2), AdvanceDetector is
// StoreAdvanceDetector (giq.4), MergeabilityChecker is
// StoreMergeabilityChecker (giq.5), IngestEnqueuer is StoreIngestEnqueuer
// (c94.2), and RepoLister/SyncStateReporter are this package's own
// StoreRepoLister and internal/mirrorsync/state -- all real. So
// StoreRepoLister still has no call site to wire into today; it remains
// ready for whichever bead constructs the last collaborator (giq.8) to
// pass to mirrorsync.New as the RepoLister argument, alongside
// Scheduler.Shutdown (added by loam-ofg.21) as the drain seam its own
// shutdown sequence needs.
type RepoLister interface {
	ListRepos(ctx context.Context) ([]RepoID, error)
}

// repoNameLister is the reposstore.Store surface StoreRepoLister adapts to
// RepoLister, defined here at the consumer per package convention. The
// production implementation is *reposstore.Store itself
// (Store.ListAllRepoNames), which satisfies this interface structurally.
type repoNameLister interface {
	ListAllRepoNames(ctx context.Context) ([]string, error)
}

// RefUpdate is one ref's SHA transition observed by a single fetch, as
// `git fetch --porcelain` reports it. OldSHA is the empty string when the
// ref did not exist in the mirror before this fetch (a brand-new branch,
// including a repo's very first sync).
type RefUpdate struct {
	Ref    string
	OldSHA string
	NewSHA string
}

// FetchResult carries every ref update a fetch produced. AdvanceDetector
// needs this, not just a bare error: step 2 detects advances by comparing
// SHAs from before and after the fetch (docs/sync-spec.md -> Mirror
// Sync, step 2), a comparison only the fetch itself can make — a
// Fetch(ctx, repo) error with no ref deltas would leave AdvanceDetector
// no before-state to compare against.
type FetchResult struct {
	Refs []RefUpdate
}

// Fetcher performs Mirror Sync step 1: a forced fetch of all upstream
// refs into repo's mirror, pruning deleted branches, upstream-wins,
// excluding registered work-branch refs from the refspec (docs/sync-spec.md
// -> Mirror Sync, step 1; owned by bead giq.2).
type Fetcher interface {
	Fetch(ctx context.Context, repo RepoID) (FetchResult, error)
}

// Advance is one target branch's SHA transition, handed from
// AdvanceDetector to MergeabilityChecker and IngestEnqueuer as a pure
// signal so neither has to re-resolve the tip itself (docs/sync-spec.md
// -> Mirror Sync, steps 2-4; loam-c94.2 DESIGN: "loam-giq.4 owns
// SHA-before/after comparison and hands the advanced ref pair to this
// bead purely as a signal (old_ref, new_ref)"). OldSHA is empty for a
// branch with no prior recorded tip (e.g. first enrollment); whether
// that counts as an "advance" for ingest purposes is for giq.4/c94.2 to
// decide — this package only carries the signal.
type Advance struct {
	Branch string
	OldSHA string
	NewSHA string
}

// AdvanceDetector performs Mirror Sync step 2: compares SHAs before and
// after the fetch and reports which target branches (listed targets, plus
// any branch that is the recorded target of an open work branch) advanced
// (docs/sync-spec.md -> Mirror Sync, step 2; owned by bead giq.4).
type AdvanceDetector interface {
	DetectAdvances(ctx context.Context, repo RepoID, fetched FetchResult) (advanced []Advance, err error)
}

// MergeabilityChecker performs Mirror Sync step 3: for every advanced
// target, tests open work branches targeting it against the new tip
// (docs/sync-spec.md -> Mergeability Check; owned by bead giq.5).
type MergeabilityChecker interface {
	CheckMergeability(ctx context.Context, repo RepoID, advanced []Advance) error
}

// IngestEnqueuer performs Mirror Sync step 4: enqueues ingest jobs for
// advanced indexed branches (docs/ingestion-spec.md; owned by bead
// loam-c94.2).
//
// enqueued reports whether ownership of this tick's terminal sync_state
// write actually passed to the ingest worker -- it is NOT "did step 4
// return without error" (loam-ax1: that conflation is exactly the bug this
// return value fixes, since a no-op call -- e.g. advanced is empty, so
// there is nothing to enqueue -- previously still surfaced as "true" and
// permanently starved ReportIdle/ReportError). enqueued must be true
// whenever, after this call returns, at least one ingest_jobs row is
// queued or running for repo as a result of an advanced branch -- whether
// this call inserted that row itself, or the row already existed and this
// call's trigger coalesced into it (internal/ingest's Enqueue: "if a
// queued job already exists for (repoID, targetBranch, kind), this call is
// a no-op"). A coalesced trigger still counts as enqueued=true: the
// existing row is still pending (coalescing only matches status='queued'
// rows, per ingest.Enqueue's doc comment), so the ingest worker still owns
// the eventual write for that branch regardless of which call created the
// row, and letting ReportIdle/ReportError fire here would race that
// worker's write over the same column exactly as the guard in
// internal/mirrorsync/state is built to prevent. enqueued is false only
// when this call queued or touched nothing at all -- advanced was empty,
// or none of its branches are indexed branches ingest.Enqueue is called
// for.
//
// enqueued and err are independent, not mutually exclusive: advanced is a
// slice, so an implementation that enqueues branch A and then fails on
// branch B must return (true, err), not (false, err). The scheduler
// propagates whatever this method returns verbatim on both the success and
// error paths -- it never coerces enqueued to false just because err is
// non-nil -- specifically so a real error partway through a multi-branch
// call cannot silently drop an ownership hand-off that already happened
// for an earlier branch and let ReportError clobber that branch's row.
type IngestEnqueuer interface {
	EnqueueIngest(ctx context.Context, repo RepoID, advanced []Advance) (enqueued bool, err error)
}

// PRPoller performs Mirror Sync step 5: polls the forge for the state of
// work branches with a recorded, still-open PR (docs/sync-spec.md -> PR
// State Tracking; owned by bead giq.8).
type PRPoller interface {
	PollPRs(ctx context.Context, repo RepoID) error
}

// SyncStateReporter surfaces the outcome of a repo's cycle so it can be
// persisted as repos.sync_state (docs/sync-spec.md -> Mirror Sync; owned
// by bead giq.9). The scheduler never persists state itself: it reports
// syncing when a cycle starts, idle when all 5 steps complete, and error
// with the failing step's error when a step aborts the cycle early. The
// tick interval is the retry backoff, so a repo that errors is simply
// cycled again, from step 1, on the next tick.
//
// enqueuedIngest on ReportIdle and ReportError is the ownership hand-off
// signal loam-giq.9's DESIGN requires: "once step 4 enqueues a job,
// ownership of sync_state for that tick passes to the ingest worker
// until the job resolves; this bead's own idle/error write covers only
// the non-ingest steps (1,2,3,5)". Without it, giq.9 cannot tell whether
// its own write would race loam-c94.13's. It is carried on both methods
// because step 4 can succeed (handing off ownership) and step 5 can
// still fail afterward.
//
// All three methods return an error so a reporting failure can be logged
// with repo context rather than swallowed. The scheduler never retries
// or aborts on a report failure: by the time a report is sent the cycle
// itself has already finished, successfully or not.
type SyncStateReporter interface {
	ReportSyncing(ctx context.Context, repo RepoID) error
	ReportIdle(ctx context.Context, repo RepoID, enqueuedIngest bool) error
	ReportError(ctx context.Context, repo RepoID, err error, enqueuedIngest bool) error
}

// upstreamRefFetcher is the gittransport.Transport surface MirrorFetcher
// runs the actual fetch subprocess through, defined here at the consumer.
// *gittransport.Transport satisfies it structurally; loam-giq.3 owns
// credential injection, config isolation, and secret scrubbing behind this
// one call -- MirrorFetcher never shells out to git itself.
type upstreamRefFetcher interface {
	Fetch(ctx context.Context, host, mirrorDir, upstreamURL string, refspecs []string) ([]byte, error)
}

// repoResolver resolves everything MirrorFetcher needs to run one repo's
// upstream fetch: the forge host backing credential resolution, the
// upstream clone URL, and the bare names of every currently registered
// work-branch ref to exclude from the refspec (docs/sync-spec.md -> Mirror
// Sync step 1; docs/git-spec.md -> Ref Policy "Work-branch refs"). Defined
// here at the consumer so MirrorFetcher never imports reposstore or
// workbranchstore directly; StoreRepoResolver (this package) is the
// production adapter joining both stores. workBranchNames are bare (e.g.
// "wb-9c2f1a"), not full refs/heads/... paths -- MirrorFetcher owns
// building the ref path from each name.
type repoResolver interface {
	ResolveRepo(ctx context.Context, repo RepoID) (host, upstreamURL string, workBranchNames []string, err error)
}

// repoByNameLookup is the reposstore.Store surface StoreRepoResolver uses
// to resolve a repo's upstream fetch coordinates, defined here at the
// consumer. *reposstore.Store satisfies it structurally.
type repoByNameLookup interface {
	GetRepoByName(ctx context.Context, name string) (reposstore.Repo, error)
}

// workBranchNameLister is the workbranchstore.Store surface
// StoreRepoResolver uses to enumerate a repo's currently registered
// work-branch names, defined here at the consumer. *workbranchstore.Store
// satisfies it structurally.
//
// StoreAdvanceDetector uses this same surface for a different projection:
// not every registered branch's Name (StoreRepoResolver's use), but the
// Target of every non-terminal one (docs/sync-spec.md -> Mirror Sync step
// 2's set (b), "any branch that is the recorded target of an open work
// branch"). One List method serves both, since workbranchstore.WorkBranch
// carries every field either caller needs.
type workBranchNameLister interface {
	List(ctx context.Context, filter workbranchstore.ListFilter, limit, offset int32) ([]workbranchstore.WorkBranch, int64, error)
}

// targetBranchLister is the reposstore.Store surface StoreAdvanceDetector
// uses to enumerate repo_target_branches -- set (a), the listed target
// branches (docs/sync-spec.md -> Mirror Sync step 2) -- defined here at
// the consumer. *reposstore.Store satisfies it structurally.
type targetBranchLister interface {
	ListTargetBranches(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error)
}

// ingestedRefLookup is the reposstore.Store surface StoreIngestEnqueuer uses
// to read the incremental-ingest diff base recorded for a repo's indexed
// branch (repo_target_branches.ingested_ref) before deciding whether, and
// with what Kind, to enqueue a job -- defined here at the consumer.
// *reposstore.Store satisfies it structurally. A NULL column (IngestedRef.Ok
// false) is the "never ingested this branch" signal StoreIngestEnqueuer
// reads as first enrollment (docs/persistence-spec.md "repo_target_branches":
// "null until first ingest"); a wrapped reposstore.ErrNotFound means branch
// is not enrolled as a target for repoID at all, which StoreIngestEnqueuer
// treats as a hard error rather than silently coercing into "first
// enrollment" -- repos.indexed_branch is validated to always be one of a
// repo's target branches at enrollment (RepoAdminService.EnrollRepo) and
// whenever it is changed, so a missing row here means that invariant broke,
// not that the branch is new.
type ingestedRefLookup interface {
	IngestedRef(ctx context.Context, repoID uuid.UUID, branch string) (reposstore.IngestedRef, error)
}

// ingestJobEnqueuer is the ingest.Enqueuer surface StoreIngestEnqueuer calls
// to queue a job, defined here at the consumer per this package's own
// convention (every other collaborator below gets the same treatment) even
// though ingest.Enqueuer (internal/ingest/interfaces.go) already declares
// this identical one-method shape for exactly this cross-package use --
// *ingest.Pool satisfies both structurally. Enqueue never takes a ref pair:
// ingest_jobs has no old/new ref columns, so the diff base
// (repo_target_branches.ingested_ref) and new tip (the live mirror) are the
// Orchestrator/diffplan.Planner's responsibility to resolve at run time from
// RepoID+TargetBranch (internal/ingest.Job's own doc comment), never this
// package's to pass through -- see StoreIngestEnqueuer's doc comment for
// what that means for where the incremental-vs-full decision actually
// lives.
type ingestJobEnqueuer interface {
	Enqueue(ctx context.Context, repoID uuid.UUID, targetBranch string, kind ingest.Kind) error
}

// mergeTreeRunner answers the one git question StoreMergeabilityChecker
// asks -- would this work branch still merge into that target tip? --
// defined here at the consumer. *gitmergetree.Checker satisfies it
// structurally; StoreMergeabilityChecker never shells out to git itself,
// the same split MirrorFetcher keeps with gittransport.
//
// conflicted is meaningful ONLY when err is nil. A check that could not be
// performed at all -- the work-branch ref missing from the mirror,
// histories with no common ancestor, a corrupt mirror, a canceled context
// -- must come back as a non-nil error, never as conflicted=true, because
// the two outcomes have opposite consequences: a true conflict demotes a
// reviewable work branch to draft and voids its verdicts, while a failed
// check must leave every work branch untouched and abort the repo's cycle
// (git-merge-tree(1) makes this easy to get wrong -- it reports an
// unresolvable ref with the SAME exit status as a conflict; see
// internal/gitmergetree's package doc comment for the measured table).
type mergeTreeRunner interface {
	MergeTree(ctx context.Context, mirrorDir, ours, theirs string) (conflicted bool, err error)
}

// workBranchConflictMarker is the workbranchstore.Store surface
// StoreMergeabilityChecker writes a conflict verdict through, defined here
// at the consumer. *workbranchstore.Store satisfies it structurally.
//
// MarkConflicted alone is the whole write surface this bead needs, because
// the draft-vs-demotion decision docs/sync-spec.md's Mergeability Check
// describes ("the branch is marked conflicted; if it was reviewable or
// reviewed it is reset to draft with conflict_reset recorded") lives
// inside that one guarded UPDATE, not here -- see
// MarkWorkBranchConflicted in internal/db/queries/work_branches.sql. It is
// idempotent and level-triggered by design, matching how this checker
// calls it: every open work branch is re-evaluated on every advance of its
// target, so the same branch can be found still-conflicting many ticks in
// a row.
//
// There is deliberately no clearing counterpart here. A flagged branch
// recovers by PUSH, not by a later clean check: docs/sync-spec.md's clean
// case is "nothing happens", and clearing is catch-up detection's job on
// an accepted push (loam-giq.6), which is also what re-opens a review
// round. Handing this checker a ClearConflict seam would let a target
// advance that happens to merge cleanly silently restore a demoted branch
// to reviewable with no push and no fresh round -- see
// StoreMergeabilityChecker's doc comment.
type workBranchConflictMarker interface {
	MarkConflicted(ctx context.Context, id uuid.UUID) (workbranchstore.WorkBranch, error)
}

// workBranchTerminator is the workbranchstore.Store surface StorePRPoller
// drives a work branch into one of its two terminal states with, defined
// here at the consumer. *workbranchstore.Store satisfies it structurally.
// Both methods are guarded, single-statement UPDATEs on the store side, so
// a branch another actor already terminated surfaces as
// workbranchstore.ErrIllegalTransition rather than as a silent second
// transition -- which is what makes StorePRPoller's per-tick re-poll
// idempotent without this package holding a lock or reading back state.
type workBranchTerminator interface {
	Complete(ctx context.Context, id uuid.UUID) (workbranchstore.WorkBranch, error)
	Close(ctx context.Context, id uuid.UUID, reason string) (workbranchstore.WorkBranch, error)
}

// pullRequestTracker is the forge.Provider surface StorePRPoller reads PR
// state through, and closes a PR with on the admin-close path, defined here
// at the consumer. forge.Provider itself (and *fakeforge.Client) satisfies
// it structurally. GetPRState returns a bare state string, not a typed
// enum: StorePRPoller validates it against the three states the Provider
// contract documents ("open", "merged", "closed") and treats anything else
// as unknown-and-non-destructive rather than trusting it.
type pullRequestTracker interface {
	GetPRState(ctx context.Context, repo string, prNumber int) (state string, err error)
	ClosePR(ctx context.Context, repo string, prNumber int) error
}

// upstreamRefDeleter is the gittransport.Transport surface StorePRPoller
// runs upstream branch cleanup through, defined here at the consumer.
// *gittransport.Transport satisfies it structurally (Transport.DeleteRemoteRef
// exists for exactly this call site). It is deliberately NOT folded into
// upstreamRefFetcher above: MirrorFetcher must never gain the ability to
// delete an upstream ref just because it shares a transport with this
// poller.
type upstreamRefDeleter interface {
	DeleteRemoteRef(ctx context.Context, host, mirrorDir, upstreamURL, ref string) ([]byte, error)
}
