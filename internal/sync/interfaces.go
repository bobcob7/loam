// Package sync runs the Mirror Sync cycle (docs/sync-spec.md -> Mirror
// Sync) on a fixed interval, serialized per repo. The scheduler owns
// orchestration only: every step of the cycle is a separate collaborator
// reached through one of the small interfaces below, defined here at the
// consumer per repo convention. The trigger is injected as a channel so
// production wires a real time.Ticker while tests drive cycles with
// explicit, hand-written ticks (docs/testing-spec.md -> Manual scheduler).
package sync

import "context"

//go:generate go tool moq -out moq_test.go . RepoLister Fetcher AdvanceDetector MergeabilityChecker IngestEnqueuer PRPoller SyncStateReporter

// RepoID identifies an enrolled repo the scheduler cycles on each tick.
type RepoID string

// RepoLister supplies the set of enrolled repos to cycle on a tick. The
// scheduler re-lists on every tick, so enrollment changes take effect on
// the next cycle without a restart.
type RepoLister interface {
	ListRepos(ctx context.Context) ([]RepoID, error)
}

// Fetcher performs Mirror Sync step 1: a forced fetch of all upstream
// refs into repo's mirror, pruning deleted branches, upstream-wins,
// excluding registered work-branch refs from the refspec (docs/sync-spec.md
// -> Mirror Sync, step 1; owned by bead giq.2).
type Fetcher interface {
	Fetch(ctx context.Context, repo RepoID) error
}

// AdvanceDetector performs Mirror Sync step 2: compares SHAs before and
// after the fetch and reports which target branches (listed targets, plus
// any branch that is the recorded target of an open work branch) advanced
// (docs/sync-spec.md -> Mirror Sync, step 2; owned by bead giq.4).
type AdvanceDetector interface {
	DetectAdvances(ctx context.Context, repo RepoID) (advanced []string, err error)
}

// MergeabilityChecker performs Mirror Sync step 3: for every advanced
// target, tests open work branches targeting it against the new tip
// (docs/sync-spec.md -> Mergeability Check; owned by bead giq.5).
type MergeabilityChecker interface {
	CheckMergeability(ctx context.Context, repo RepoID, advanced []string) error
}

// IngestEnqueuer performs Mirror Sync step 4: enqueues ingest jobs for
// advanced indexed branches (docs/ingestion-spec.md; owned by bead
// loam-c94.2).
type IngestEnqueuer interface {
	EnqueueIngest(ctx context.Context, repo RepoID, advanced []string) error
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
type SyncStateReporter interface {
	ReportSyncing(ctx context.Context, repo RepoID)
	ReportIdle(ctx context.Context, repo RepoID)
	ReportError(ctx context.Context, repo RepoID, err error)
}
