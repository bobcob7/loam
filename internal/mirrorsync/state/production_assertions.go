package state

import "github.com/bobcob7/loam/internal/mirrorsync"

// Reporter is the production mirrorsync.SyncStateReporter, the seventh of
// the Scheduler's seven required collaborators. The other six are asserted
// in internal/mirrorsync/production_assertions.go; this one lives here
// because that package cannot import this one without a cycle.
//
// It is worth pinning specifically. mirrorsync's RepoLister doc comment
// once carried a prose inventory of which collaborators had production
// implementations, and that inventory was misread as saying this one did
// not -- it has existed since loam-ofg.21. A compile-time assertion cannot
// go stale the way the sentence did.
var _ mirrorsync.SyncStateReporter = (*Reporter)(nil)
