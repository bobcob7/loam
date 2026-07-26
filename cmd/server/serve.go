package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// defaultShutdownGrace bounds how long in-flight HTTP requests and
// already-running background work get to finish once SIGINT/SIGTERM
// arrives before shutdown gives up waiting (docs/server-spec.md ->
// Shutdown: "a grace period, default 30s"). serve takes it as a
// parameter rather than a hardcoded constant so a test can shrink it
// (the bead's DESIGN note: "make it an injectable value, not hardcoded,
// so tests can shrink it") -- run always passes this default.
const defaultShutdownGrace = 30 * time.Second

// listenerFDEnv names the env var a test harness sets to hand this
// process an already-bound listener via os/exec's ExtraFiles, instead of
// this process binding cfg.HTTPAddr itself. This closes the bind-close-
// rebind race an otherwise-typical "reserve a free port, close it, then
// tell the child to rebind" test helper has (loam-2m0): whatever else is
// running on the machine can steal the port in the gap between that
// close and this process's own bind. Handing the *os.File belonging to
// an already-bound *net.TCPListener across the process boundary instead
// closes that window entirely, since nothing ever unbinds the port.
// os/exec numbers ExtraFiles starting at fd 3 in the child, in order, so
// a caller with exactly one ExtraFiles entry sets this to "3". Production
// never sets this: it always binds cfg.HTTPAddr fresh.
const listenerFDEnv = "LOAM_LISTENER_FD"

// newListener binds the HTTP listener: from an inherited file descriptor
// if listenerFDEnv is set (test harnesses only, see its doc comment),
// otherwise a fresh TCP bind on addr. Binding here, before the caller
// logs "listening" or starts Serve, is what makes that log line an honest
// readiness signal: by the time it is written the OS has already
// completed the bind, unlike calling http.Server.ListenAndServe directly
// (which binds AND serves internally, so a log line written just before
// calling it precedes the real bind, not follows it).
func newListener(addr string) (net.Listener, error) {
	fdStr := os.Getenv(listenerFDEnv)
	if fdStr == "" {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("binding %s: %w", addr, err)
		}
		return listener, nil
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return nil, fmt.Errorf("parsing %s=%q: %w", listenerFDEnv, fdStr, err)
	}
	if fd < 0 {
		return nil, fmt.Errorf("parsing %s=%q: file descriptor must not be negative", listenerFDEnv, fdStr)
	}
	file := os.NewFile(uintptr(fd), "loam-http-listener")
	if file == nil {
		return nil, fmt.Errorf("wrapping inherited listener fd %d: not a valid file descriptor", fd)
	}
	// net.FileListener dups fd into its own returned Listener (its doc
	// comment: "closing the returned Listener does not put an end to
	// file"). Without this Close, file -- and, since it is a dup of the
	// same underlying socket, the fd 3 the OS actually keeps in LISTEN --
	// stays open for this whole process's lifetime, including past
	// httpServer.Shutdown closing the *returned* listener: the port would
	// remain bound and accepting connections via this leaked duplicate,
	// silently defeating "stop accepting connections" on shutdown.
	defer file.Close()
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("wrapping inherited listener fd %d: %w", fd, err)
	}
	return listener, nil
}

// serve starts background (the ingest worker pool today; see run's doc
// comment for why the sync scheduler is not one of these yet) and
// httpServer, then blocks until ctx is done (SIGINT/SIGTERM) or Serve
// itself fails, at which point it runs docs/server-spec.md's Shutdown
// sequence: stop watching for a second signal (so it falls through to
// Go's default terminate-immediately behavior, an operator's escape
// hatch out of a hung shutdown, instead of being absorbed here for the
// whole grace period), shut down httpServer bounded by grace, wait
// (bounded by that same grace deadline) for background's Run to return,
// then close db. db is closed last and unconditionally (via defer) so a
// background job or HTTP handler still finishing its work during the
// bounded wait never observes a closed pool out from under it.
func serve(ctx context.Context, stop context.CancelFunc, logger *slog.Logger, listener net.Listener, httpServer *http.Server, background runner, db closer, grace time.Duration) error {
	defer db.Close()
	backgroundDone := make(chan struct{})
	go func() {
		defer close(backgroundDone)
		logger.Info("starting background components")
		background.Run(ctx)
	}()
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", listener.Addr().String())
		err := httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()
	var serveResult error
	var serveRead bool
	select {
	case serveResult = <-serveErr:
		serveRead = true
		stop()
	case <-ctx.Done():
		stop()
		logger.Info("shutdown signal received", "grace", grace)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	select {
	case <-backgroundDone:
	case <-shutdownCtx.Done():
		logger.Warn("background components did not drain within the shutdown grace period")
	}
	// serveRead, not "serveResult == nil", is what distinguishes "already
	// read serveErr in the select above" from "haven't read it yet": nil
	// is Serve's own legitimate success value (http.ErrServerClosed maps
	// to it), not just a zero-value sentinel, so testing serveResult
	// itself would block here forever on the ctx.Done() path whenever
	// Serve's error also happens to be nil by the time this runs.
	if !serveRead {
		serveResult = <-serveErr
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutting down http server: %w", shutdownErr)
	}
	return serveResult
}
