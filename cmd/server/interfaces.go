package main

import (
	"context"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/reposstore"
)

//go:generate go tool moq -out moq_test.go . runner closer repoNameLister repoForgeLookup forgeCredentialLookup

// runner is a long-lived background component whose Run blocks until ctx
// is canceled and, per its own contract, every unit of work it already
// started has drained (see ingest.Pool.Run's doc comment: "blocks until
// ctx is canceled AND every in-flight job ... has drained"). serve treats
// every background component uniformly through this interface: start it
// in its own goroutine, then wait -- bounded by the shutdown grace period
// -- for that goroutine to return. *ingest.Pool satisfies this
// structurally.
//
// *mirrorsync.Scheduler does NOT satisfy it usefully on its own: its Run
// returns the instant ctx is canceled without draining the per-repo cycle
// goroutines it started, unlike Pool's. syncRunner (sync.go) is the value
// run() actually passes for the sync scheduler -- it pairs Scheduler.Run
// with Scheduler.Shutdown so this interface's drain half genuinely holds,
// and it is the only thing in this package that ever holds a reference to
// the Scheduler at all.
type runner interface {
	Run(ctx context.Context)
}

// closer releases a resource whose lifetime must outlive every runner's
// shutdown, since a still-draining job may still be querying it.
// *pgxpool.Pool satisfies this structurally. serve defers this call until
// after it has finished waiting on the background runner (bounded by the
// shutdown grace period), so the pool is never closed out from under an
// in-flight query THAT FINISHES WITHIN THAT WAIT. It offers no such
// guarantee once the grace period itself elapses: serve stops waiting
// either way (docs/server-spec.md -> Shutdown: work still running past
// the grace period is killed, not protected), so a runner still
// genuinely in flight at that point can observe db closing out from
// under it.
type closer interface {
	Close()
}

// repoNameLister supplies every enrolled repo's name for mirror
// reconciliation at startup (docs/server-spec.md Startup step 3).
// *reposstore.Store satisfies this structurally via its own
// ListAllRepoNames -- mirroring internal/mirrorsync's own identical
// unexported repoNameLister interface, defined separately here per this
// repo's "interfaces live where consumed" convention rather than shared
// across packages.
type repoNameLister interface {
	ListAllRepoNames(ctx context.Context) ([]string, error)
}

// repoForgeLookup resolves an enrolled repo's row -- forgePRTracker
// (sync.go) reads repos.forge_host from it to bind a forge client to the
// right instance. *reposstore.Store satisfies it structurally, mirroring
// internal/mirrorsync's own repoByNameLookup, which is the same one-method
// shape defined separately at its own consumer per this repo's convention.
type repoForgeLookup interface {
	GetRepoByName(ctx context.Context, name string) (reposstore.Repo, error)
}

// forgeCredentialLookup resolves a forge host's stored token, so
// forgePRTracker can authenticate the PR-state reads Mirror Sync step 5
// makes. *credentialstore.Store satisfies it structurally, and it is the
// same seam internal/gittransport already defines at its own consumer for
// the git half of the same credential.
type forgeCredentialLookup interface {
	GetByHost(ctx context.Context, host string) (credentialstore.Credential, error)
}
