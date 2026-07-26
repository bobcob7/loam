package main

import (
	"net/http"
	"testing"
)

// newIsolatedTransport returns an *http.Transport cloned from
// http.DefaultTransport, with its own private idle-connection pool.
//
// This package's tests start and stop many httptest.Server/real-listener
// instances, often on OS-reassigned ports across otherwise-unrelated
// tests -- these are shutdown tests, so closing servers is their whole
// purpose. http.DefaultTransport (what http.Get, http.Post, and any
// http.Client with a nil Transport field all use under the hood) keeps
// ONE idle-connection pool for the entire process, keyed only by
// host:port. A connection left idle after one test's server closes can
// get handed to a LATER test's request once the OS reissues that same
// port, producing a bogus "EOF" or "http: server closed idle connection"
// failure that has nothing to do with either test's own logic (loam-nk6).
//
// Every test in this package that talks HTTP to a server it starts/stops
// itself must build its client on a transport from here (or another
// private *http.Transport) instead of http.DefaultClient / http.Get /
// http.Post -- do not "simplify" this back to the stdlib shortcuts.
func newIsolatedTransport(t *testing.T) *http.Transport {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

// newIsolatedHTTPClient returns an *http.Client wrapping a fresh, private
// transport (see newIsolatedTransport's doc comment for why) -- the
// drop-in replacement for http.Get/http.DefaultClient.Do in this
// package's tests.
func newIsolatedHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: newIsolatedTransport(t)}
}
