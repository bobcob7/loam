package main

import "context"

// runner is a long-lived background component whose Run blocks until ctx
// is canceled and, per its own contract, every unit of work it already
// started has drained (see ingest.Pool.Run's doc comment: "blocks until
// ctx is canceled AND every in-flight job ... has drained"). serve treats
// every background component uniformly through this interface: start it
// in its own goroutine, then wait -- bounded by the shutdown grace period
// -- for that goroutine to return. *ingest.Pool satisfies this
// structurally today; *mirrorsync.Scheduler will too, once loam-ofg.21's
// sibling sync-epic beads land enough real collaborators to construct one
// (see run's doc comment) -- but Scheduler's Run does NOT itself drain
// in-flight cycles the way Pool's does, so a caller wiring it in here
// must also call its Shutdown method during shutdown, not rely on runner
// alone.
type runner interface {
	Run(ctx context.Context)
}

// closer releases a resource whose lifetime must outlive every runner's
// shutdown, since a still-draining job may still be querying it.
// *pgxpool.Pool satisfies this structurally. serve defers this call until
// after it has finished waiting on the background runner, so the pool is
// never closed out from under an in-flight query.
type closer interface {
	Close()
}
