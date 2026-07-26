package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner is a spy runner: it records that Run started and finished,
// and delegates to an injected function so a test controls exactly when
// (or whether) it returns.
type fakeRunner struct {
	mu       sync.Mutex
	started  bool
	finished bool
	run      func(ctx context.Context)
}

func (f *fakeRunner) Run(ctx context.Context) {
	f.mu.Lock()
	f.started = true
	f.mu.Unlock()
	if f.run != nil {
		f.run(ctx)
	}
	f.mu.Lock()
	f.finished = true
	f.mu.Unlock()
}

func (f *fakeRunner) isFinished() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished
}

// fakeCloser is a spy closer: it records whether Close was called.
type fakeCloser struct {
	mu     sync.Mutex
	closed bool
}

func (f *fakeCloser) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func (f *fakeCloser) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// newTestListener binds a real, ephemeral TCP listener for tests that
// need to prove the socket itself is genuinely closed (net.Dial, not a
// mock, is the only honest way to observe that).
func newTestListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return listener
}

// runServeAsync starts serve in a goroutine and returns a channel its
// result lands on, so tests can assert both "serve is still blocked" (by
// checking the channel is empty) and "serve returned within a bound" (by
// selecting on the channel with a timeout) without sleeping to guess at
// timing.
func runServeAsync(ctx context.Context, stop context.CancelFunc, listener net.Listener, httpServer *http.Server, background runner, db closer, grace time.Duration) <-chan error {
	done := make(chan error, 1)
	go func() { done <- serve(ctx, stop, testLogger(), listener, httpServer, background, db, grace) }()
	return done
}

// TestServe_ShutdownClosesListenerAndDatabaseAfterBackgroundDrains is the
// combined discriminating proof for two of this bead's named mutations:
// "skip closing the DB pool" and "return from shutdown before the
// listener actually closes". It uses a REAL TCP listener (not a fake) so
// the listener-closed assertion is an honest net.Dial failure, and holds
// the background runner open on a channel the test controls so it can
// assert db.Close() has NOT happened while background is still draining,
// only that it happens once background finishes -- a mutant that called
// db.Close() eagerly (e.g. synchronously right after starting the
// background goroutine, instead of via the final defer) would fail the
// first assertion; a mutant that skipped the Close call entirely would
// fail the second.
func TestServe_ShutdownClosesListenerAndDatabaseAfterBackgroundDrains(t *testing.T) {
	t.Parallel()
	listener := newTestListener(t)
	addr := listener.Addr().String()
	httpServer := &http.Server{Handler: http.NewServeMux()}
	release := make(chan struct{})
	background := &fakeRunner{run: func(ctx context.Context) { <-release }}
	db := &fakeCloser{}
	ctx, cancel := context.WithCancel(context.Background())
	done := runServeAsync(ctx, cancel, listener, httpServer, background, db, 5*time.Second)

	cancel() // simulate SIGTERM/SIGINT

	select {
	case err := <-done:
		t.Fatalf("serve returned (err=%v) before the background component was released -- it did not wait for it to drain", err)
	case <-time.After(200 * time.Millisecond):
	}
	assert.False(t, db.isClosed(), "the database must not be closed while the background component is still draining")

	close(release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the background component finished draining")
	}
	assert.True(t, background.isFinished())
	assert.True(t, db.isClosed(), "the database must be closed once shutdown completes")
	_, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	assert.Error(t, dialErr, "the listener must actually be closed after serve returns")
}

// TestServe_AbandonsBackgroundWaitAfterGracePeriodElapses is the
// discriminating proof for "ignore the shutdown context deadline": a
// background component that never respects ctx (a bug in whatever it
// wraps, or simply still busy) must not be allowed to hang shutdown
// forever. serve is given a short grace period and a background runner
// that blocks unconditionally; the assertion is that serve still returns
// within a small bounded multiple of that grace period. A mutant that
// replaced the bounded select (waiting on backgroundDone OR the grace
// deadline) with an unconditional `<-backgroundDone` would hang this test
// until its own outer timeout fires and fails it -- proving the mutation
// is caught by assertion (a failed, bounded test) rather than by an
// indefinite hang.
func TestServe_AbandonsBackgroundWaitAfterGracePeriodElapses(t *testing.T) {
	t.Parallel()
	listener := newTestListener(t)
	httpServer := &http.Server{Handler: http.NewServeMux()}
	background := &fakeRunner{run: func(ctx context.Context) { <-make(chan struct{}) }} // never returns
	db := &fakeCloser{}
	ctx, cancel := context.WithCancel(context.Background())
	const grace = 100 * time.Millisecond
	done := runServeAsync(ctx, cancel, listener, httpServer, background, db, grace)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return within a bounded multiple of the grace period -- a hung background component wedged shutdown")
	}
	assert.True(t, db.isClosed(), "the database must still be closed even when the background component never drains")
}

// TestServe_StartsBackgroundComponentBeforeReturning proves serve
// actually starts the injected background runner at all (a mutant that
// forgot to call background.Run, e.g. while wiring a different
// component, would leave started false forever).
func TestServe_StartsBackgroundComponentBeforeReturning(t *testing.T) {
	t.Parallel()
	listener := newTestListener(t)
	httpServer := &http.Server{Handler: http.NewServeMux()}
	background := &fakeRunner{run: func(ctx context.Context) { <-ctx.Done() }}
	db := &fakeCloser{}
	ctx, cancel := context.WithCancel(context.Background())
	done := runServeAsync(ctx, cancel, listener, httpServer, background, db, time.Second)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return")
	}
	background.mu.Lock()
	started := background.started
	background.mu.Unlock()
	assert.True(t, started, "serve must start the background component")
}
