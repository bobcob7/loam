package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
)

// member pairs a background runner with the human-readable identity
// multiRunner logs if that member's Run panics -- the runner interface
// (interfaces.go) exposes only Run, so there is no other source of a name
// to log.
type member struct {
	name string
	runner
}

// multiRunner composes several named runner members into one, so serve's
// single `background runner` parameter can start and drain more than one
// long-lived background component (docs/server-spec.md Process Model:
// the ingest worker pool AND the sync scheduler, both of which must be
// running before shutdown begins draining anything).
// Run starts every member's own Run concurrently and returns only once
// ALL of them have returned, preserving each member's individual
// contract (Run blocks until ctx is canceled and that member's own
// in-flight work has drained) without serve itself needing to know how
// many background components exist.
//
// Unlike internal/ingest.Pool's per-job recover (loam-337) or
// internal/mirrorsync.Scheduler's per-cycle recover (loam-lae,
// scheduler.go's recoverCyclePanic), a panic recovered here has no
// smaller unit of work to retry: it IS the member's Run, and Run is that
// member's whole lifetime. Recovering it therefore trades "the process
// dies loudly" for "this one background subsystem is now silently dead
// while the process keeps serving reads" -- a real trade-off, not a pure
// win (loam-lae's bead notes this explicitly). It is still strictly
// better than the alternative -- e.g. a panicking sync scheduler taking
// down the HTTP listener and the still-healthy ingest pool along with it
// -- but it means recoverMember MUST log loudly enough that "this member
// is silently dead" is discoverable from logs alone, since nothing else
// in this binary will ever notice: docs/server-spec.md's own /readyz
// deliberately excludes the ingest pool and sync scheduler from
// readiness (internal/health/health.go's doc comment), so no load
// balancer or orchestrator gets a signal from this either.
type multiRunner struct {
	logger  *slog.Logger
	members []member
}

// newMultiRunner builds a multiRunner. logger is used only if a member's
// Run panics -- see multiRunner's own doc comment for why nothing else in
// this binary would otherwise notice.
func newMultiRunner(logger *slog.Logger, members ...member) multiRunner {
	return multiRunner{logger: logger, members: members}
}

// Run implements runner.
func (m multiRunner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, mem := range m.members {
		wg.Add(1)
		go func(mem member) {
			defer wg.Done()
			defer m.recoverMember(mem.name)
			mem.Run(ctx)
		}(mem)
	}
	wg.Wait()
}

// recoverMember is Run's per-member panic boundary. It never re-panics --
// a silent recover would be strictly worse than the crash it replaces
// (loam-lae's bead: "A silent recover is strictly worse than the panic")
// -- every recovered panic is logged at ERROR with the member's name, the
// recovered value, and a stack trace, matching internal/ingest/pool.go's
// runOrchestrator/recoverOutcomeRecording and
// internal/mirrorsync/scheduler.go's recoverCyclePanic shape.
//
// What an operator observes when this fires: one ERROR log line naming
// the dead member, its panic value, and a stack trace -- nothing else;
// there is no metric, no /readyz change, no process exit. That member's
// subsystem then simply goes quiet for the rest of the process's life: a
// dead "ingest pool" stops claiming ingest_jobs rows (they accumulate in
// 'queued' with nothing ever running them); a dead "sync scheduler" stops
// ticking entirely (repos.sync_state and last_synced_at stop advancing
// for every enrolled repo, not just one). The HTTP listener and any other
// still-alive member keep serving normally throughout, which is exactly
// what makes this silent rather than loud: an operator who is not
// watching logs, or has no alert on this line, has no other way to find
// out that a whole background subsystem died.
func (m multiRunner) recoverMember(name string) {
	r := recover()
	if r == nil {
		return
	}
	m.logger.Error("background runner member panicked and has permanently stopped; the process keeps serving reads but this subsystem is now silently dead",
		"member", name, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
}
