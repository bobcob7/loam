// Command server is the Loam server binary described in docs/server-spec.md:
// a single process that dispatches Connect RPC, git smart HTTP, and the
// embedded admin SPA behind one HTTP listener (docs/web-spec.md -> Hosting
// & Routing).
//
// This composition root currently wires only the LISTENER and its DISPATCH
// (internal/server), the auth wrappers (internal/httpauth), and the
// embedded SPA (web) -- loam-ofg.2's stated scope. It deliberately stops
// short of the rest of docs/server-spec.md -> Startup (connecting to
// Postgres, running migrations, reconciling mirrors, re-queuing orphaned
// ingest jobs, and starting the policy socket / sync scheduler / ingest
// worker pool): that sequencing, and coordinating its shutdown alongside
// this listener's, is loam-ofg.21's job. loam-ofg.21 is expected to grow
// run() by inserting those steps before this file's ListenAndServe and
// starting its own components around the shutdown boundary already
// established here, rather than redesigning it.
//
// The /healthz and /readyz handlers below are placeholders: loam-ofg.22
// owns their real liveness/readiness logic. They exist so this bead's own
// claim -- that the health exemption is reachable at the mux level with no
// Authorization header, the "only such exemption" in docs/server-spec.md
// -- is checkable against a running binary today, and so a future bead
// replaces only their handler bodies, not the RegisterUnauthenticated call
// that makes them unauthenticated.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/server"
	loamweb "github.com/bobcob7/loam/web"
)

// shutdownGrace bounds how long in-flight HTTP requests get to finish once
// SIGINT/SIGTERM arrives before the listener is forced closed
// (docs/server-spec.md -> Shutdown: "a grace period, default 30s"). Draining
// the sync scheduler and ingest worker pool within the same grace period is
// loam-ofg.21's job, once those components exist here.
const shutdownGrace = 30 * time.Second

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

// run builds the dispatch router and the single HTTP listener, then serves
// until SIGINT/SIGTERM triggers a graceful shutdown.
func run(cfg config.Config) error {
	router := buildRouter(cfg)
	httpServer := server.NewHTTPServer(cfg.HTTPAddr, router.Handler())
	return serve(cfg.Logger, cfg.HTTPAddr, httpServer)
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

// serve runs httpServer until ctx is cancelled by SIGINT/SIGTERM, then
// drains in-flight requests for up to shutdownGrace before returning.
func serve(logger *slog.Logger, addr string, httpServer *http.Server) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		err := httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()
	select {
	case err := <-serveErr:
		stop()
		return err
	case <-ctx.Done():
		// Stop watching for signals now, rather than deferring to the end
		// of this function: a second SIGINT/SIGTERM during the drain below
		// must fall through to Go's default terminate-immediately behavior
		// (an operator's escape hatch out of a hung shutdown) instead of
		// being silently absorbed by this same handler for the whole grace
		// period.
		stop()
		logger.Info("shutdown signal received", "grace", shutdownGrace)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-serveErr
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
