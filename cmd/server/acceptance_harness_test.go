//go:build acceptance

package main

import (
	"context"
	"testing"

	"github.com/cucumber/godog"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/crypto"
	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/gitmergetree"
	"github.com/bobcob7/loam/internal/gitref"
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
// fakeforge instance plus the two client handles pointed at it (a
// *fakeforge.Client for the provider REST surface and a
// *gittransport.Transport for authenticated upstream git), and the two
// testsched wrappers backing the "the next sync runs"/"the upstream PR
// merges" and "after ingestion" step-vocabulary rows. Constructed once by
// newAcceptanceHarness; per-scenario mutable state lives in
// acceptanceWorld (acceptance_world_test.go), never here.
type acceptanceHarness struct {
	t             *testing.T
	server        acceptanceServer
	forge         *fakeforge.Server
	forgeClient   *fakeforge.Client
	forgeHost     string
	transport     *gittransport.Transport
	loamBinary    string
	adminUser     string
	adminPassword string
	syncHarness   *testsched.SyncHarness
	ingestHarness *testsched.IngestHarness
	accepter      *mirrorsync.StoreProposalAccepter
	// credentials is the SAME production *credentialstore.Store type the
	// real server's own composition root builds (cmd/server/sync.go's
	// buildUpstreamPRCloser/buildProposalAccepter/buildSyncScheduler each
	// build one over cfg.EncryptionKey), over this suite's shared live
	// pool. It exists so insertRepoRow (acceptance_seed_test.go) can seed
	// the encrypted credentials row those production code paths resolve a
	// repo's forge_host through -- the ONE piece of production wiring this
	// harness cannot substitute a static token for, since it is reached
	// only via the real server process's own RPC handlers (e.g.
	// ProposalService.CloseWorkBranch's best-effort upstream PR close),
	// never via this harness's own accepter/transport/syncHarness, which
	// all use staticTokenCredentialSource instead.
	credentials *credentialstore.Store
	// prAttribution is the loaded LOAM_PR_ATTRIBUTION value the server and
	// the harness's own accepter were both built with, so the step that
	// asserts an upstream PR's body predicts the SAME body the running
	// configuration produces rather than assuming the default.
	prAttribution bool
}

// newAcceptanceHarness assembles the fixed, whole-suite acceptanceHarness.
// forgeBaseURL is the shared fakeforge instance's own httptest URL, and
// forgeHost its host:port -- the repos.forge_host value every repo this
// suite seeds against the fake carries, and therefore the key
// gittransport resolves a credential under.
func newAcceptanceHarness(t *testing.T, srv acceptanceServer, forge *fakeforge.Server, forgeBaseURL, forgeHost string, cfg config.Config) *acceptanceHarness {
	t.Helper()
	forge.AddToken(acceptanceForgeToken)
	forgeClient := fakeforge.NewClient(forgeBaseURL, acceptanceForgeToken)
	transport := gittransport.New(staticTokenCredentialSource{token: acceptanceForgeToken}, forgeClient, acceptanceLogger())
	encryptor, err := crypto.NewEncryptor(cfg.EncryptionKey)
	require.NoError(t, err)
	return &acceptanceHarness{
		t:             t,
		server:        srv,
		forge:         forge,
		forgeClient:   forgeClient,
		forgeHost:     forgeHost,
		transport:     transport,
		loamBinary:    acceptanceLoamBinary,
		adminUser:     cfg.AdminUser,
		adminPassword: cfg.AdminPassword,
		syncHarness:   newSyncHarness(srv, transport, forgeClient),
		ingestHarness: testsched.NewIngestHarness(srv.ingestPool),
		accepter:      newAcceptanceAccepter(srv, transport, forgeClient, cfg),
		prAttribution: cfg.PRAttribution,
		credentials:   credentialstore.New(srv.pool, encryptor, acceptanceLogger()),
	}
}

// newAcceptanceAccepter builds the harness's own
// *mirrorsync.StoreProposalAccepter over the SAME live pool run()
// constructed, the same authenticated gittransport.Transport, and the
// shared fakeforge's provider surface -- the production graph
// cmd/server/sync.go's buildProposalAccepter wires, with the two
// substitutions this whole harness makes everywhere (the fake forge for a
// real one, a static token for the encrypted credential store).
//
// Attribution comes from the loaded config, not a literal, so
// LOAM_PR_ATTRIBUTION's default reaches this suite exactly as it reaches
// production.
//
// This is what makes the "an accepted work branch whose upstream PR has
// merged" fixture a real accept rather than a hand-written UPDATE:
// work_branches.upstream_pr_number is the entire poll set of
// mirrorsync.StorePRPoller, nothing else in the tree writes it, and until
// loam-giq.7 landed the only way to reach the poller at all was to seed
// the column directly (see stepAnAcceptedWorkBranchWhosePRHasMerged).
func newAcceptanceAccepter(srv acceptanceServer, transport *gittransport.Transport, forgeClient *fakeforge.Client, cfg config.Config) *mirrorsync.StoreProposalAccepter {
	return newAcceptanceAccepterWithAttribution(srv, transport, forgeClient, cfg.PRAttribution)
}

// newAcceptanceAccepterWithAttribution is newAcceptanceAccepter's own
// construction, factored out so a scenario that means to exercise the
// PRAttribution knob itself (features/sync.feature's "the server is
// configured without PR attribution") can build a second, scenario-scoped
// *mirrorsync.StoreProposalAccepter over the SAME live pool, transport, and
// fake-forge client the harness's own h.accepter uses -- differing only in
// the one bool this package has no other seam to vary per scenario, since
// LOAM_PR_ATTRIBUTION is read once, process-wide, before TestFeatures'
// single shared server ever boots (acceptanceConfig).
func newAcceptanceAccepterWithAttribution(srv acceptanceServer, transport *gittransport.Transport, forgeClient *fakeforge.Client, attribution bool) *mirrorsync.StoreProposalAccepter {
	logger := acceptanceLogger()
	repoStore := reposstore.NewStore(gen.New(srv.pool), logger)
	workBranchStore := workbranchstore.New(gen.New(srv.pool), logger)
	tips := gitref.New(srv.dataDir)
	return mirrorsync.NewStoreProposalAccepter(srv.dataDir, logger, attribution, repoStore, workBranchStore, workBranchStore, forgeClient, transport, tips)
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
// instance in newAcceptanceHarness, above.
const acceptanceForgeToken = "loam-acceptance-static-token"

// newSyncHarness builds the harness's OWN *mirrorsync.Scheduler --
// wired over the SAME live pool run() constructed (via real
// reposstore/workbranchstore-backed collaborators, so it reads whatever
// this scenario's own SQL fixtures just seeded) and a real MirrorFetcher
// pointed at the shared fakeforge instance -- and immediately wraps it in
// a *testsched.SyncHarness, which is ALL this function returns.
//
// Every one of the scheduler's seven collaborators is now the production
// type, constructed exactly as cmd/server/main.go's run() would construct
// it (loam-a16): StoreRepoLister (loam-13z), MirrorFetcher (giq.2),
// StoreAdvanceDetector (giq.4), StoreMergeabilityChecker (giq.5) over a
// real gitmergetree.Checker, StoreIngestEnqueuer (c94.2) over THE live
// ingest.Pool run() itself built, StorePRPoller (giq.8) over the shared
// fakeforge's provider surface and this suite's one authenticated
// gittransport.Transport, and internal/mirrorsync/state's Reporter (giq.9)
// over the live pool. There are no harness-local stand-ins left in this
// graph at all, so "the next sync runs" drives a genuine five-step Mirror
// Sync cycle end to end rather than erroring out at step 2.
//
// It also applies production's own cross-repo concurrency bound
// (defaultMaxConcurrentCycles, sync.go -- loam-k1fb), so this graph stays
// identical to buildSyncScheduler's in fan-out shape too, not just in
// which type backs which seam. No scenario enrolls anywhere near that many
// repos, so the bound is behaviourally invisible here; it is passed so the
// harness cannot drift into exercising a fan-out the shipped binary is
// incapable of.
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
// production reaches the same constraint from the other direction.
// cmd/server/main.go's run() DOES construct a Scheduler now (loam-0do) --
// an earlier version of this comment said it did not, which stopped being
// true the moment that bead landed -- but buildSyncScheduler (sync.go)
// keeps that Scheduler as a local too and returns only a runner, so its
// Tick is just as unreachable as this one's Run. The two Schedulers are
// distinct objects, and loam-f75's panic ("WaitGroup is reused before
// previous Wait has returned") is per instance, so no Tick here can ever
// collide with that Run there.
//
// mirrorsync.Scheduler now also guards this internally (driveMu
// serializes Run and Tick against each other on one instance instead of
// racing the shared WaitGroup), so even a Scheduler that DID leak both
// directions would block, not panic. This function's own escape
// prevention is kept regardless, for the same reason cmd/server/main.go's
// run() keeps its own: a reachable Tick on either Scheduler is a bug
// worth making unreachable, not merely safe to trip over.
//
// The production scheduler's wall-clock TICKS are kept away from this
// suite separately, by acceptanceConfig's LOAM_SYNC_INTERVAL -- see
// acceptanceSyncInterval for why interval, not the per-repo guard, is the
// mechanism that has to do that job.
func newSyncHarness(srv acceptanceServer, transport *gittransport.Transport, forgeClient *fakeforge.Client) *testsched.SyncHarness {
	logger := acceptanceLogger()
	repoStore := reposstore.NewStore(gen.New(srv.pool), logger)
	workBranchStore := workbranchstore.New(gen.New(srv.pool), logger)
	resolver := mirrorsync.NewStoreRepoResolver(repoStore, workBranchStore)
	fetcher := mirrorsync.NewMirrorFetcher(srv.dataDir, transport, resolver)
	advances := mirrorsync.NewStoreAdvanceDetector(repoStore, repoStore, workBranchStore)
	mergeability := mirrorsync.NewStoreMergeabilityChecker(srv.dataDir, repoStore, workBranchStore, gitmergetree.New(logger), workBranchStore)
	ingestEnqueuer := mirrorsync.NewStoreIngestEnqueuer(repoStore, repoStore, srv.ingestPool)
	prPoller := mirrorsync.NewStorePRPoller(srv.dataDir, logger, repoStore, workBranchStore, workBranchStore, forgeClient, transport)
	reporter := state.New(srv.pool)
	scheduler := mirrorsync.New(logger, nil, mirrorsync.NewStoreRepoLister(repoStore), fetcher, advances, mergeability, ingestEnqueuer, prPoller, reporter, mirrorsync.WithMaxConcurrentCycles(defaultMaxConcurrentCycles))
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
	h.registerSyncSteps(sc)
	h.registerVocabularySteps(sc)
	h.registerIngestAndQuerySteps(sc)
	h.registerReviewSteps(sc)
	h.registerProposalSteps(sc)
	h.registerRoleSteps(sc)
	h.registerEnrollmentSteps(sc)
	h.registerInstructionsSteps(sc)
}
