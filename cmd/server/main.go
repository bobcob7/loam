// Command server is the Loam server binary described in docs/server-spec.md:
// a single process that dispatches Connect RPC, git smart HTTP, and the
// embedded admin SPA behind one HTTP listener (docs/web-spec.md -> Hosting
// & Routing).
//
// This composition root wires docs/server-spec.md's Startup sequence in
// order: load config (already validated by config.Load); run migrations
// against the DSN, THEN connect the database pool (never the other order --
// see internal/db/pool.go's NewPool doc comment: a pool built before
// migrations have created the pgvector extension deadlocks permanently on a
// virgin database; this is the ordering loam-ut9 filed against the spec
// text, fixed alongside this wiring); verify LOAM_ENCRYPTION_KEY can decrypt
// every already-stored credential (loam-0ab, credentialcheck.go: a wrong key
// otherwise makes CredentialService.GetCredentialStatus/ListCredentials
// report a perfectly healthy credential that every real use then fails to
// decrypt); reconcile every enrolled repo's bare mirror (Startup step 3,
// loam-ofg.19/.18: idempotently copy the real pre-receive hook binary
// (cmd/loamhook) into place and set the
// receive.denyNonFastForwards/receive.denyDeletes config, docs/git-spec.md
// "Enforcement Mechanics"); build the ingest worker pool and re-queue any
// ingest_jobs orphaned by a prior crash (Startup step 4); then start the
// policy socket (loam-ofg.18), the ingest worker pool, and the HTTP
// listener, listener last, exactly as run's own doc comment below details.
//
// The sync scheduler (mirrorsync.Scheduler) IS wired here, as of loam-0do:
// buildSyncScheduler (sync.go) constructs it and all seven of its
// collaborators over the same pool and ingest pool built above, driven by a
// real time.Ticker at cfg.SyncInterval (LOAM_SYNC_INTERVAL), and it joins
// the ingest pool and policy socket in serve's background tier. An earlier
// version of this comment explained why it was deliberately NOT wired --
// most of its collaborators had no production implementation. That reason
// has expired: internal/mirrorsync/production_assertions.go and
// internal/mirrorsync/state/production_assertions.go now pin all seven at
// compile time.
//
// buildRouter's pool parameter (loam-ofg.11) is the seam RepoService and
// MetaService (registerMetadataServices, below) need: connectDatabase's
// pool -- alive by the time run() calls buildRouter -- is threaded straight
// through, so both services are genuinely registered against the real
// connection every other /loam.v1.* handler this composition root builds
// will eventually share. registerMetadataServices still guards against a
// nil pool (buildRouter's own tests exercise that path directly, without a
// live database), but run() never passes one: connectDatabase either
// returns a live pool or run() has already returned its error before
// buildRouter is ever reached.
//
// The /healthz and /readyz handlers are internal/health's (loam-ofg.22),
// registered by registerHealth below. They replaced the placeholders this
// comment used to describe, and only the handler bodies changed: the
// RegisterUnauthenticated calls that make them the "only such exemption"
// in docs/server-spec.md are the same ones loam-ofg.2 wrote. /healthz is
// unconditional liveness and /readyz re-checks Postgres reachability plus
// migration currency on every request -- see internal/health's package
// doc comment for the full account of what readiness checks, what it
// deliberately does not, and why the two endpoints are asymmetric.
//
// Because Startup gates the listener behind a real
// migrate-then-pool-connect (both of which fail fast and exit the process
// on error), simply reaching either endpoint at all is already a
// meaningful startup signal for anything driving this binary (e.g. a
// Taskfile backgrounding it for a demo): poll GET /healthz until it
// returns 200 instead of sleeping a guessed number of seconds. That
// remains the right poll for STARTUP -- it is the one endpoint that
// cannot start reporting failure because a dependency degraded, so it
// never turns a demo or a test harness into a false negative. /readyz is
// the sharper signal for whether the process can serve correctly RIGHT
// NOW, which is a different question.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/catchup"
	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/codegraph"
	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/crypto"
	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/forge"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/gitancestry"
	"github.com/bobcob7/loam/internal/gitanchor"
	"github.com/bobcob7/loam/internal/gitdiff"
	"github.com/bobcob7/loam/internal/gitref"
	"github.com/bobcob7/loam/internal/gittransport"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/credential"
	"github.com/bobcob7/loam/internal/handler/git"
	"github.com/bobcob7/loam/internal/handler/graph"
	"github.com/bobcob7/loam/internal/handler/meta"
	"github.com/bobcob7/loam/internal/handler/proposal"
	"github.com/bobcob7/loam/internal/handler/repo"
	"github.com/bobcob7/loam/internal/handler/repoadmin"
	"github.com/bobcob7/loam/internal/handler/role"
	"github.com/bobcob7/loam/internal/handler/search"
	"github.com/bobcob7/loam/internal/handler/workbranch"
	"github.com/bobcob7/loam/internal/health"
	"github.com/bobcob7/loam/internal/hooksocket"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/ingest/chunker"
	"github.com/bobcob7/loam/internal/ingest/embed/ollama"
	ingestgraph "github.com/bobcob7/loam/internal/ingest/graph"
	"github.com/bobcob7/loam/internal/ingest/orchestrator"
	"github.com/bobcob7/loam/internal/ingest/vectors"
	"github.com/bobcob7/loam/internal/mirrorreconcile"
	"github.com/bobcob7/loam/internal/parser"
	"github.com/bobcob7/loam/internal/reporemove"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/reviewpublish"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/rolestore"
	"github.com/bobcob7/loam/internal/server"
	"github.com/bobcob7/loam/internal/telemetry"
	"github.com/bobcob7/loam/internal/workbranchstore"
	loamweb "github.com/bobcob7/loam/web"
	"go.opentelemetry.io/otel/trace"
)

// buildIngestOrchestrator constructs loam-c94.12's real ingest pipeline
// (plan -> parse/chunk -> embed -> one-transaction swap) and the collaborators
// it owns, returning it alongside a closer that releases the Tree-sitter
// queries the extractor compiled at construction. The returned closer is
// always non-nil, so a caller can defer it unconditionally.
//
// Both compute tracks share ONE *parser.ParserPool: a single parser.Parser
// is not safe for concurrent use (its Tree-sitter state lives in C memory),
// and this orchestrator runs the parse->graph and chunk->embed tracks
// concurrently within a job, on top of however many jobs LOAM_INGEST_WORKERS
// allows at once. The pool leases a Parser per Parse call, which is exactly
// that contract.
//
// A failure here fails startup rather than degrading to a stand-in: unlike
// registerSearchService, which logs and skips its own registration when the
// embedder model is unrecognized (a misconfigured search service must not
// take down the graph service), an ingest pool with no working pipeline
// would claim jobs, fail every one, and retry them forever with backoff
// while quietly reporting the repo as enrolled and syncing. Refusing to
// boot is the honest signal.
func buildIngestOrchestrator(cfg config.Config, pool *pgxpool.Pool, repoStore *reposstore.Store) (ingest.Orchestrator, func(), error) {
	embedder, err := ollama.New(cfg.EmbedderURL, cfg.EmbedderModel, ollama.InstrumentHTTPClient(&http.Client{}, cfg.TracerProvider), cfg.Logger)
	if err != nil {
		return nil, func() {}, fmt.Errorf("building embedder: %w", err)
	}
	parsers := parser.NewParserPool(cfg.Logger)
	extractor, err := ingestgraph.New(parsers, cfg.Logger)
	if err != nil {
		return nil, func() {}, fmt.Errorf("compiling symbol extraction queries: %w", err)
	}
	chunk := chunker.NewChunker(parsers, cfg.Logger)
	indexer := vectors.New(embedder, cfg.Logger)
	return orchestrator.New(cfg.Logger, cfg.DataDir, pool, repoStore, extractor, chunk, indexer, embedder), extractor.Close, nil
}

func main() {
	bootLogger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := config.Load()
	if err != nil {
		bootLogger.Error("loading configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, stop, cfg, nil); err != nil {
		cfg.Logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// run executes docs/server-spec.md's Startup sequence and blocks until ctx
// is done, triggering Shutdown. main() constructs ctx/stop via
// signal.NotifyContext(os.Interrupt, syscall.SIGTERM) so production shuts
// down on SIGINT/SIGTERM exactly as before; ctx and stop are parameters --
// rather than derived here internally -- solely so loam-li0.5's acceptance
// harness can call this exact function, unmodified in every other respect,
// with its own cancelable context instead of a real OS signal it cannot
// send itself (a test process sending SIGINT/SIGTERM to itself would tear
// down the whole `go test` binary, not just the in-process server under
// test). This is what lets the acceptance harness be "wired through the
// same constructor graph as `main()`" (docs/testing-spec.md, Layer 1's
// "Topology" paragraph): the harness calls run, not a hand-rolled subset
// of its steps.
//
// The sync scheduler is constructed here (loam-0do), by buildSyncScheduler
// in sync.go, and handed to serve as a background runner alongside the
// ingest worker pool and the policy socket. Its trigger is a real
// time.Ticker at cfg.SyncInterval; validateSyncInterval runs first, at the
// very top of this function, because time.NewTicker panics on a
// non-positive duration and internal/config range-checks nothing today
// (see validateSyncInterval's own doc comment). The ticker is stopped when
// this function returns.
//
// loam-f75 ("never call Scheduler.Run and Scheduler.Tick on the same
// Scheduler") is satisfied by construction from this end: buildSyncScheduler
// returns a plain runner and never lets the *mirrorsync.Scheduler value
// escape, so nothing reachable from here can call Tick at all. The
// acceptance harness (loam-li0.5) builds its OWN, separate Scheduler over
// this same real pool/ingest.Pool wiring plus fakeforge, purely to drive
// testsched.SyncHarness.Tick from step definitions, and reaches the same
// constraint from its end by never letting its Scheduler escape either --
// see cmd/server/acceptance_harness_test.go's newSyncHarness. The two
// Schedulers are separate objects, so neither one's WaitGroup is ever
// driven by both a Run and a Tick; the acceptance suite additionally sets
// LOAM_SYNC_INTERVAL well past its own runtime so this function's
// wall-clock scheduler cannot cycle a repo underneath a manual tick.
//
// This wiring-level separation is the first line of defense, kept even
// though mirrorsync.Scheduler itself no longer corrupts state if it is
// violated: Scheduler.Run and Scheduler.Tick now serialize against each
// other internally (an unexported driveMu, added for loam-f75) rather
// than racing the same sync.WaitGroup, so a hypothetical future call
// site that DID let a Scheduler's Tick and Run both become reachable
// would see one call block behind the other, not the "WaitGroup is
// reused before previous Wait has returned" panic this comment used to
// warn about. That internal fix does not change the reasoning above:
// this function still keeps its Scheduler local and returns only a
// runner, because letting the production wall-clock scheduler be
// Tick-able at all would be surprising -- an admin RPC or test that
// found a way to call Tick on it would silently block on the next
// LOAM_SYNC_INTERVAL tick's wg.Wait rather than doing anything useful --
// not because of the panic risk, which mirrorsync.Scheduler now owns.
//
// onReady, if non-nil, is called exactly once, after every collaborator
// below is constructed and reachable but before this function hands off to
// serve's blocking Serve/Shutdown loop, with the live pool, ingestPool, and
// resolved hookBinaryPath. Production (main()) always passes nil: nothing
// outside this function needs those handles once serve takes over, since
// every request-handling path already closes over them via buildRouter.
// The one caller that does need them is loam-li0.5's acceptance harness,
// which must call this exact function -- the "same constructor graph as
// `main()`" docs/testing-spec.md's Layer 1 "Topology" paragraph requires --
// while still building its own testsched.SyncHarness/IngestHarness over
// the SAME pool and ingest.Pool this call constructs, not a second,
// divergent instance. A callback is the minimal seam for that: it adds no
// branching to the startup sequence itself (every line above and below it
// is unchanged from before this parameter existed) and cannot be invoked
// more than once, since run() itself never loops.
//
// onReady's contract: it is called synchronously, on this same goroutine,
// AFTER the HTTP listener is already bound (newListener above has already
// succeeded) but BEFORE serve starts accepting connections on it. It must
// neither block nor panic -- a non-nil onReady that does either wedges or
// crashes startup with the port already bound and nothing actually being
// served, which is strictly worse than never calling onReady at all. This
// is why production's onReady is always nil today: the only caller that
// exists (loam-li0.5's acceptance harness) sends its handles over a
// buffered channel and returns immediately, never blocking here.
func run(ctx context.Context, stop context.CancelFunc, cfg config.Config, onReady func(pool *pgxpool.Pool, ingestPool *ingest.Pool, hookBinaryPath string)) error {
	if err := validateSyncInterval(cfg.SyncInterval); err != nil {
		return err
	}
	// Telemetry is constructed FIRST, before anything it might one day
	// instrument exists, so a later bead can hand its providers to the pgx
	// pool's QueryTracer, the router, and the ingest pool without moving
	// this line. With LOAM_OTEL_ENDPOINT unset this costs nothing at all:
	// telemetry.New returns upstream's no-op providers, having created no
	// exporter and started no goroutine (internal/telemetry's package doc).
	telemetryProvider, err := telemetry.New(ctx, telemetry.Config{
		Endpoint:       cfg.OTelEndpoint,
		ServiceName:    cfg.OTelServiceName,
		ServiceVersion: telemetry.BuildVersion(),
		SampleRatio:    cfg.OTelSampleRatio,
	}, cfg.Logger)
	if err != nil {
		return fmt.Errorf("initializing telemetry: %w", err)
	}
	// cfg is a VALUE parameter, so this hands the tracer provider to every
	// buildRouter/register*/buildIngestOrchestrator call below without
	// widening a single signature, and without escaping this function
	// (loam-9v9s). It is never nil -- telemetry.New returns upstream's
	// no-op provider when telemetry is disabled -- so no consumer needs a
	// disabled check.
	cfg.TracerProvider = telemetryProvider.TracerProvider()
	// This defer covers only the STARTUP-FAILURE paths below -- every
	// `pool.Close(); return err` between here and serve -- so a boot that
	// dies at, say, mirror reconciliation still stops the exporter's
	// goroutines instead of leaking them until the process exits. It is NOT
	// where the real flush happens: a defer here runs after serve has
	// returned, and serve closes the pgx pool via its own defer, which
	// would put the flush on the wrong side of the pool close. serve takes
	// the shutdown hook explicitly and calls it at the one correct point
	// (see serve's doc comment); Provider.Shutdown is idempotent, so this
	// defer is a no-op on the path that reaches serve.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), telemetry.DefaultShutdownTimeout)
		defer cancel()
		if err := telemetryProvider.Shutdown(shutdownCtx); err != nil {
			cfg.Logger.Warn("shutting down telemetry", "error", err)
		}
	}()
	pool, err := connectDatabase(ctx, cfg, migrations.Migrate, db.NewPool)
	if err != nil {
		return err
	}
	if err := verifyEncryptionKeyAgainstStoredCredentials(ctx, cfg, pool); err != nil {
		pool.Close()
		return err
	}
	hookBinaryPath, err := loamhookBinaryPath(os.Executable, os.Stat)
	if err != nil {
		pool.Close()
		return fmt.Errorf("locating loamhook binary: %w", err)
	}
	repoStore := reposstore.NewStore(gen.New(pool), cfg.Logger)
	if err := reconcileMirrors(ctx, cfg.Logger, cfg.DataDir, hookBinaryPath, repoStore, mirrorreconcile.ReconcileMirror); err != nil {
		pool.Close()
		return fmt.Errorf("reconciling mirrors: %w", err)
	}
	ingestOrchestrator, closeIngest, err := buildIngestOrchestrator(cfg, pool, repoStore)
	if err != nil {
		pool.Close()
		return fmt.Errorf("building ingest orchestrator: %w", err)
	}
	defer closeIngest()
	ingestPool := ingest.NewPool(cfg.Logger, pool, ingestOrchestrator, cfg.IngestWorkers)
	if err := ingestPool.RequeueOrphaned(ctx); err != nil {
		pool.Close()
		return fmt.Errorf("requeuing orphaned ingest jobs: %w", err)
	}
	// The ticker is created here, after validateSyncInterval has already
	// rejected a non-positive interval at the top of this function --
	// time.NewTicker panics otherwise, and nothing in this binary recovers.
	syncTicker := time.NewTicker(cfg.SyncInterval)
	defer syncTicker.Stop()
	syncScheduler, err := buildSyncScheduler(cfg, pool, ingestPool, syncTicker.C, defaultShutdownGrace)
	if err != nil {
		pool.Close()
		return fmt.Errorf("building sync scheduler: %w", err)
	}
	// loam-ofg.18 (the policy socket) MUST start here, before newListener,
	// not as another background runner passed into serve alongside the
	// ingest pool: docs/server-spec.md Startup step 5 orders the policy
	// socket ahead of the HTTP listener specifically so git pushes are
	// never accepted while it is down. newListener below already binds
	// the port (making it visible to clients per this file's readiness
	// doc comment); serve then starts httpServer.Serve concurrently with
	// every runner it is given. Adding the policy socket as a same-tier
	// background runner inside serve would let Serve start accepting
	// connections before the socket is confirmed live -- it must be
	// constructed and confirmed serving above this line instead.
	//
	// onAccept is internal/catchup's detector: docs/git-spec.md -> "Target
	// Advances & Catch-Up" says a conflict-flagged branch recovers BY
	// PUSH, and this is the only place in the process that sees an
	// accepted push.
	workBranches := workbranchstore.New(gen.New(pool), cfg.Logger)
	policyStore := policyStoreAdapter{repos: repoStore, workBranches: workBranches}
	policySocketPath := filepath.Join(cfg.DataDir, "hook.sock")
	catchupDetector := catchup.New(cfg.DataDir, gitancestry.New(cfg.Logger), workBranches, reviewstore.NewRoundStore(pool, cfg.Logger), cfg.Logger)
	policyServer, err := hooksocket.Listen(policySocketPath, policyStore, catchupDetector.OnAcceptedPush, cfg.Logger)
	if err != nil {
		pool.Close()
		return fmt.Errorf("starting policy socket: %w", err)
	}
	listener, err := newListener(cfg.HTTPAddr)
	if err != nil {
		pool.Close()
		return fmt.Errorf("starting http listener: %w", err)
	}
	router := buildRouter(cfg, pool, ingestPool, hookBinaryPath, telemetryProvider.TracerProvider())
	httpServer := server.NewHTTPServer(cfg.HTTPAddr, router.Handler())
	// policyServer is deliberately NOT a member of this multiRunner: serve
	// (serve.go) closes it separately, strictly after httpServer.Shutdown
	// returns, rather than tearing it down alongside ingestPool and
	// syncScheduler the instant the shutdown signal arrives -- see serve's
	// own doc comment for why (loam-48y, mirroring this file's own
	// loam-ofg.18 comment above about STARTUP ordering, in the other
	// direction). It is also, separately, deliberately NOT given
	// multiRunner's recover-panic-and-keep-serving treatment (loam-lae's
	// recoverMember): a panic in policyServer.Run is left to crash this
	// whole process, on purpose -- see the doc comment directly above the
	// `go func() { ...; policySocket.Run(policyCtx) }()` line in serve.go
	// (loam-ymyq) for the full argument.
	background := newMultiRunner(cfg.Logger,
		member{name: "ingest pool", runner: ingestPool},
		member{name: "sync scheduler", runner: syncScheduler},
	)
	if onReady != nil {
		onReady(pool, ingestPool, hookBinaryPath)
	}
	return serve(ctx, stop, cfg.Logger, listener, httpServer, background, policyServer, pool, telemetryProvider.Shutdown, defaultShutdownGrace)
}

// loamhookBinaryPathName is the filename this process expects to find its
// pre-receive hook client binary under, as a sibling of its own executable
// -- both built from this same module (cmd/server and cmd/loamhook) and
// shipped side by side, the same "install two binaries from one build"
// convention forge tooling like git-lfs's own smudge/clean filters and
// git's own remote helpers use. There is no LOAM_ environment variable for
// this: docs/server-spec.md's configuration table is settled for the MVP
// and does not name one, and the sibling-of-the-running-executable
// location needs no new configuration surface at all.
const loamhookBinaryPathName = "loamhook"

// loamhookBinaryPath resolves cmd/loamhook's compiled binary path as a
// sibling of this process's own executable (executable, typically
// os.Executable, injected so tests can substitute a path without
// depending on the actual test binary's own location) and confirms it
// actually exists there (stat, typically os.Stat, likewise injected).
//
// The stat is deliberately UNCONDITIONAL -- run even when zero repos are
// enrolled yet, not merely left for mirrorreconcile.ReconcileMirror's own
// per-repo hard-error check (see that function's own doc comment) to
// catch later. Review of this bead's first version found exactly that
// gap: ReconcileMirror returns nil, without ever reading hookBinaryPath,
// for a mirror that is not yet on disk (a legitimate, documented no-op --
// see its own doc comment), so a fresh install with zero enrolled repos
// started up cleanly with no loamhook binary present at all and said
// nothing -- a fail-OPEN deployment gap that would only surface, silently,
// the moment the first repo was enrolled and its mirror cloned. Stat-ing
// here turns that latent failure into a loud one at startup, before this
// process ever claims to be ready, on every boot, regardless of
// enrollment state.
func loamhookBinaryPath(executable func() (string, error), stat func(string) (os.FileInfo, error)) (string, error) {
	execPath, err := executable()
	if err != nil {
		return "", fmt.Errorf("resolving own executable path: %w", err)
	}
	path := filepath.Join(filepath.Dir(execPath), loamhookBinaryPathName)
	if _, err := stat(path); err != nil {
		return "", fmt.Errorf("loamhook binary not found at %s (expected as a sibling of this server's own executable): %w", path, err)
	}
	return path, nil
}

// buildRouter wires the auth wrappers onto the mux and mounts the embedded
// SPA plus the two unauthenticated health placeholders, then registers
// every /loam.v1.*, /loam.admin.v1.*, and /git/* handler whose
// dependencies pool makes available. Later handler beads (loam-ofg.8/.10
// for the rest of /loam.v1.*, loam-ofg.12/.13/.14/.15 for
// /loam.admin.v1.*) each add their own RegisterCLI / RegisterAdmin call
// here, once their service constructors exist -- the registration point
// this bead's DESIGN note establishes; registerGitService (loam-ofg.16,
// gated by loam-ofg.17's role gate) is that same pattern applied to
// /git/*.
// pool may be nil (buildRouter's own tests exercise that path without a
// live database); registerMetadataServices no-ops in that case rather than
// registering handlers that would panic on their first real request. run()
// itself never passes nil -- see this file's package doc comment.
// ingestPool is loam-ofg.21's worker pool (constructed in run() before this
// is called), threaded through for registerRepoAdminService's EnrollRepo/
// ReindexRepo/ListIngestJobs; it may be nil for the same reason pool may.
//
// tracerProvider is telemetry.Provider.TracerProvider() (loam-7d3o), the
// source of every RPC span this process emits. run() always passes the real
// one -- which is upstream's no-op when LOAM_OTEL_ENDPOINT is unset, so
// there is no enabled/disabled branch here -- and buildRouter's own tests
// pass nil, which server.New degrades to the same no-op.
//
// EVERY generated service constructor below must take router.RPCOptions()...
// One that does not still routes and still authenticates, but goes untraced.
// internal/server cannot catch that -- connect interceptors are handler
// construction options, so the Router has no way to apply them for the caller
// the way it applies the auth wrappers -- but this package can, and does:
// TestBuildRouter_EveryDeclaredServiceIsTraced walks protoregistry.GlobalFiles
// for every declared loam.v1 / loam.admin.v1 service and asserts a span per
// procedure through this very function, so a forgotten RPCOptions() fails by
// name. Add a service, add its constructor here, and that test covers it with
// no edit of its own.
//
// The two RegisterUnauthenticated health handlers deliberately take nothing
// -- see internal/server's rpcOptions for why the liveness exemption is
// structural.
func buildRouter(cfg config.Config, pool *pgxpool.Pool, ingestPool *ingest.Pool, hookBinaryPath string, tracerProvider trace.TracerProvider) *server.Router {
	auth := httpauth.New(cfg.AdminUser, cfg.AdminPassword)
	router := server.New(auth, tracerProvider)
	router.RegisterSPA(loamweb.Dist())
	registerHealth(router, cfg, pool)
	registerMetadataServices(router, cfg, pool)
	registerWorkBranchService(router, cfg, pool)
	registerGitService(router, cfg, pool)
	registerRepoAdminService(router, cfg, pool, ingestPool, hookBinaryPath)
	registerCredentialService(router, cfg, pool)
	registerProposalService(router, cfg, pool)
	registerRoleService(router, cfg, pool)
	registerGraphService(router, cfg, pool)
	registerSearchService(router, cfg, pool)
	return router
}

// registerMetadataServices wires loam.v1.RepoService and loam.v1.MetaService
// (loam-ofg.11) over pool, the single Postgres connection every /loam.v1.*
// handler this composition root builds ultimately shares. Both read
// internal/reposstore and internal/rolestore directly; MetaService also
// needs RoleStore's instructions text, so roleStoreAdapter (below) is
// built once and satisfies both internal/handler.RoleStore (the
// capability gate RepoService.GetRepo's git.clone check reads) and
// meta.RoleStore (the fuller surface GetInstructions reads) from the same
// underlying rolestore.Store.
//
// pool == nil is the only guard here, exercised directly by buildRouter's
// own tests (no live database): registering these handlers without a pool
// would panic the first request that reached them. run() always supplies
// a real, already-connected pool (connectDatabase returns before
// buildRouter is ever called), so this guard is never hit in production.
func registerMetadataServices(router *server.Router, cfg config.Config, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	repos := reposstore.NewStore(gen.New(pool), cfg.Logger)
	roles := roleStoreAdapter{store: rolestore.NewStore(pool, cfg.Logger)}
	capabilities := handler.NewCapabilityChecker(roles)
	errorMapper := handler.NewErrorMapper(cfg.Logger)
	router.RegisterCLI(loamv1connect.NewRepoServiceHandler(repo.New(repos, capabilities, errorMapper, cfg.Logger), router.RPCOptions()...))
	router.RegisterCLI(loamv1connect.NewMetaServiceHandler(meta.New(roles, errorMapper, cfg.Logger), router.RPCOptions()...))
}

// registerWorkBranchService wires loam.v1.WorkBranchService in full over
// pool, the same single Postgres connection registerMetadataServices'
// services share: the lifecycle half (loam-ofg.8: CreateWorkBranch,
// UpdateWorkBranch, RequestReview, ListWorkBranches, GetWorkBranch,
// GetWorkBranchDiff) and the review half (loam-ofg.9: ListComments,
// ListVerdicts, SubmitVerdict, ReplyToThread). GetWorkBranchDiff is backed
// by internal/gitdiff.Computer (loam-fwk), rooted at cfg.DataDir over the
// same repos store every other RPC here shares; every RPC this handler
// implements is wired against real stores and genuinely reachable.
//
// SubmitVerdict's publisher is given the POOL, not a pre-bound querier,
// because it opens and owns a pgx.Tx per call -- that transaction is what
// makes the publish atomic (internal/reviewpublish). Handing it the same
// gen.New(pool) the other stores use would quietly remove that property.
//
// pool == nil is the only guard here, for the same reason and exercised
// the same way as registerMetadataServices' own guard: run() always
// supplies a real, already-connected pool, so this is never hit in
// production, only by buildRouter's own tests.
func registerWorkBranchService(router *server.Router, cfg config.Config, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	repos := reposstore.NewStore(gen.New(pool), cfg.Logger)
	roles := roleStoreAdapter{store: rolestore.NewStore(pool, cfg.Logger)}
	capabilities := handler.NewCapabilityChecker(roles)
	errorMapper := handler.NewErrorMapper(cfg.Logger)
	workBranches := workbranchstore.New(gen.New(pool), cfg.Logger)
	rounds := reviewstore.NewRoundStore(pool, cfg.Logger)
	threads := reviewstore.NewThreadStore(pool, cfg.Logger)
	verdicts := reviewstore.NewVerdictStore(pool, cfg.Logger)
	anchors := gitanchor.New(cfg.DataDir, repos)
	publisher := reviewpublish.New(pool, anchors, cfg.Logger)
	diff := gitdiff.New(cfg.DataDir, repos)
	// The work-branch ref writer: docs/git-spec.md -> Ref Policy makes the
	// server the ONLY creator of a work-branch ref (loam-5iu), and the
	// pre-receive hook enforces the other half by rejecting a push that
	// tries to create one.
	refs := gitref.New(cfg.DataDir)
	router.RegisterCLI(loamv1connect.NewWorkBranchServiceHandler(
		workbranch.New(workBranches, repos, rounds, diff, refs, threads, verdicts, publisher, capabilities, errorMapper, cfg.Logger),
		router.RPCOptions()...,
	))
}

// registerGitService wires the /git/* smart-HTTP transport (loam-ofg.16)
// behind internal/handler.GitRoleGate (loam-ofg.17), reusing the same
// repos store and capability checker registerMetadataServices' and
// registerWorkBranchService's services already build over pool. Wiring
// composition recorded on loam-ofg.16 during loam-ofg.17's review:
// router.RegisterGit(prefix, gate.Middleware(gitHandler)) -- RegisterGit
// itself additionally wraps the result in httpauth.Auth.GitIdentity (see
// internal/server/router.go), so the full chain a request passes through
// is GitIdentity -> GitRoleGate -> git.Handler.
//
// pool == nil is the only guard here, for the same reason and exercised
// the same way as registerMetadataServices' own guard.
func registerGitService(router *server.Router, cfg config.Config, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	repos := reposstore.NewStore(gen.New(pool), cfg.Logger)
	roles := roleStoreAdapter{store: rolestore.NewStore(pool, cfg.Logger)}
	capabilities := handler.NewCapabilityChecker(roles)
	gate := handler.NewGitRoleGate(capabilities, cfg.Logger)
	gitHandler := git.New(cfg.DataDir, repos, cfg.Logger)
	router.RegisterGit("/git/", gate.Middleware(gitHandler))
}

// bindHookBinary adapts mirrorreconcile.ReconcileMirror to the two-argument
// seam internal/handler/repoadmin defines at its consumer. The hook binary's
// location is a property of THIS process's deployment (loam-ofg.18 resolves
// it as a sibling of os.Executable and run() fails startup if it is absent),
// not something an enrollment handler should know or care about -- so it is
// bound here at the composition root rather than widening repoadmin's seam.
func bindHookBinary(hookBinaryPath string) func(context.Context, string) error {
	return func(ctx context.Context, repoPath string) error {
		return mirrorreconcile.ReconcileMirror(ctx, repoPath, hookBinaryPath)
	}
}

// registerRepoAdminService wires loam.admin.v1.RepoAdminService
// (loam-ofg.12) over pool and ingestPool -- the same live Postgres
// connection and ingest worker pool run() already constructed -- plus a
// gittransport.Transport (loam-giq.3) for the initial mirror clone/
// ls-remote and a credentialstore.Store (loam-54o.8) for resolving each
// upstream host's token. Unlike every /loam.v1.* handler this file
// registers, RepoAdminService needs no handler.CapabilityChecker: the
// entire /loam.admin.v1.* path group is already wrapped in
// httpauth.Auth.AdminOnly before any request reaches a handler
// (docs/web-spec.md -> Auth), so there is no per-RPC capability gate to
// add on top of it. RemoveRepo's actual cross-table delete is a real
// reporemove.Remover (loam-cwb) over the same repos store and the same
// cfg.DataDir the enrollment clone writes its mirror under, so both halves
// of that RPC -- the guard enumerating blocking non-terminal work branches
// and the delete itself -- are live here.
//
// pool == nil or ingestPool == nil is the only guard here, for the same
// reason and exercised the same way as registerMetadataServices' own
// guard: run() always supplies both live values, so this is never hit in
// production, only by buildRouter's own tests.
func registerRepoAdminService(router *server.Router, cfg config.Config, pool *pgxpool.Pool, ingestPool *ingest.Pool, hookBinaryPath string) {
	if pool == nil || ingestPool == nil {
		return
	}
	repos := reposstore.NewStore(gen.New(pool), cfg.Logger)
	workBranches := workbranchstore.New(gen.New(pool), cfg.Logger)
	enc, err := crypto.NewEncryptor(cfg.EncryptionKey)
	if err != nil {
		cfg.Logger.Error("repo admin service: building encryptor, not registering", "error", err)
		return
	}
	credentials := credentialstore.New(pool, enc, cfg.Logger)
	httpClient := forge.InstrumentHTTPClient(&http.Client{}, cfg.TracerProvider)
	// A single, host-agnostic *forge.Resolver is deliberately reused for
	// gitCredentialConverter here, mirroring internal/mirrorsync's own
	// tests and internal/gittransport's doc comment on GitCredentials:
	// that method's token-as-password convention is the same for every
	// Kind this package resolves to (forge.gitCredentialsConvention), so
	// it needs neither binding nor per-call reconstruction the way
	// ForgeChecker's CheckRepo below does (CheckRepo enforces the
	// instance's OWN bound host matches the upstream URL, so it must be
	// rebuilt per host+token, AND resolve the right Kind for that host).
	transport := gittransport.New(credentials, forge.NewResolver(httpClient, cfg.Logger), cfg.Logger)
	checker := repoadmin.ForgeChecker{HTTPClient: httpClient, Logger: cfg.Logger}
	errorMapper := handler.NewErrorMapper(cfg.Logger)
	router.RegisterAdmin(adminv1connect.NewRepoAdminServiceHandler(
		repoadmin.New(cfg.DataDir, repos, workBranches, credentials, checker, transport, bindHookBinary(hookBinaryPath), ingestPool, ingestPool, reporemove.New(cfg.DataDir, repos, cfg.Logger), errorMapper, cfg.Logger),
		router.RPCOptions()...,
	))
}

// registerCredentialService wires loam.admin.v1.CredentialService
// (loam-ofg.15) over pool: the one token per forge host that every repo on
// that host shares. It builds the same crypto.NewEncryptor +
// credentialstore.New pair registerRepoAdminService builds -- deliberately
// a second, independent construction rather than a shared one, so a
// failure to build either service's encryptor takes down only that
// service, matching the log-and-skip choice every other register* function
// here already makes for its own optional collaborator.
//
// Until this line existed there was NO supported way to store an encrypted
// forge token at all: /loam.admin.v1.CredentialService/* 404'd through
// internal/server's group-level fallback, and the only writer of the
// credentials table in the tree was cmd/demoenv's seed-credential
// subcommand reaching through internal/credentialstore directly (a plain
// psql INSERT cannot substitute -- token_ciphertext is AES-GCM under
// LOAM_ENCRYPTION_KEY). RepoAdminService.EnrollRepo, gittransport, and
// mirrorsync have all been READING that table since they landed.
//
// The forge provider passed as the token validator is a single,
// host-agnostic *forge.Resolver, the same shape registerRepoAdminService
// reuses for gittransport's credential converter and for the same
// reason: Provider.ValidateToken takes its host and token explicitly,
// and Resolver resolves the right Kind for whatever host each call
// names, so no per-host binding is needed or wanted here. That is the
// opposite of repoadmin.ForgeChecker, which must rebuild per call
// because CheckRepo compares against the instance's own bound host.
//
// pool == nil is the only unconditional guard, for the same reason and
// exercised the same way as registerMetadataServices' own guard.
func registerCredentialService(router *server.Router, cfg config.Config, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	enc, err := crypto.NewEncryptor(cfg.EncryptionKey)
	if err != nil {
		cfg.Logger.Error("credential service: building encryptor, not registering", "error", err)
		return
	}
	credentials := credentialstore.New(pool, enc, cfg.Logger)
	validator := forge.NewResolver(forge.InstrumentHTTPClient(&http.Client{}, cfg.TracerProvider), cfg.Logger)
	router.RegisterAdmin(adminv1connect.NewCredentialServiceHandler(
		credential.New(credentials, validator, handler.NewErrorMapper(cfg.Logger), cfg.Logger),
		router.RPCOptions()...,
	))
}

// registerProposalService wires loam.admin.v1.ProposalService
// (loam-ofg.14) over pool: the admin's proposal queue and the accept/close
// decisions. Like registerRepoAdminService it needs no
// handler.CapabilityChecker -- the whole /loam.admin.v1.* group is already
// behind httpauth.Auth.AdminOnly -- though the handler itself re-asserts
// admin status per RPC as defence in depth (see proposal.requireAdmin).
//
// This registration is what makes work_branches.upstream_pr_number a
// column anything in a RUNNING server ever writes. *mirrorsync
// .StoreProposalAccepter is its only writer tree-wide, and that column is
// the entire poll set of mirrorsync.StorePRPoller (Mirror Sync step 5),
// which buildSyncScheduler has been starting since loam-0do with nothing
// able to put a row into its poll set. Until this line existed the sync
// cycle's PR-tracking step polled an empty set on every tick in production.
//
// pool == nil is the only guard, for the same reason and exercised the same
// way as registerMetadataServices' own; a failure to build either
// mirrorsync collaborator logs and skips just this service, the same choice
// registerRepoAdminService makes for its encryptor.
func registerProposalService(router *server.Router, cfg config.Config, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	httpClient := forge.InstrumentHTTPClient(&http.Client{}, cfg.TracerProvider)
	accepter, err := buildProposalAccepter(cfg, pool, httpClient)
	if err != nil {
		cfg.Logger.Error("proposal service: building the acceptance engine, not registering", "error", err)
		return
	}
	prCloser, err := buildUpstreamPRCloser(cfg, pool, httpClient)
	if err != nil {
		cfg.Logger.Error("proposal service: building the upstream PR closer, not registering", "error", err)
		return
	}
	repos := reposstore.NewStore(gen.New(pool), cfg.Logger)
	workBranches := workbranchstore.New(gen.New(pool), cfg.Logger)
	verdicts := reviewstore.NewVerdictStore(pool, cfg.Logger)
	// tips resolves a work branch's live mirror tip against
	// ListProposals's recorded accepted_tip (loam-cgg) -- the same
	// *gitref.Creator type registerWorkBranchService wires for ref
	// creation, rooted at the same LOAM_DATA_DIR.
	tips := gitref.New(cfg.DataDir)
	router.RegisterAdmin(adminv1connect.NewProposalServiceHandler(
		proposal.New(workBranches, repos, verdicts, accepter, prCloser, tips, handler.NewErrorMapper(cfg.Logger), cfg.Logger),
		router.RPCOptions()...,
	))
}

// registerRoleService wires loam.admin.v1.RoleService (loam-ofg.13) over
// pool: the admin's configuration of what each agent role may do and the
// instruction text MetaService.GetInstructions returns for it. It takes
// *rolestore.Store DIRECTLY, not the roleStoreAdapter every /loam.v1.*
// registration above wraps it in -- the adapter narrows the store to the
// []handler.Capability read those services need, and this service is the
// writer of the very rows that read returns.
//
// Until this line existed there was no supported way to change a role at
// all: the two built-ins seeded by migration 0001_init were the entire
// runtime role set, both with empty instructions, and
// /loam.admin.v1.RoleService/* 404'd through internal/server's group-level
// fallback.
//
// pool == nil is the only guard here, for the same reason and exercised the
// same way as registerMetadataServices' own guard.
func registerRoleService(router *server.Router, cfg config.Config, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	router.RegisterAdmin(adminv1connect.NewRoleServiceHandler(
		role.New(rolestore.NewStore(pool, cfg.Logger), handler.NewErrorMapper(cfg.Logger), cfg.Logger),
		router.RPCOptions()...,
	))
}

// registerGraphService wires loam.v1.GraphService (loam-ofg.10) over pool,
// the same live Postgres connection every other /loam.v1.* handler this
// composition root builds shares. Repo-scope expansion (an empty
// QueryScope.repos, or `--all`, resolving to every enrolled repo) is
// handler.ScopeResolver, shared with registerSearchService below so both
// services build the "ingested" envelope field identically.
//
// pool == nil is the only guard here, for the same reason and exercised the
// same way as registerMetadataServices' own guard: run() always supplies a
// real, already-connected pool, so this is never hit in production, only by
// buildRouter's own tests.
func registerGraphService(router *server.Router, cfg config.Config, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	repos := reposstore.NewStore(gen.New(pool), cfg.Logger)
	roles := roleStoreAdapter{store: rolestore.NewStore(pool, cfg.Logger)}
	capabilities := handler.NewCapabilityChecker(roles)
	errorMapper := handler.NewErrorMapper(cfg.Logger)
	scope := handler.NewScopeResolver(repos)
	symbols := codegraph.New(gen.New(pool), cfg.Logger)
	router.RegisterCLI(loamv1connect.NewGraphServiceHandler(graph.New(symbols, scope, capabilities, errorMapper, cfg.Logger), router.RPCOptions()...))
}

// registerSearchService wires loam.v1.SearchService (loam-ofg.10) over pool
// and an Ollama embedder resolved from cfg.EmbedderURL/cfg.EmbedderModel
// (docs/server-spec.md), mirroring internal/ingest's own embedder wiring
// (internal/ingest/embed/ollama). Registered independently of
// registerGraphService -- a misconfigured/unrecognized embedder model must
// not also take down GraphService, which needs no embedder at all -- the
// same "log and skip only this service" choice registerRepoAdminService
// makes for its own encryptor failure.
//
// pool == nil is the only unconditional guard here, for the same reason as
// every other register* function's own guard.
func registerSearchService(router *server.Router, cfg config.Config, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	embedder, err := ollama.New(cfg.EmbedderURL, cfg.EmbedderModel, ollama.InstrumentHTTPClient(&http.Client{}, cfg.TracerProvider), cfg.Logger)
	if err != nil {
		cfg.Logger.Error("search service: building embedder, not registering", "error", err)
		return
	}
	repos := reposstore.NewStore(gen.New(pool), cfg.Logger)
	roles := roleStoreAdapter{store: rolestore.NewStore(pool, cfg.Logger)}
	capabilities := handler.NewCapabilityChecker(roles)
	errorMapper := handler.NewErrorMapper(cfg.Logger)
	scope := handler.NewScopeResolver(repos)
	chunks := chunkstore.New(pool, cfg.Logger)
	router.RegisterCLI(loamv1connect.NewSearchServiceHandler(search.New(chunks, embedder, scope, capabilities, errorMapper, cfg.Logger), router.RPCOptions()...))
}

// roleStoreAdapter adapts internal/rolestore.Store's plain-string
// role_operations to the []handler.Capability surfaces
// internal/handler.RoleStore (CapabilityChecker's gate) and
// internal/handler/meta.RoleStore (GetInstructions) both consume.
// role_operations.operation is CHECK-constrained to exactly the fixed
// capability vocabulary (role_operations_operation_check,
// 0001_init.up.sql), so every string this converts is already a valid
// handler.Capability -- rolestore itself does not import internal/handler
// (a store package depending on an RPC-boundary package would invert the
// layering "interfaces defined at the consumer" establishes), so this
// conversion belongs here, at composition-root wiring time, not in
// rolestore.
type roleStoreAdapter struct {
	store *rolestore.Store
}

// RoleCapabilities implements internal/handler.RoleStore and
// internal/handler/meta.RoleStore.
func (a roleStoreAdapter) RoleCapabilities(ctx context.Context, role string) ([]handler.Capability, error) {
	r, err := a.store.GetRole(ctx, role)
	if err != nil {
		return nil, err
	}
	capabilities := make([]handler.Capability, len(r.Operations))
	for i, operation := range r.Operations {
		capabilities[i] = handler.Capability(operation)
	}
	return capabilities, nil
}

// RoleInstructions implements internal/handler/meta.RoleStore.
func (a roleStoreAdapter) RoleInstructions(ctx context.Context, role string) (string, error) {
	r, err := a.store.GetRole(ctx, role)
	if err != nil {
		return "", err
	}
	return r.Instructions, nil
}

// registerHealth wires docs/server-spec.md -> Health's two unauthenticated
// endpoints (loam-ofg.22). internal/health owns both handler bodies and
// the reasoning behind them; this function owns only which collaborators
// they get.
//
// /healthz is registered UNCONDITIONALLY, unlike every other register*
// function's pool-guarded body, and that is the point: liveness takes no
// collaborator, so there is nothing a nil pool could make it unable to
// answer. Every integration test in this package and every `task demo:*`
// target polls this endpoint as its startup signal, so it must exist in
// every configuration this router can be built in.
//
// /readyz needs the live pool for both of its checks -- Pool.Ping and a
// schema_migrations read over that same pool -- so it takes the same
// pool == nil guard every other register* function here takes, exercised
// the same way (buildRouter's own tests, which have no database). run()
// never passes nil.
func registerHealth(router *server.Router, cfg config.Config, pool *pgxpool.Pool) {
	router.RegisterUnauthenticated("/healthz", health.Live())
	if pool == nil {
		return
	}
	router.RegisterUnauthenticated("/readyz", health.NewReadiness(pool, migrations.NewSchemaCheck(pool), cfg.Logger))
}
