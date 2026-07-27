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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/git"
	"github.com/bobcob7/loam/internal/handler/meta"
	"github.com/bobcob7/loam/internal/handler/repo"
	"github.com/bobcob7/loam/internal/handler/workbranch"
	"github.com/bobcob7/loam/internal/hooksocket"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/ingest"
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

// errDiffComputerNotImplemented is returned by notImplementedDiffComputer
// in place of the git diff plumbing loam-ofg.8's own research turned up as
// missing: no package anywhere in this tree shells out to `git diff`
// against a repo's bare mirror (see
// internal/handler/workbranch.DiffComputer's doc comment for the full
// account, including why docs/git-spec.md's passing claim that "the server
// already shells out to git for sync, diffs, and ingest" does not describe
// this tree's actual state) -- that is filed as loam-fwk, NOT loam-ofg.16
// (the git smart-HTTP transport handler, upload-pack/receive-pack framing
// only; it does not cover diff computation).
var errDiffComputerNotImplemented = errors.New("git diff plumbing not implemented (loam-fwk)")

// notImplementedDiffComputer stands in for workbranch.DiffComputer, wired
// here instead of leaving GetWorkBranchDiff unregistered so every other
// WorkBranchService RPC in loam-ofg.8's scope is still genuinely reachable.
// This one RPC fails loudly (CodeInternal, logged by handler.ErrorMapper)
// rather than silently -- the same "loud failure over silent wrong
// behavior" choice notImplementedOrchestrator above already makes for the
// ingest pipeline -- until the real plumbing exists.
type notImplementedDiffComputer struct{}

// Diff implements workbranch.DiffComputer.
func (notImplementedDiffComputer) Diff(_ context.Context, _ workbranchstore.WorkBranch) (string, error) {
	return "", errDiffComputerNotImplemented
}

func main() {
	bootLogger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := config.Load()
	if err != nil {
		bootLogger.Error("loading configuration", "error", err)
		os.Exit(1)
	}
	if err := run(cfg); err != nil {
		cfg.Logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// run executes docs/server-spec.md's Startup sequence and blocks until
// SIGINT/SIGTERM triggers Shutdown. The sync scheduler is deliberately not
// constructed: 5 of its 7 collaborators (Fetcher/loam-giq.2,
// AdvanceDetector/loam-giq.4, MergeabilityChecker/loam-giq.5, the
// IngestEnqueuer adapter/loam-c94.2, PRPoller/loam-giq.8) have no
// production implementation anywhere in the tree -- only RepoLister
// (loam-13z's StoreRepoLister) and SyncStateReporter are real. Wiring a
// Scheduler with placeholder stand-ins for those five would make every
// enrolled repo's Mirror Sync cycle fail step 1 immediately and report
// sync_state='error' for the entire enrollment on every tick -- materially
// worse and more misleading than simply not starting it yet, the same
// conclusion loam-13z's own closing NOTES reached constructing this
// package's RepoLister producer. mirrorsync.Scheduler.Shutdown (added
// alongside this bead) and the runner/closer seams in interfaces.go are
// both ready for that Scheduler to be constructed and passed to serve as
// its background runner the moment giq.2/4/5/8 and c94.2 land -- a single
// mechanical addition, not a redesign of this function.
func run(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	router := buildRouter(cfg, pool)
	httpServer := server.NewHTTPServer(cfg.HTTPAddr, router.Handler())
	background := multiRunner{ingestPool, policyServer}
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
func buildRouter(cfg config.Config, pool *pgxpool.Pool) *server.Router {
	auth := httpauth.New(cfg.AdminUser, cfg.AdminPassword)
	router := server.New(auth)
	router.RegisterSPA(loamweb.Dist())
	router.RegisterUnauthenticated("/healthz", placeholderHealthHandler("live"))
	router.RegisterUnauthenticated("/readyz", placeholderHealthHandler("ready"))
	registerMetadataServices(router, cfg, pool)
	registerWorkBranchService(router, cfg, pool)
	registerGitService(router, cfg, pool)
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
// notImplementedDiffComputer stands in for the git diff plumbing that does
// not exist in this tree yet (see its own doc comment); every other RPC
// this handler implements is wired against real stores and genuinely
// reachable.
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
	router.RegisterCLI(loamv1connect.NewWorkBranchServiceHandler(
		workbranch.New(workBranches, repos, rounds, notImplementedDiffComputer{}, capabilities, errorMapper, cfg.Logger),
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
