package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingRunner is a runner whose Run blocks until ctx is canceled, then
// records that it returned -- enough to prove multiRunner starts every
// member and waits for all of them.
type recordingRunner struct {
	started  atomic.Bool
	finished atomic.Bool
}

func (r *recordingRunner) Run(ctx context.Context) {
	r.started.Store(true)
	<-ctx.Done()
	r.finished.Store(true)
}

// TestMultiRunner_RunsEveryMemberAndWaitsForAll proves multiRunner starts
// every member concurrently and does not return until ALL of them have --
// not just the first, and not before any of them.
func TestMultiRunner_RunsEveryMemberAndWaitsForAll(t *testing.T) {
	t.Parallel()
	a := &recordingRunner{}
	b := &recordingRunner{}
	m := newMultiRunner(testLogger(), member{name: "a", runner: a}, member{name: "b", runner: b})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	// Give both members a moment to actually start before canceling --
	// this also proves both were started, not just one.
	for !a.started.Load() || !b.started.Load() {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("multiRunner.Run did not return after ctx was canceled")
	}
	assert.True(t, a.finished.Load(), "every member's Run must have returned before multiRunner.Run returns")
	assert.True(t, b.finished.Load(), "every member's Run must have returned before multiRunner.Run returns")
}

// TestMultiRunner_EmptyIsANoOp proves a multiRunner with zero members
// returns immediately rather than blocking forever.
func TestMultiRunner_EmptyIsANoOp(t *testing.T) {
	t.Parallel()
	m := newMultiRunner(testLogger())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an empty multiRunner must return immediately")
	}
}

// TestMultiRunner_OneSlowMemberStillBlocksTheWholeGroup proves the "wait
// for ALL, not just the first to finish" contract concretely: a WaitGroup
// misuse (e.g. only waiting on the first member) would let this test's
// second, deliberately-slower member's completion race the outer
// goroutine's own bookkeeping -- asserting via a shared counter, not
// timing, is what makes this deterministic.
func TestMultiRunner_OneSlowMemberStillBlocksTheWholeGroup(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var finishOrder []string
	fast := runnerFunc(func(ctx context.Context) {
		<-ctx.Done()
		mu.Lock()
		finishOrder = append(finishOrder, "fast")
		mu.Unlock()
	})
	slow := runnerFunc(func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		finishOrder = append(finishOrder, "slow")
		mu.Unlock()
	})
	m := newMultiRunner(testLogger(), member{name: "fast", runner: fast}, member{name: "slow", runner: slow})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("multiRunner.Run did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"fast", "slow"}, finishOrder, "multiRunner.Run must not return until the SLOWEST member has also finished")
}

// runnerFunc adapts a plain func(context.Context) to the runner interface,
// for this test file's own fixtures.
type runnerFunc func(ctx context.Context)

func (f runnerFunc) Run(ctx context.Context) { f(ctx) }

// capturingHandler is a minimal, thread-safe slog.Handler used in place of
// testLogger's io.Discard sink wherever a test must assert on LOG CONTENT
// -- not merely on survival. It captures every record verbatim (Clone, so
// a record's lazily-evaluated attrs are safe to read after Handle
// returns) for the test to inspect once the call under test has returned.
// This is its own copy rather than a shared helper: internal/mirrorsync's
// scheduler_test.go has the identical type for the same reason, but it is
// unexported in a different package, and this bead's scope does not
// include inventing a shared test-support package for one small handler.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func newCapturingHandler() (*capturingHandler, *slog.Logger) {
	h := &capturingHandler{}
	return h, slog.New(h)
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// find returns the first captured record with the given message, or false
// if none has been written.
func (h *capturingHandler) find(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

// attrString returns the string form of key's value on r, or "" if key was
// never set -- good enough for these tests, which only assert non-empty /
// Contains, never an exact numeric or structured comparison.
func attrString(r slog.Record, key string) string {
	var val string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val = a.Value.String()
			return false
		}
		return true
	})
	return val
}

// TestMultiRunner_MemberPanicIsRecoveredLoggedAndTheGroupSurvives is
// loam-lae's core proof for this file's site: a panic out of one member's
// Run (e.g. ingest.Pool.Run's claim loop, or hooksocket.Server.Run) must
// not escape Run and kill the whole process, must be logged with enough
// detail to diagnose (which member, the recovered value, a stack trace),
// and -- unlike mirrorsync's per-cycle recover, which lets the SAME repo
// retry on the next tick -- necessarily means that one member's Run has
// permanently stopped. The proof of that trade-off is the OTHER,
// still-healthy member: it must keep running regardless, and
// multiRunner.Run itself must not return until ctx is canceled, not the
// instant the panicking member dies.
func TestMultiRunner_MemberPanicIsRecoveredLoggedAndTheGroupSurvives(t *testing.T) {
	t.Parallel()
	handler, logger := newCapturingHandler()
	wantPanic := "collaborator exploded"
	boom := runnerFunc(func(ctx context.Context) { panic(wantPanic) })
	survivor := &recordingRunner{}
	m := newMultiRunner(logger, member{name: "doomed member", runner: boom}, member{name: "survivor", runner: survivor})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	for !survivor.started.Load() {
		time.Sleep(time.Millisecond)
	}
	// multiRunner.Run must NOT have returned yet: the doomed member's
	// panic is recovered on its own goroutine, independent of the
	// survivor's. If it instead tore down the whole group (e.g. by
	// re-panicking, or by some Wait-related bug), done would already be
	// closed here, since the survivor is still deliberately blocked on
	// ctx and has not itself returned.
	select {
	case <-done:
		t.Fatal("multiRunner.Run returned before ctx was canceled -- one member's panic must not tear down the whole group")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("multiRunner.Run did not return after ctx was canceled")
	}
	assert.True(t, survivor.finished.Load(), "the surviving member's Run must have completed normally, undisturbed by the other member's panic")
	rec, found := handler.find("background runner member panicked and has permanently stopped; the process keeps serving reads but this subsystem is now silently dead")
	require.True(t, found, "a recovered member panic must be logged, not silently dropped")
	assert.Equal(t, "doomed member", attrString(rec, "member"), "the log must identify WHICH member panicked")
	assert.Contains(t, attrString(rec, "panic"), wantPanic, "the log must carry the recovered value")
	assert.NotEmpty(t, attrString(rec, "stack"), "the log must carry a stack trace -- a recovered panic that loses its stack is harder to diagnose than a crash")
}
