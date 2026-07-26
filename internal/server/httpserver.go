package server

import (
	"net/http"
	"time"
)

// readHeaderTimeout bounds how long the server waits to read a request's
// headers, defending against slow-header (slowloris-style) clients without
// touching the body-transfer phase.
const readHeaderTimeout = 10 * time.Second

// idleTimeout bounds how long a keep-alive connection may sit idle between
// requests, so the server eventually reaps connections a client opened and
// abandoned.
const idleTimeout = 2 * time.Minute

// NewHTTPServer builds the single-port *http.Server that fronts handler
// (docs/server-spec.md -> Process Model: "One binary, one process ...
// HTTP listener"). ReadTimeout and WriteTimeout are deliberately left at
// their zero value (no limit): this same port carries git smart-HTTP
// clone/push traffic (docs/git-spec.md), whose request and response
// bodies can be large and slow on purpose, and net/http's ReadTimeout/
// WriteTimeout apply to the whole request/response, not just headers — a
// fixed bound there would abort a legitimately long clone. Only
// ReadHeaderTimeout and IdleTimeout are bounded, since neither has a
// legitimate reason to run long.
//
// Starting this server, wiring os/signal-driven graceful shutdown, and
// coordinating that shutdown with the sync scheduler, ingest worker pool,
// and policy socket is loam-ofg.21's job (docs/server-spec.md ->
// Startup/Shutdown); this constructor only builds the *http.Server value
// with its timeouts, the "Provide an http.Server with timeouts" half of
// this bead's scope.
func NewHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}
