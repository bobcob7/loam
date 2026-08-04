package mirrorsync

// The Scheduler takes all eight of its collaborators as required
// parameters, so it is constructible only when every one of them has a
// production implementation. Tracking which ones did was previously done
// in a prose inventory inside interfaces.go's RepoLister doc comment, and
// that inventory went stale by construction: every bead that landed
// invalidated it, and nothing failed when it did. Three separate readers
// drew a wrong conclusion from it in a single session.
//
// These assertions are the same statement in a form the compiler checks.
// If a production type stops satisfying its seam -- or a seam grows a
// method its implementation lacks -- this file fails to build, loudly, at
// the exact commit that broke it. A comment cannot do that.
//
// Seven of the eight are asserted here. The eighth, SyncStateReporter, is
// asserted in internal/mirrorsync/state instead: that package imports this
// one, so naming *state.Reporter here would be an import cycle. See
// production_assertions.go there -- and note that SyncStateReporter is
// exactly the seam the stale comment was once misread as lacking an
// implementation, so it is the one most worth pinning.
var (
	_ RepoLister          = (*StoreRepoLister)(nil)
	_ Fetcher             = (*MirrorFetcher)(nil)
	_ AdvanceDetector     = (*StoreAdvanceDetector)(nil)
	_ MergeabilityChecker = (*StoreMergeabilityChecker)(nil)
	_ IngestEnqueuer      = (*StoreIngestEnqueuer)(nil)
	_ PRPoller            = (*StorePRPoller)(nil)
	_ DriftReconciler     = (*StoreDriftReconciler)(nil)
)
