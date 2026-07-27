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
	}
	resp, err := dial(socketPath, req)
	if err != nil {
		fmt.Fprintf(stderr, "loam: policy socket unavailable; rejecting push: %v\n", err)
		return 1
	}
	if resp.Accepted {
		return 0
	}
	for _, v := range resp.Verdicts {
		if !v.Allowed {
			fmt.Fprintln(stderr, v.Reason)
		}
	}
	return 1
}
