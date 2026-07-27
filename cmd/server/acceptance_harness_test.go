//go:build acceptance

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/cucumber/godog"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/gittransport"
	"github.com/bobcob7/loam/internal/mirrorreconcile"
	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/mirrorsync/state"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/testsched"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// acceptanceHarness is the fixed, whole-suite context every step definition
// closes over: the live in-process server, the CLI binary path, the shared
// fakeforge instance, and the two testsched wrappers backing the "the next
// sync runs"/"the upstream PR merges" and "after ingestion" step-vocabulary
// rows. Constructed once by newAcceptanceHarness; per-scenario mutable
// state lives in acceptanceWorld (acceptance_world_test.go), never here.
type acceptanceHarness struct {
	t             *testing.T
	server        acceptanceServer
	forge         *fakeforge.Server
	loamBinary    string
	adminUser     string
	adminPassword string
	syncHarness   *testsched.SyncHarness
	ingestHarness *testsched.IngestHarness
}

// newAcceptanceHarness assembles the fixed, whole-suite acceptanceHarness.
func newAcceptanceHarness(t *testing.T, srv acceptanceServer, forge *fakeforge.Server, cfg config.Config) *acceptanceHarness {
	t.Helper()
	return &acceptanceHarness{
		t:             t,
		server:        srv,
		forge:         forge,
		loamBinary:    acceptanceLoamBinary,
		adminUser:     cfg.AdminUser,
		adminPassword: cfg.AdminPassword,
		syncHarness:   newSyncHarness(srv, forge),
		ingestHarness: testsched.NewIngestHarness(srv.ingestPool),
	}
}

// errAdvanceDetectorNotImplemented, errMergeabilityCheckerNotImplemented,
// errIngestEnqueuerNotImplemented, and errPRPollerNotImplemented mirror
// cmd/server/main.go's own notImplementedOrchestrator/DiffComputer/
// RepoDeleter idiom: a labeled error standing in for a collaborator with
// no production implementation anywhere in the tree yet (loam-giq.4,
// loam-giq.5, loam-c94.2, loam-giq.8, respectively -- all still open),
// rather than a silent no-op that would misrepresent "the next sync runs"
// as having actually detected, merge-checked, enqueued, or polled
// anything.
//
// This is loud FROM THE SCHEDULER's own point of view only:
// mirrorsync.Scheduler's cycle logs each of these and writes
// repos.sync_state='error' (scheduler.go), but never returns them through
// Scheduler.Tick -- Tick's own error return is exclusively a ListRepos
// failure (its doc comment). A caller that only checked Tick's return
// value would see a nil error and nothing else, which is why
// stepTheNextSyncRuns (acceptance_steps_test.go) additionally reads
// repos.sync_state back after every tick and fails the step if it is
// 'error' -- these vars alone are not "loud" to a godog step, only to the
// database column the scheduler itself writes.
//
// No scenario in this suite's default (@wip-filtered) run exercises these
// today; they exist so the step vocabulary row itself is wired and
// "resolvable" (loam-li0.5's own scope), ready for each collaborator bead
// to swap in its real implementation here with no other harness change.
var (
	errAdvanceDetectorNotImplemented     = errors.New("acceptance harness: AdvanceDetector not implemented (loam-giq.4)")
	errMergeabilityCheckerNotImplemented = errors.New("acceptance harness: MergeabilityChecker not implemented (loam-giq.5)")
	errIngestEnqueuerNotImplemented      = errors.New("acceptance harness: IngestEnqueuer not implemented (loam-c94.2)")
	errPRPollerNotImplemented            = errors.New("acceptance harness: PRPoller not implemented (loam-giq.8)")
)

// acceptanceAdvanceDetector, acceptanceMergeabilityChecker,
// acceptanceIngestEnqueuer, and acceptancePRPoller are the harness's own
// not-implemented stand-ins for the four mirrorsync collaborators that
// have no production implementation anywhere in the tree yet (see the
// error vars' doc comment above). Each is a zero-field type so
// newSyncHarness can construct one inline with no further wiring.
type acceptanceAdvanceDetector struct{}

// DetectAdvances implements mirrorsync.AdvanceDetector.
func (acceptanceAdvanceDetector) DetectAdvances(_ context.Context, _ mirrorsync.RepoID, _ mirrorsync.FetchResult) ([]mirrorsync.Advance, error) {
	return nil, errAdvanceDetectorNotImplemented
}

type acceptanceMergeabilityChecker struct{}

// CheckMergeability implements mirrorsync.MergeabilityChecker.
func (acceptanceMergeabilityChecker) CheckMergeability(_ context.Context, _ mirrorsync.RepoID, _ []mirrorsync.Advance) error {
	return errMergeabilityCheckerNotImplemented
}

type acceptanceIngestEnqueuer struct{}

// EnqueueIngest implements mirrorsync.IngestEnqueuer.
func (acceptanceIngestEnqueuer) EnqueueIngest(_ context.Context, _ mirrorsync.RepoID, _ []mirrorsync.Advance) (bool, error) {
	return false, errIngestEnqueuerNotImplemented
}

type acceptancePRPoller struct{}

// PollPRs implements mirrorsync.PRPoller.
func (acceptancePRPoller) PollPRs(_ context.Context, _ mirrorsync.RepoID) error {
	return errPRPollerNotImplemented
}

// staticTokenCredentialSource is a minimal credentialSource (gittransport's
// consumer-defined interface) returning the same fixed token for every
// host, mirroring internal/mirrorsync/fetcher_gittransport_test.go's own
// staticCredentialSource -- reproduced here rather than imported, since
// that type lives in a package's _test.go file and is not reachable
// outside it.
type staticTokenCredentialSource struct {
	token string
}

// GetByHost implements gittransport's credentialSource.
func (s staticTokenCredentialSource) GetByHost(context.Context, string) (credentialstore.Credential, error) {
	return credentialstore.Credential{Token: s.token}, nil
}

// acceptanceForgeToken is the fixed fake-forge token every repo this suite
// seeds is reachable with -- registered once against the shared fakeforge
// instance in newSyncHarness, below.
const acceptanceForgeToken = "loam-acceptance-static-token"

// newSyncHarness builds the harness's OWN *mirrorsync.Scheduler --
// wired over the SAME live pool run() constructed (via real
// reposstore/workbranchstore-backed collaborators, so it reads whatever
// this scenario's own SQL fixtures just seeded) and a real MirrorFetcher
// pointed at the shared fakeforge instance -- and immediately wraps it in
// a *testsched.SyncHarness, which is ALL this function returns.
//
// This is loam-f75's constraint ("never call Scheduler.Run and
// Scheduler.Tick on the same Scheduler") satisfied by construction, not by
// convention: the *mirrorsync.Scheduler value itself is a local variable
// of this one function and is never returned, stored on acceptanceHarness,
// or passed to any other function. Nothing outside this function ever
// holds a reference capable of calling its Run method at all -- there is
// no comment to violate, because there is no reachable Scheduler to call
// Run on in the first place. Every step definition and every other file in
// this suite only ever sees the returned *testsched.SyncHarness, whose own
// type has no Run method (internal/testsched/sync.go).
//
// production has already reached the same conclusion from the other
// direction: cmd/server/main.go's run() does not construct a Scheduler at
// all today (see its own doc comment), so there is no risk of this
// harness's Tick calls ever racing a production goroutine's Run -- the two
// Schedulers are not just logically separate, one of them does not exist.
func newSyncHarness(srv acceptanceServer, forge *fakeforge.Server) *testsched.SyncHarness {
	forge.AddToken(acceptanceForgeToken)
	repoStore := reposstore.NewStore(gen.New(srv.pool), acceptanceLogger())
	workBranchStore := workbranchstore.New(gen.New(srv.pool), acceptanceLogger())
	resolver := mirrorsync.NewStoreRepoResolver(repoStore, workBranchStore)
	transport := gittransport.New(staticTokenCredentialSource{token: acceptanceForgeToken}, fakeforge.NewClient("", ""), acceptanceLogger())
	fetcher := mirrorsync.NewMirrorFetcher(srv.dataDir, transport, resolver)
	reporter := state.New(srv.pool)
	scheduler := mirrorsync.New(acceptanceLogger(), nil, mirrorsync.NewStoreRepoLister(repoStore), fetcher, acceptanceAdvanceDetector{}, acceptanceMergeabilityChecker{}, acceptanceIngestEnqueuer{}, acceptancePRPoller{}, reporter)
	return testsched.NewSyncHarness(scheduler)
}

// reconcileSeededMirror installs the real, compiled loamhook binary as
// mirrorDir's pre-receive hook and sets receive.denyNonFastForwards/
// denyDeletes, exactly as cmd/server/main.go's Startup step 3 does for
// every mirror already on disk when the server boots (docs/server-spec.md).
// Every acceptance scenario that seeds a repo mid-suite (after this
// package's one shared server has already finished booting) must call this
// itself: Startup's own one-time reconciliation loop ran once, before any
// scenario's fixture existed, so nothing else in this binary ever
// reconciles a mirror seeded afterward. Without it, a push against the
// freshly seeded mirror would hit no pre-receive hook at all -- no policy
// enforcement, not even the read-only-target-branch or force-push checks
// -- which is the wrong fixture for a scenario that means to observe a
// real rejection.
func (h *acceptanceHarness) reconcileSeededMirror(ctx context.Context, mirrorDir string) error {
	return mirrorreconcile.ReconcileMirror(ctx, mirrorDir, h.server.hookBinaryPath)
}

// initializeScenario wires every step definition and the per-scenario
// Before/After hooks (acceptance_world_test.go) into sc, closing over h so
// every step function has the whole-suite handles it needs.
func (h *acceptanceHarness) initializeScenario(sc *godog.ScenarioContext) {
	sc.Before(h.beforeScenario)
	sc.After(h.afterScenario)
	h.registerCloneAndPushSteps(sc)
	h.registerVocabularySteps(sc)
}
