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
// text, fixed alongside this wiring); build the ingest worker pool and
// re-queue any ingest_jobs orphaned by a prior crash (Startup step 4); then
// start the ingest worker pool and the HTTP listener, listener last, exactly
// as run's own doc comment below details.
//
// Steps 3 (mirror reconciliation, loam-ofg.19) and the policy socket half
// of step 5 (loam-ofg.18) are not wired here -- neither package exists yet
// in this tree. Nor is the sync scheduler (mirrorsync.Scheduler): see run's
// doc comment for why constructing one today would do more harm than good.
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
	"syscall"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/server"
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
	ingestPool := ingest.NewPool(cfg.Logger, pool, notImplementedOrchestrator{}, cfg.IngestWorkers)
	if err := ingestPool.RequeueOrphaned(ctx); err != nil {
		pool.Close()
		return fmt.Errorf("requeuing orphaned ingest jobs: %w", err)
	}
	listener, err := newListener(cfg.HTTPAddr)
	if err != nil {
		pool.Close()
		return fmt.Errorf("starting http listener: %w", err)
	}
	router := buildRouter(cfg)
	httpServer := server.NewHTTPServer(cfg.HTTPAddr, router.Handler())
	return serve(ctx, stop, cfg.Logger, listener, httpServer, ingestPool, pool, defaultShutdownGrace)
}

// buildRouter wires the auth wrappers onto the mux and mounts the embedded
// SPA plus the two unauthenticated health placeholders. Later handler beads
// (loam-ofg.8/.10/.11 for /loam.v1.*, loam-ofg.12/.13/.14/.15 for
// /loam.admin.v1.*, loam-ofg.16 for /git/*) each add one RegisterCLI /
// RegisterAdmin / RegisterGit call here, once their service constructors
// exist -- the registration point this bead's DESIGN note establishes.
func buildRouter(cfg config.Config) *server.Router {
	auth := httpauth.New(cfg.AdminUser, cfg.AdminPassword)
	router := server.New(auth)
	router.RegisterSPA(loamweb.Dist())
	router.RegisterUnauthenticated("/healthz", placeholderHealthHandler("live"))
	router.RegisterUnauthenticated("/readyz", placeholderHealthHandler("ready"))
	return router
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
