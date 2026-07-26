package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTrackedRunner builds a runnerMock (moq-generated in moq_test.go, per
// this package's interfaces.go //go:generate directive) whose RunFunc
// delegates to run -- or blocks on ctx.Done() if run is nil -- and
// returns an isFinished func reporting whether Run has returned. "Has
// Run started at all" needs no separate tracking: the mock's own
// generated RunCalls() records the call before RunFunc runs, so
// len(mock.RunCalls()) > 0 already answers that.
func newTrackedRunner(run func(ctx context.Context)) (mock *runnerMock, isFinished func() bool) {
	var mu sync.Mutex
	var finished bool
	mock = &runnerMock{
		RunFunc: func(ctx context.Context) {
			if run != nil {
				run(ctx)
			} else {
				<-ctx.Done()
			}
			mu.Lock()
			finished = true
			mu.Unlock()
		},
	}
	return mock, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return finished
	}
}

// newTrackedCloser builds a closerMock whose CloseFunc records that Close
// was called, returning an isClosed func to check it.
func newTrackedCloser() (mock *closerMock, isClosed func() bool) {
	var mu sync.Mutex
	var closed bool
	mock = &closerMock{
		CloseFunc: func() {
			mu.Lock()
			closed = true
			mu.Unlock()
		},
	}
	return mock, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return closed
	}
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
	background, backgroundFinished := newTrackedRunner(func(ctx context.Context) { <-release })
	db, dbClosed := newTrackedCloser()
	ctx, cancel := context.WithCancel(context.Background())
	done := runServeAsync(ctx, cancel, listener, httpServer, background, db, 5*time.Second)

	cancel() // simulate SIGTERM/SIGINT

	select {
	case err := <-done:
		t.Fatalf("serve returned (err=%v) before the background component was released -- it did not wait for it to drain", err)
	case <-time.After(200 * time.Millisecond):
	}
	assert.False(t, dbClosed(), "the database must not be closed while the background component is still draining")

	close(release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the background component finished draining")
	}
	assert.True(t, backgroundFinished())
	assert.True(t, dbClosed(), "the database must be closed once shutdown completes")
	_, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	assert.Error(t, dialErr, "the listener must actually be closed after serve returns")
}

// TestServe_AbandonsBackgroundWaitAfterGracePeriodElapses is the
// discriminating proof for "ignore the shutdown context deadline": a
// background component that never respects ctx (a bug in whatever it
// wraps, or simply still busy) must not be allowed to hang shutdown
// forever. serve is given a short grace period and a background runner
// that blocks unconditionally; the assertion is that serve still returns
// within a bounded ceiling well above that grace period -- the ceiling
// here (100x grace) is deliberately loose, not "a small bounded
// multiple": it exists only to fail the test rather than hang it forever,
// and any finite value would equally catch the mutant this test targets
// (replacing the bounded select -- backgroundDone OR the grace deadline
// -- with an unconditional `<-backgroundDone`, which hangs forever
// regardless of grace), so there is no correctness reason to tighten it.
func TestServe_AbandonsBackgroundWaitAfterGracePeriodElapses(t *testing.T) {
	t.Parallel()
	listener := newTestListener(t)
	httpServer := &http.Server{Handler: http.NewServeMux()}
	background, _ := newTrackedRunner(func(ctx context.Context) { <-make(chan struct{}) }) // never returns
	db, dbClosed := newTrackedCloser()
	ctx, cancel := context.WithCancel(context.Background())
	const grace = 100 * time.Millisecond
	done := runServeAsync(ctx, cancel, listener, httpServer, background, db, grace)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return within a bounded multiple of the grace period -- a hung background component wedged shutdown")
	}
	assert.True(t, dbClosed(), "the database must still be closed even when the background component never drains")
}

// TestServe_StartsBackgroundComponentBeforeReturning proves serve
// actually starts the injected background runner at all (a mutant that
// forgot to call background.Run, e.g. while wiring a different
// component, would leave RunCalls empty forever).
func TestServe_StartsBackgroundComponentBeforeReturning(t *testing.T) {
	t.Parallel()
	listener := newTestListener(t)
	httpServer := &http.Server{Handler: http.NewServeMux()}
	background, _ := newTrackedRunner(nil) // nil: blocks on ctx.Done(), per newTrackedRunner's doc comment
	db, _ := newTrackedCloser()
	ctx, cancel := context.WithCancel(context.Background())
	done := runServeAsync(ctx, cancel, listener, httpServer, background, db, time.Second)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return")
	}
	assert.NotEmpty(t, background.RunCalls(), "serve must start the background component")
}

// TestServe_DrainsInFlightHTTPRequestOnShutdown is this bead's own named
// acceptance line: "a SIGTERM mid-request drains it before process exit
// within the grace period." It holds a real HTTP request open (blocked in
// the handler, not just queued) at the moment shutdown begins, and proves
// two things a request that merely never started could not: (1) serve
// does not return while that request is still being handled, and (2) the
// client actually receives its response (the connection was never cut),
// only after which serve returns. httpServer.Shutdown provides the
// draining itself (stdlib behavior); this test proves cmd/server's serve
// actually invokes it against a listener carrying a live connection,
// rather than e.g. closing the listener some other way that would abort
// in-flight requests.
func TestServe_DrainsInFlightHTTPRequestOnShutdown(t *testing.T) {
	t.Parallel()
	listener := newTestListener(t)
	addr := listener.Addr().String()
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})
	httpServer := &http.Server{Handler: mux}
	background, _ := newTrackedRunner(nil)
	db, _ := newTrackedCloser()
	ctx, cancel := context.WithCancel(context.Background())
	// loam-ofg.21 widened these to 30s/20s, misattributing the package's
	// real cause (loam-6ob: a double-owned fd with an armed finalizer in
	// TestNewListener_ClosesInheritedSourceFileDescriptor, closed later by
	// GC, killing an unrelated live connection elsewhere in the binary --
	// not a timing issue at all) to contention/slowness. With that fd bug
	// fixed, this test has no real dependency on wall-clock slack: every
	// step is a local goroutine handoff or a loopback round trip, and it
	// completes in well under a second even under the full package's
	// parallel `-race` load (measured: 200-300ms per run across `go test
	// ./cmd/server/ -race -count=30`, 0 failures). 5s leaves an order of
	// magnitude of headroom over that observed cost for slower/loaded CI
	// runners while still failing fast, not after a real 30s hang, if
	// draining ever regresses.
	const grace = 5 * time.Second
	done := runServeAsync(ctx, cancel, listener, httpServer, background, db, grace)

	type result struct {
		status int
		err    error
	}
	requestResult := make(chan result, 1)
	client := newIsolatedHTTPClient(t)
	go func() {
		resp, err := client.Get("http://" + addr + "/slow")
		if err != nil {
			requestResult <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		requestResult <- result{status: resp.StatusCode}
	}()

	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request never reached the handler")
	}

	cancel() // simulate SIGTERM arriving mid-request

	select {
	case err := <-done:
		t.Fatalf("serve returned (err=%v) while a request was still being handled -- it did not drain the in-flight request", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseRequest)

	select {
	case r := <-requestResult:
		require.NoError(t, r.err, "the in-flight request must receive its response, not have its connection cut by shutdown")
		assert.Equal(t, http.StatusOK, r.status)
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request never completed")
	}

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the in-flight request finished")
	}
}

// TestServe_ServeFailureCallsStopSoBackgroundStillDrains is the
// discriminating proof that the Serve-failure branch's stop() call is
// load-bearing, not incidental: stop is the SAME context.CancelFunc that
// cancels the ctx background.Run was given, so on this branch it is the
// ONLY thing that ever tells background to stop. A mutant that deleted
// both stop() calls from serve (one per select branch) would pass every
// other test in this file -- none of them give background a Run that
// blocks on ctx and then exercise the Serve-failure branch specifically
// -- yet leave this test's runner blocked forever, caught here by
// isFinished() staying false rather than by a hang: shutdownCtx's own
// grace-bounded deadline (independent of stop having run at all) still
// lets serve return, so the failure surfaces as a false assertion, not a
// wedged test.
func TestServe_ServeFailureCallsStopSoBackgroundStillDrains(t *testing.T) {
	t.Parallel()
	listener := newTestListener(t)
	require.NoError(t, listener.Close()) // Serve on this listener fails immediately
	httpServer := &http.Server{Handler: http.NewServeMux()}
	background, backgroundFinished := newTrackedRunner(nil)
	db, dbClosed := newTrackedCloser()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // safety net only; serve calling stop (== cancel) is what this test is actually proving
	const grace = 2 * time.Second
	done := runServeAsync(ctx, cancel, listener, httpServer, background, db, grace)

	select {
	case err := <-done:
		require.Error(t, err, "Serve on an already-closed listener must surface an error")
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within a bounded multiple of the grace period")
	}
	assert.True(t, backgroundFinished(), "background must have observed ctx cancellation -- on this branch, only serve's stop() call causes that")
	assert.True(t, dbClosed())
}

// TestNewListener_ClosesInheritedSourceFileDescriptor is the discriminating
// regression proof for the fd leak a review caught in newListener: without
// closing the *os.File wrapping the inherited fd once net.FileListener has
// dup'd it, that source file -- and, since it is a dup of the same
// underlying socket, the fd itself -- stays open for this whole process's
// lifetime, keeping the port in LISTEN even after the returned Listener's
// own Close. It reproduces the inherited-fd path directly in-process
// (setting listenerFDEnv to a real, already-open fd, exactly as
// cmd/server/main_integration_test.go's startServer does across a process
// boundary) rather than through a subprocess, so it stays fast and
// container-free.
func TestNewListener_ClosesInheritedSourceFileDescriptor(t *testing.T) {
	source, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := source.Addr().String()
	file, err := source.(*net.TCPListener).File()
	require.NoError(t, err)
	require.NoError(t, source.Close()) // file holds its own dup; the port stays bound
	// Ownership rule: exactly one *os.File per live fd number, always.
	// Handing file's own fd straight to newListener via listenerFDEnv
	// would leave TWO *os.File Go values -- this test's file and the one
	// newListener builds with os.NewFile -- both wrapping the SAME fd
	// number. newListener closes its copy deterministically (correct: see
	// its own doc comment), but file's finalizer is still armed; whenever
	// the GC gets around to collecting file, the finalizer closes that fd
	// number a SECOND time, by which point the OS may have handed the
	// number back out to an unrelated, live socket elsewhere in this test
	// binary -- corrupting or killing that connection nondeterministically
	// (loam-6ob). Dup the fd so newListener and this test each own a
	// distinct fd number, with exactly one *os.File -- and one finalizer
	// -- per number.
	nfd, err := syscall.Dup(int(file.Fd()))
	require.NoError(t, err)
	t.Setenv(listenerFDEnv, strconv.Itoa(nfd)) // newListener owns and closes nfd
	// file's own fd (N) is a separate dup of the same underlying open file
	// description as nfd -- closing nfd alone does not release the port,
	// since N is still an open reference to it. Close file deterministically
	// here, now that nfd exists as an independent handle for newListener to
	// take ownership of, rather than deferring to the end of the test: the
	// later "port must be free" assertion below depends on N actually being
	// closed by the time it runs, not merely scheduled to close on return.
	require.NoError(t, file.Close())
	listener, err := newListener("unused-when-" + listenerFDEnv + "-is-set")
	require.NoError(t, err)
	// Confirm newListener closed its own *os.File wrapping nfd once
	// net.FileListener had dup'd it, via a raw Fstat on the fd number --
	// not by wrapping nfd in a fresh *os.File, which would just
	// reintroduce the same double-owned-fd hazard for this assertion.
	var stat syscall.Stat_t
	statErr := syscall.Fstat(nfd, &stat)
	assert.Error(t, statErr, "newListener must close its own *os.File wrapping the inherited fd once net.FileListener has dup'd it, not leak it")
	require.NoError(t, listener.Close())
	relisten, err := net.Listen("tcp", addr)
	require.NoError(t, err, "the port must be free once the returned listener closes -- a leaked source fd duplicate would keep it in LISTEN")
	require.NoError(t, relisten.Close())
}

// TestOSFileDoubleClose_ClobbersReusedDescriptor is a deterministic,
// mechanism-level reproduction of loam-6ob: two independent *os.File Go
// values wrapping the SAME raw fd number (exactly the shape
// TestNewListener_ClosesInheritedSourceFileDescriptor had before its dup
// fix -- newListener's os.NewFile(fd) as one owner, the test's own file as
// the other), where the first owner closes explicitly and the second's
// runtime finalizer only fires later, after the OS has already handed that
// fd number to something unrelated. It does not depend on GC ever
// happening to race a live connection during a real test run -- it forces
// the collision on purpose, with a finalizer completion barrier instead of
// a bare runtime.GC() call, so it fails every time the double-ownership
// pattern is present rather than ~2 times in 10.
func TestOSFileDoubleClose_ClobbersReusedDescriptor(t *testing.T) {
	owner1, err := os.Open(os.DevNull)
	require.NoError(t, err)
	fd := owner1.Fd()
	owner2 := os.NewFile(fd, "ghost-owner") // no dup: the same fd number, a second unrelated Go value
	require.NoError(t, owner1.Close())      // the "newListener" role: explicit, correct close of its own copy
	bystander, err := os.Open(os.DevNull)
	require.NoError(t, err)
	defer bystander.Close()
	require.Equal(t, fd, bystander.Fd(), "POSIX guarantees the lowest free fd is reused; if this fails, the reuse assumption itself does not hold on this platform")
	finalized := make(chan struct{})
	runtime.SetFinalizer(owner2, func(f *os.File) {
		_ = f.Close()
		close(finalized)
	})
	owner2 = nil
	runtime.GC()
	select {
	case <-finalized:
	case <-time.After(5 * time.Second):
		t.Fatal("ghost-owner's finalizer did not run -- cannot demonstrate the collision")
	}
	_, statErr := bystander.Stat()
	assert.Error(t, statErr, "the finalizer closing a stale, already-reassigned fd number must have clobbered the bystander's descriptor -- this is loam-6ob's exact mechanism")
}
