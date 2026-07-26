// Package mirrorsync runs the Mirror Sync cycle (docs/sync-spec.md ->
// Mirror Sync) on a fixed interval, serialized per repo. The scheduler
// owns orchestration only: every step of the cycle is a separate
// collaborator reached through one of the small interfaces below, defined
// here at the consumer per repo convention. The trigger is injected as a
// channel so production wires a real time.Ticker while tests drive
// cycles with explicit, hand-written ticks (docs/testing-spec.md ->
// Manual scheduler).
package mirrorsync

import "context"

//go:generate go tool moq -out moq_test.go . RepoLister Fetcher AdvanceDetector MergeabilityChecker IngestEnqueuer PRPoller SyncStateReporter

// RepoID identifies an enrolled repo the scheduler cycles on each tick.
type RepoID string

// RepoLister supplies the set of enrolled repos to cycle on a tick. The
// scheduler re-lists on every tick, so enrollment changes take effect on
// the next cycle without a restart. No bead supplies this enumeration
// today; this is the scheduler's own narrow, read-only view of
// enrollment, owned wherever repo enrollment itself lands (loam-54o.7).
type RepoLister interface {
	ListRepos(ctx context.Context) ([]RepoID, error)
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
