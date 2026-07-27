package main

import (
	"context"
	"sync"
)

// multiRunner composes several runner values into one, so serve's single
// `background runner` parameter can start and drain more than one
// long-lived background component (docs/server-spec.md Process Model:
// the ingest worker pool AND the policy socket's own accept loop, both of
// which must be running before shutdown begins draining anything).
// Run starts every member's own Run concurrently and returns only once
// ALL of them have returned, preserving each member's individual
// contract (Run blocks until ctx is canceled and that member's own
// in-flight work has drained) without serve itself needing to know how
// many background components exist.
type multiRunner []runner

// Run implements runner.
func (m multiRunner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, r := range m {
		wg.Add(1)
		go func(r runner) {
			defer wg.Done()
			r.Run(ctx)
		}(r)
	}
	wg.Wait()
}
