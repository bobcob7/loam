package ingest

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestBackoffDelay_DoublesPerAttemptUpToCap proves the pure backoff math:
// the first failure (attempts=1) waits base, each further failure doubles
// the wait, and the wait never exceeds max. This is the "increasing
// delay" half of the bead's ACCEPTANCE CRITERIA that does not need a
// database to verify.
func TestBackoffDelay_DoublesPerAttemptUpToCap(t *testing.T) {
	t.Parallel()
	base := 1 * time.Second
	max := 10 * time.Second
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 1, want: 1 * time.Second},
		{attempts: 2, want: 2 * time.Second},
		{attempts: 3, want: 4 * time.Second},
		{attempts: 4, want: 8 * time.Second},
		{attempts: 5, want: 10 * time.Second},
		{attempts: 6, want: 10 * time.Second},
		{attempts: 100, want: 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.want.String(), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, backoffDelay(tt.attempts, base, max))
		})
	}
}

// TestBackoffDelay_ZeroAttemptsStillBounded guards the degenerate input
// (attempts=0, which should never happen since fail() always increments
// before computing delay) against a negative or unbounded result.
func TestBackoffDelay_ZeroAttemptsStillBounded(t *testing.T) {
	t.Parallel()
	got := backoffDelay(0, 1*time.Second, 10*time.Second)
	assert.Equal(t, 1*time.Second, got)
}

// TestBackoffDelay_FloorBaseStillDoublesSensibly pins backoffDelay's own
// math at the exact floor WithBackoff clamps a non-positive base to
// (minBackoffBase): the doubling and cap still behave like any other
// positive base, just at a smaller scale a test harness can actually wait
// out. This is backoffDelay's side of the WithBackoff clamp fix -- the
// clamp only helps if the floor it picks still produces a real,
// increasing delay rather than another degenerate case.
func TestBackoffDelay_FloorBaseStillDoublesSensibly(t *testing.T) {
	t.Parallel()
	max := 10 * minBackoffBase
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 1, want: minBackoffBase},
		{attempts: 2, want: 2 * minBackoffBase},
		{attempts: 3, want: 4 * minBackoffBase},
		{attempts: 4, want: 8 * minBackoffBase},
		{attempts: 5, want: 10 * minBackoffBase},
		{attempts: 100, want: 10 * minBackoffBase},
	}
	for _, tt := range tests {
		t.Run(tt.want.String(), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, backoffDelay(tt.attempts, minBackoffBase, max))
		})
	}
}

// TestNewPool_ClampsNonPositiveWorkersToOne proves a misconfigured
// LOAM_INGEST_WORKERS (0 or negative) still yields a pool that makes
// progress instead of one that starts zero worker goroutines and hangs
// forever.
func TestNewPool_ClampsNonPositiveWorkersToOne(t *testing.T) {
	t.Parallel()
	for _, workers := range []int{0, -1, -5} {
		pool := NewPool(testLogger(), nil, nil, workers)
		assert.Equal(t, 1, pool.workers)
	}
}

// TestNewPool_PreservesConfiguredWorkerCount is the mutation-catching
// counterpart to the clamp test above: without it, hardcoding workers=1
// unconditionally in NewPool would still pass every other test in this
// file.
func TestNewPool_PreservesConfiguredWorkerCount(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, nil, 7)
	assert.Equal(t, 7, pool.workers)
}

// TestNewPool_DefaultsWithoutOptions pins the zero-option construction
// path so a later change to the option-application order (e.g.
// accidentally applying opts before the defaults, silently discarding
// them) has a test to fail.
func TestNewPool_DefaultsWithoutOptions(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, nil, 1)
	assert.Equal(t, defaultPollInterval, pool.pollInterval)
	assert.Equal(t, defaultBackoffBase, pool.backoffBase)
	assert.Equal(t, defaultBackoffMax, pool.backoffMax)
}

// TestWithBackoff_OverridesBothBounds is FIX 3's core seam: a test in
// another package (internal/testsched's "Manual scheduler" harness) has
// no way to reach Pool's unexported backoff fields directly and must go
// through this option.
func TestWithBackoff_OverridesBothBounds(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, nil, 1, WithBackoff(10*time.Millisecond, 40*time.Millisecond))
	assert.Equal(t, 10*time.Millisecond, pool.backoffBase)
	assert.Equal(t, 40*time.Millisecond, pool.backoffMax)
}

// TestWithPollInterval_OverridesTheDefault is WithBackoff's counterpart
// for the fallback poll cadence.
func TestWithPollInterval_OverridesTheDefault(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, nil, 1, WithPollInterval(250*time.Millisecond))
	assert.Equal(t, 250*time.Millisecond, pool.pollInterval)
}

// TestWithPollInterval_ClampsNonPositiveToTheFloor pins the review
// finding that WithPollInterval(0) (or any non-positive value) reached
// time.NewTicker directly and panicked ("PANIC: non-positive interval for
// NewTicker") the moment Run started a worker -- a harder failure than
// the backoff hot loop WithBackoff's clamp fixes, and just as reachable:
// a caller reaching for 0 to mean "never poll, rely purely on the wake
// channel" is a natural mistake for exactly the harness this option
// exists to serve. Sibling to TestWithBackoff_ClampsNonPositiveAndInvertedBounds.
func TestWithPollInterval_ClampsNonPositiveToTheFloor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    time.Duration
		want time.Duration
	}{
		{name: "zero", d: 0, want: minPollInterval},
		{name: "negative", d: -1 * time.Second, want: minPollInterval},
		{name: "valid value below the floor is still clamped up", d: 1 * time.Millisecond, want: minPollInterval},
		{name: "valid value above the floor passes through unchanged", d: 250 * time.Millisecond, want: 250 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool := NewPool(testLogger(), nil, nil, 1, WithPollInterval(tt.d))
			assert.Equal(t, tt.want, pool.pollInterval)
			assert.Positive(t, pool.pollInterval, "a non-positive interval must never reach time.NewTicker")
		})
	}
}

// TestWithBackoff_ClampsNonPositiveAndInvertedBounds pins the review
// finding that WithBackoff(0, 0) (or any non-positive base) silently gave
// backoffDelay(attempts, 0, 0) == 0 at every attempt: time.NewTimer(0)
// fires immediately, so a permanently-failing job in scheduleRetry became
// an unbounded hot loop against Postgres instead of a backoff. A negative
// base is worse -- backoffDelay's doubling runs backwards, growing more
// negative with attempts. WithBackoff(0, 5*time.Minute) is the case that
// reads safest and is equally broken: doubling a zero base stays zero
// regardless of max.
func TestWithBackoff_ClampsNonPositiveAndInvertedBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		base, max time.Duration
		wantBase  time.Duration
		wantMax   time.Duration
	}{
		{name: "zero base and max", base: 0, max: 0, wantBase: minBackoffBase, wantMax: minBackoffBase},
		{name: "negative base and max", base: -1 * time.Second, max: -1 * time.Second, wantBase: minBackoffBase, wantMax: minBackoffBase},
		{name: "zero base with a generous max", base: 0, max: 5 * time.Minute, wantBase: minBackoffBase, wantMax: 5 * time.Minute},
		{name: "max smaller than base", base: 10 * time.Millisecond, max: 5 * time.Millisecond, wantBase: 10 * time.Millisecond, wantMax: 10 * time.Millisecond},
		{name: "valid bounds pass through unchanged", base: 50 * time.Millisecond, max: 200 * time.Millisecond, wantBase: 50 * time.Millisecond, wantMax: 200 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool := NewPool(testLogger(), nil, nil, 1, WithBackoff(tt.base, tt.max))
			assert.Equal(t, tt.wantBase, pool.backoffBase)
			assert.Equal(t, tt.wantMax, pool.backoffMax)
			assert.GreaterOrEqual(t, pool.backoffMax, pool.backoffBase, "max must never end up below base")
			assert.Positive(t, backoffDelay(1, pool.backoffBase, pool.backoffMax), "the resulting bounds must never yield a zero or negative delay")
		})
	}
}

// TestRunOrchestrator_ConvertsAPanicIntoAJobError is loam-337's core unit:
// a panicking Orchestrator must come back as an ordinary error, so run()
// routes it into the existing fail() path with no panic-specific branch.
//
// require.NotPanics is what makes this test kill the mutation "delete the
// recover" with an ASSERTION rather than by crashing the test binary: it
// installs its own recover, so an escaping panic is reported as a failed
// assertion here instead of taking the whole `go test` process (and every
// sibling test in this package) down with it -- which is precisely the
// production failure mode this bead exists to remove.
//
// db is nil and never touched: runOrchestrator does no I/O.
func TestRunOrchestrator_ConvertsAPanicIntoAJobError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		boom        func()
		wantMessage string
	}{
		{name: "string value", boom: func() { panic("chunker exploded") }, wantMessage: "chunker exploded"},
		{name: "error value", boom: func() { panic(assert.AnError) }, wantMessage: assert.AnError.Error()},
		{name: "non-string value", boom: func() { panic([]int{1, 2}) }, wantMessage: "[1 2]"},
		{name: "zero value", boom: func() { panic(0) }, wantMessage: "0"},
		{
			// The loam-c94.11 shape: an index-out-of-range in the vector
			// write path, raised by the runtime rather than by a literal
			// panic() call, and found only by mutation testing.
			name:        "runtime index-out-of-range",
			boom:        func() { vec := []float32{}; _ = vec[3] },
			wantMessage: "index out of range [3] with length 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			orch := &OrchestratorMock{
				RunFunc: func(ctx context.Context, job Job) (Stats, error) {
					tt.boom()
					return Stats{}, nil
				},
			}
			pool := NewPool(testLogger(), nil, orch, 1)
			job := Job{ID: uuid.New(), RepoID: uuid.New(), TargetBranch: "main", Kind: KindFull}
			var stats Stats
			var err error
			require.NotPanics(t, func() { stats, err = pool.runOrchestrator(t.Context(), job) },
				"a panicking orchestrator must not escape the per-job boundary -- if it does, the server process dies")
			require.Error(t, err, "a recovered panic must be reported as a job error, not swallowed into a success")
			assert.ErrorIs(t, err, errJobPanicked)
			assert.Contains(t, err.Error(), tt.wantMessage, "the recovered value must survive into the message fail() records")
			assert.Equal(t, Stats{}, stats, "a panicked job reports no stats")
		})
	}
}

// TestRunOrchestrator_PassesASuccessfulRunThrough is the mutation-catching
// counterpart to the test above: without it, a runOrchestrator that
// unconditionally returned an error (or dropped the orchestrator's stats)
// would still satisfy every panic assertion.
func TestRunOrchestrator_PassesASuccessfulRunThrough(t *testing.T) {
	t.Parallel()
	want := Stats{FilesParsed: 3, ChunksEmbedded: 9}
	var gotJob Job
	orch := &OrchestratorMock{
		RunFunc: func(ctx context.Context, job Job) (Stats, error) {
			gotJob = job
			return want, nil
		},
	}
	pool := NewPool(testLogger(), nil, orch, 1)
	job := Job{ID: uuid.New(), RepoID: uuid.New(), TargetBranch: "main", Kind: KindIncremental}
	stats, err := pool.runOrchestrator(t.Context(), job)
	require.NoError(t, err)
	assert.Equal(t, want, stats)
	assert.Equal(t, job, gotJob, "the claimed job must reach the orchestrator unmodified")
}

// TestRun_SurvivesAPanicWhileRecordingTheOutcome covers the second guard:
// a panic from succeed/fail themselves -- not from the pipeline -- must
// also not reach work() and kill the process.
//
// The injected fault is a nil *pgxpool.Pool, which is how every other unit
// test in this file constructs a Pool (Pool.db is a concrete
// *pgxpool.Pool with no querier seam a moq mock could stand in for). The
// orchestrator returns an ordinary error, so fail() runs and its very
// first database call dereferences the nil pool. That reproduces the exact
// shape of the hole -- a panic raised after the orchestrator has already
// returned -- without needing Postgres.
//
// The slot and waiter assertions are the leak checks the bead asks for,
// and they are only satisfiable because run registers release and
// notifyDrainWaiters as its own defers, before anything that can panic --
// when they lived inside succeed/fail, a panic that skipped both functions
// entirely would have skipped them too. The repo must not be left marked
// busy, or claim() skips it and every future job for it is unclaimable
// forever.
func TestRun_SurvivesAPanicWhileRecordingTheOutcome(t *testing.T) {
	t.Parallel()
	orch := &OrchestratorMock{
		RunFunc: func(ctx context.Context, job Job) (Stats, error) {
			return Stats{}, assert.AnError
		},
	}
	pool := NewPool(testLogger(), nil, orch, 1)
	job := Job{ID: uuid.New(), RepoID: uuid.New(), TargetBranch: "main", Kind: KindFull}
	pool.mu.Lock()
	pool.busy[job.RepoID] = struct{}{}
	waiter := make(chan struct{})
	pool.drainWaiters[job.RepoID] = append(pool.drainWaiters[job.RepoID], waiter)
	pool.mu.Unlock()
	require.NotPanics(t, func() { pool.run(t.Context(), job) },
		"a panic while recording a job's outcome must not escape run() -- work() has no guard of its own")
	pool.mu.Lock()
	_, stillBusy := pool.busy[job.RepoID]
	pool.mu.Unlock()
	assert.False(t, stillBusy, "the per-repo serialization slot must be released even when recording the outcome panicked")
	select {
	case <-waiter:
	default:
		t.Fatal("a DrainRepoID waiter registered for this repo was never woken -- a recovered panic that wedges the pool is barely better than the crash")
	}
}

// TestRun_ReleasesTheSlotAndWakesDrainWaitersOnAPipelinePanic is the
// same two leak checks for the ordinary panic path (the pipeline panics,
// runOrchestrator converts it, fail() records it). fail()'s database work
// panics here too because db is nil, so this test proves only the
// release/notify halves -- the persisted row is what the integration
// tests in integration_test.go assert against real Postgres.
func TestRun_ReleasesTheSlotAndWakesDrainWaitersOnAPipelinePanic(t *testing.T) {
	t.Parallel()
	orch := &OrchestratorMock{
		RunFunc: func(ctx context.Context, job Job) (Stats, error) {
			panic("tree-sitter cgo boundary")
		},
	}
	pool := NewPool(testLogger(), nil, orch, 1)
	job := Job{ID: uuid.New(), RepoID: uuid.New(), TargetBranch: "main", Kind: KindFull}
	pool.mu.Lock()
	pool.busy[job.RepoID] = struct{}{}
	waiter := make(chan struct{})
	pool.drainWaiters[job.RepoID] = append(pool.drainWaiters[job.RepoID], waiter)
	pool.mu.Unlock()
	require.NotPanics(t, func() { pool.run(t.Context(), job) })
	pool.mu.Lock()
	_, stillBusy := pool.busy[job.RepoID]
	pool.mu.Unlock()
	assert.False(t, stillBusy, "a panicking job must not leak its repo's serialization slot")
	select {
	case <-waiter:
	default:
		t.Fatal("a DrainRepoID waiter must be woken on the panic path too")
	}
}
