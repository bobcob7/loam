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
// text, fixed alongside this wiring); reconcile every enrolled repo's bare
// mirror (Startup step 3, loam-ofg.19/.18: idempotently copy the real
// pre-receive hook binary (cmd/loamhook) into place and set the
// receive.denyNonFastForwards/receive.denyDeletes config, docs/git-spec.md
// "Enforcement Mechanics"); build the ingest worker pool and re-queue any
// ingest_jobs orphaned by a prior crash (Startup step 4); then start the
// policy socket (loam-ofg.18), the ingest worker pool, and the HTTP
// listener, listener last, exactly as run's own doc comment below details.
//
// The sync scheduler (mirrorsync.Scheduler) is deliberately NOT wired here:
// see run's doc comment for why constructing one today would do more harm
// than good.
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
// The /healthz and /readyz handlers below are placeholders: loam-ofg.22
// owns their real liveness/readiness logic. They exist so this bead's own
// claim -- that the health exemption is reachable at the mux level with no
// Authorization header, the "only such exemption" in docs/server-spec.md
// -- is checkable against a running binary today, and so a future bead
// replaces only their handler bodies, not the RegisterUnauthenticated call
// that makes them unauthenticated. Because Startup now gates the listener
// behind a real migrate-then-pool-connect (both of which fail fast and
// exit the process on error), simply reaching either endpoint at all is
// already a meaningful readiness signal for anything driving this binary
// (e.g. a Taskfile backgrounding it for a demo): poll GET /readyz until it
// returns 200 instead of sleeping a guessed number of seconds. ofg.22's
// follow-up (an ongoing per-request Postgres/migration check) only
// sharpens that signal for failures occurring *after* startup; it does not
// change what a caller polling readiness during startup should do today.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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
	"github.com/bobcob7/loam/internal/gitdiff"
	"github.com/bobcob7/loam/internal/gittransport"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/git"
	"github.com/bobcob7/loam/internal/handler/graph"
	"github.com/bobcob7/loam/internal/handler/meta"
	"github.com/bobcob7/loam/internal/handler/repo"
	"github.com/bobcob7/loam/internal/handler/repoadmin"
	"github.com/bobcob7/loam/internal/handler/search"
	"github.com/bobcob7/loam/internal/handler/workbranch"
	"github.com/bobcob7/loam/internal/hooksocket"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/ingest/embed/ollama"
	"github.com/bobcob7/loam/internal/mirrorreconcile"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/rolestore"
	"github.com/bobcob7/loam/internal/server"
	"github.com/bobcob7/loam/internal/workbranchstore"
	loamweb "github.com/bobcob7/loam/web"
)

// errIngestOrchestratorNotImplemented is returned by
// notImplementedOrchestrator in place of loam-c94.12's real ingest
// pipeline. See notImplementedOrchestrator's doc comment.
var errIngestOrchestratorNotImplemented = errors.New("ingest orchestrator not implemented (loam-c94.12)")

// notImplementedOrchestrator stands in for loam-c94.12's real ingest
// pipeline (parse -> chunk -> embed -> swap), which has no production
// implementation anywhere in this tree yet. It is wired into a real
// ingest.Pool here -- rather than leaving the worker pool unconstructed
// entirely -- so RequeueOrphaned and the pool's own start/drain lifecycle
// run for real against Postgres from this bead onward. In practice this is
// never invoked: nothing in the tree can enqueue an ingest_jobs row yet
// either (loam-c94.2, also open), so the table stays empty. If a row is
// ever present regardless (e.g. seeded by hand), this fails loudly and the
// job retries with backoff rather than silently doing nothing -- the same
// "loud failure over silent wrong behavior" choice this file already makes
// for /healthz and /readyz below.
type notImplementedOrchestrator struct{}

// Run implements ingest.Orchestrator.
func (notImplementedOrchestrator) Run(_ context.Context, _ ingest.Job) (ingest.Stats, error) {
	return ingest.Stats{}, errIngestOrchestratorNotImplemented
}

// errRepoDeleteNotImplemented is returned by notImplementedRepoDeleter in
// place of loam-cwb's real cross-table repos-row delete path (a separate,
// still-open bead: "no store can delete a repos row today"). See
// internal/handler/repoadmin's RemoveRepo doc comment for the guard-vs-
// delete split this bead's own scope draws -- the guard (enumerate
// blocking non-terminal work branches, typed RemovalBlocked detail) is
// wired below and genuinely enforced; only the final delete step fails
// loudly until loam-cwb lands.
var errRepoDeleteNotImplemented = errors.New("repo delete path not implemented (loam-cwb)")

// notImplementedRepoDeleter stands in for loam-cwb's real repos-row
// delete path, wired here instead of leaving RemoveRepo unregistered so
// its guard (the half loam-ofg.12 owns) is genuinely reachable and
// enforced. The same "loud failure over silent wrong behavior" choice
// this file already makes for notImplementedOrchestrator.
type notImplementedRepoDeleter struct{}

// DeleteRepo implements internal/handler/repoadmin's repoDeleter.
func (notImplementedRepoDeleter) DeleteRepo(_ context.Context, _ uuid.UUID) error {
	return errRepoDeleteNotImplemented
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
// The sync scheduler is deliberately not constructed here: 5 of its 7
// collaborators (Fetcher/loam-giq.2, AdvanceDetector/loam-giq.4,
// MergeabilityChecker/loam-giq.5, the IngestEnqueuer adapter/loam-c94.2,
// PRPoller/loam-giq.8) have no production implementation anywhere in the
// tree -- only RepoLister (loam-13z's StoreRepoLister) and
// SyncStateReporter are real. Wiring a Scheduler with placeholder stand-ins
// for those five would make every enrolled repo's Mirror Sync cycle fail
// step 1 immediately and report sync_state='error' for the entire
// enrollment on every tick -- materially worse and more misleading than
// simply not starting it yet, the same conclusion loam-13z's own closing
// NOTES reached constructing this package's RepoLister producer.
// mirrorsync.Scheduler.Shutdown (added alongside this bead) and the
// runner/closer seams in interfaces.go are both ready for that Scheduler to
// be constructed and passed to serve as its background runner the moment
// giq.2/4/5/8 and c94.2 land -- a single mechanical addition, not a
// redesign of this function.
//
// loam-f75: this function -- production's only caller of Scheduler.Run --
// still does not construct one, so the constraint it names ("never call
// Scheduler.Run and Scheduler.Tick on the same Scheduler") cannot be
// violated from here today. The acceptance harness (loam-li0.5) builds its
// OWN Scheduler, over this same real ingest.Pool/reposstore wiring plus
// fakeforge, purely to drive testsched.SyncHarness.Tick from step
// definitions -- see cmd/server/acceptance_harness_test.go's
// newSyncHarness doc comment for how that Scheduler is built so its Run is
// never reachable at all, satisfying loam-f75 by construction rather than
// by convention.
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
	pool, err := connectDatabase(ctx, cfg, migrations.Migrate, db.NewPool)
	if err != nil {
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
	ingestPool := ingest.NewPool(cfg.Logger, pool, notImplementedOrchestrator{}, cfg.IngestWorkers)
	if err := ingestPool.RequeueOrphaned(ctx); err != nil {
		pool.Close()
		return fmt.Errorf("requeuing orphaned ingest jobs: %w", err)
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
	// onAccept is nil: loam-giq.6 (catch-up detection, the consumer of
	// refpolicy.PostAcceptFunc) does not exist anywhere in this tree yet.
	// A nil hook is EvaluatePush's own documented no-op, not a missing
	// wiring step here.
	policyStore := policyStoreAdapter{repos: repoStore, workBranches: workbranchstore.New(gen.New(pool), cfg.Logger)}
	policySocketPath := filepath.Join(cfg.DataDir, "hook.sock")
	policyServer, err := hooksocket.Listen(policySocketPath, policyStore, nil, cfg.Logger)
	if err != nil {
		pool.Close()
		return fmt.Errorf("starting policy socket: %w", err)
	}
	listener, err := newListener(cfg.HTTPAddr)
	if err != nil {
		pool.Close()
		return fmt.Errorf("starting http listener: %w", err)
	}
	router := buildRouter(cfg, pool, ingestPool, hookBinaryPath)
	httpServer := server.NewHTTPServer(cfg.HTTPAddr, router.Handler())
	background := multiRunner{ingestPool, policyServer}
	if onReady != nil {
		onReady(pool, ingestPool, hookBinaryPath)
	}
	return serve(ctx, stop, cfg.Logger, listener, httpServer, background, pool, defaultShutdownGrace)
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
func buildRouter(cfg config.Config, pool *pgxpool.Pool, ingestPool *ingest.Pool, hookBinaryPath string) *server.Router {
	auth := httpauth.New(cfg.AdminUser, cfg.AdminPassword)
	router := server.New(auth)
	router.RegisterSPA(loamweb.Dist())
	router.RegisterUnauthenticated("/healthz", placeholderHealthHandler("live"))
	router.RegisterUnauthenticated("/readyz", placeholderHealthHandler("ready"))
	registerMetadataServices(router, cfg, pool)
	registerWorkBranchService(router, cfg, pool)
	registerGitService(router, cfg, pool)
	registerRepoAdminService(router, cfg, pool, ingestPool, hookBinaryPath)
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
	router.RegisterCLI(loamv1connect.NewRepoServiceHandler(repo.New(repos, capabilities, errorMapper, cfg.Logger)))
	router.RegisterCLI(loamv1connect.NewMetaServiceHandler(meta.New(roles, errorMapper, cfg.Logger)))
}

// registerWorkBranchService wires loam.v1.WorkBranchService's lifecycle
// half (loam-ofg.8: CreateWorkBranch, UpdateWorkBranch, RequestReview,
// ListWorkBranches, GetWorkBranch, GetWorkBranchDiff) over pool, the same
// single Postgres connection registerMetadataServices' services share.
// GetWorkBranchDiff is backed by internal/gitdiff.Computer (loam-fwk),
// rooted at cfg.DataDir over the same repos store every other RPC here
// shares; every RPC this handler implements is wired against real stores
// and genuinely reachable.
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
	diff := gitdiff.New(cfg.DataDir, repos)
	router.RegisterCLI(loamv1connect.NewWorkBranchServiceHandler(
		workbranch.New(workBranches, repos, rounds, diff, capabilities, errorMapper, cfg.Logger),
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
// add on top of it. RemoveRepo's actual cross-table delete is wired to
// notImplementedRepoDeleter until loam-cwb lands a real one; its guard
// (enumerating blocking non-terminal work branches) is fully real.
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
	httpClient := &http.Client{}
	// A single, host-agnostic *forge.Forgejo (host/token both empty) is
	// deliberately reused for gitCredentialConverter here, mirroring
	// internal/mirrorsync's own tests and internal/gittransport's doc
	// comment on GitCredentials: that method's Forgejo-token-as-password
	// convention is the same for every Forgejo host, so it needs neither
	// binding nor per-call reconstruction the way ForgeChecker's CheckRepo
	// below does (CheckRepo enforces the instance's OWN bound host matches
	// the upstream URL, so it must be rebuilt per host+token).
	transport := gittransport.New(credentials, forge.NewForgejo("", "", httpClient, cfg.Logger), cfg.Logger)
	checker := repoadmin.ForgeChecker{HTTPClient: httpClient, Logger: cfg.Logger}
	errorMapper := handler.NewErrorMapper(cfg.Logger)
	router.RegisterAdmin(adminv1connect.NewRepoAdminServiceHandler(
		repoadmin.New(cfg.DataDir, repos, workBranches, credentials, checker, transport, bindHookBinary(hookBinaryPath), ingestPool, ingestPool, notImplementedRepoDeleter{}, errorMapper, cfg.Logger),
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
	router.RegisterCLI(loamv1connect.NewGraphServiceHandler(graph.New(symbols, scope, capabilities, errorMapper, cfg.Logger)))
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
	embedder, err := ollama.New(cfg.EmbedderURL, cfg.EmbedderModel, &http.Client{}, cfg.Logger)
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
	router.RegisterCLI(loamv1connect.NewSearchServiceHandler(search.New(chunks, embedder, scope, capabilities, errorMapper, cfg.Logger)))
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

// placeholderHealthHandler stands in for loam-ofg.22's real /healthz and
// /readyz handlers (liveness, and Postgres/migration readiness
// respectively).
func placeholderHealthHandler(status string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(status))
	})
}
