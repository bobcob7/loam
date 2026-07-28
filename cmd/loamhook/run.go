package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/bobcob7/loam/internal/hooksocket"
	"github.com/bobcob7/loam/internal/mirrorpath"
)

// dialFunc matches the one socket round trip this hook performs per
// invocation (docs/git-spec.md "Enforcement Mechanics": "it sends one
// request over the socket ... and gets back a per-ref verdict").
// Production always binds this to hooksocket.Call with
// hooksocket.DialTimeout/RPCTimeout; this package's own tests substitute a
// fake to exercise run's own logic -- stdin parsing, env propagation,
// fail-closed on error -- without a real socket.
type dialFunc func(socketPath string, req hooksocket.Request) (hooksocket.Response, error)

// run is this hook's entire decision, injected with every external
// dependency (stdin, stderr, environment lookup, cwd, and the socket
// round trip itself) so it is fully unit-testable without a real
// subprocess, a real socket, or a real git invocation. It returns the
// process exit code main should use: 0 only when the policy socket
// explicitly accepted the whole push; 1 for every other outcome --
// malformed stdin, a failure locating or reaching the policy socket, or
// an explicit rejection. That is docs/git-spec.md "Enforcement
// Mechanics"'s fail-closed contract applied literally: every failure mode
// this function can observe ends in a nonzero exit, never a silent
// accept.
func run(stdin io.Reader, stderr io.Writer, getenv func(string) string, getwd func() (string, error), dial dialFunc) int {
	updates, err := parseUpdates(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "loam: pre-receive hook: %v\n", err)
		return 1
	}
	if len(updates) == 0 {
		// git never actually invokes pre-receive with zero proposed ref
		// updates in practice, but there is nothing to police here, and
		// dialing the socket over an empty update set would be pure
		// overhead for a case that cannot reject anything anyway.
		return 0
	}
	cwd, err := getwd()
	if err != nil {
		fmt.Fprintf(stderr, "loam: pre-receive hook: determining working directory: %v\n", err)
		return 1
	}
	dataDir, err := mirrorpath.DataDir(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "loam: pre-receive hook: locating policy socket from %s: %v\n", cwd, err)
		return 1
	}
	socketPath := filepath.Join(dataDir, "hook.sock")
	req := hooksocket.Request{
		Repo: getenv("LOAM_REPO"),
		Agent: hooksocket.AgentIdentity{
			Name: getenv("LOAM_AGENT_NAME"),
			ID:   getenv("LOAM_AGENT_ID"),
			Role: getenv("LOAM_AGENT_ROLE"),
		},
		Updates: updates,
		// GIT_QUARANTINE_PATH is set by receive-pack itself (git >= 2.11),
		// not by internal/handler/git's explicit subprocess environment,
		// and it names the temporary object directory holding every object
		// this push is proposing. Forwarding it is what lets the SERVER
		// side inspect the pushed history at all: the objects are not in
		// the bare mirror's own object store yet, so a `git
		// --git-dir=<mirror>` process there cannot resolve the new tip
		// (measured against git 2.50.1 -- see internal/gitancestry's
		// package doc comment). Empty when git did not set it, which the
		// server treats as "nothing extra to read", never as an error.
		QuarantineDir: getenv("GIT_QUARANTINE_PATH"),
	}
	resp, err := dial(socketPath, req)
	if err != nil {
		fmt.Fprintf(stderr, "loam: policy socket unavailable; rejecting push: %v\n", err)
		return 1
	}
	if resp.Accepted {
		return 0
	}
	printedAReason := false
	for _, v := range resp.Verdicts {
		if !v.Allowed {
			fmt.Fprintln(stderr, v.Reason)
			printedAReason = true
		}
	}
	// A rejected response with no per-ref reason to print is exactly the
	// shape a hard evaluation error produces (internal/hooksocket.Server's
	// own evaluate: {Accepted: false, Verdicts: nil} when the store errors
	// or a context deadline expires mid-lookup) -- the most important
	// fail-closed case this hook has, since it is what a down/unreachable
	// Postgres looks like from here. Without this fallback, the agent sees
	// only git's own bare "pre-receive hook declined", with no loam: line
	// at all, which looks like a transport bug rather than a deliberate
	// policy rejection.
	if !printedAReason {
		fmt.Fprintln(stderr, "loam: push rejected by policy (no per-ref reason available)")
	}
	return 1
}
