package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/ingest/embed/ollama"
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
	assert.Equal(t, defaultMaxAttempts, pool.maxAttempts)
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

// TestWithMaxAttempts_OverridesTheDefault is the retry-ceiling counterpart
// to TestWithBackoff_OverridesBothBounds: a test harness needs a way to
// shrink the ceiling down from defaultMaxAttempts without waiting out ten
// real backoff cycles.
func TestWithMaxAttempts_OverridesTheDefault(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, nil, 1, WithMaxAttempts(3))
	assert.Equal(t, 3, pool.maxAttempts)
}

// TestWithMaxAttempts_ClampsNonPositiveToTheFloor pins minMaxAttempts's
// doc comment: a zero or negative ceiling must not reach abandonReason as
// "never retry at all" by accident -- see minMaxAttempts's rationale for
// why that would over-correct the exact bug this bead fixes.
func TestWithMaxAttempts_ClampsNonPositiveToTheFloor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		n    int
		want int
	}{
		{name: "zero", n: 0, want: minMaxAttempts},
		{name: "negative", n: -5, want: minMaxAttempts},
		{name: "valid value passes through unchanged", n: 7, want: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool := NewPool(testLogger(), nil, nil, 1, WithMaxAttempts(tt.n))
			assert.Equal(t, tt.want, pool.maxAttempts)
		})
	}
}

// newOllamaPermanentError returns a genuine error produced by the real
// ollama.Embedder against a 4xx response. ollama's own classification
// sentinels are unexported, so the only way a test outside that package can
// produce a value ollama.IsPermanent actually recognizes is to trigger the
// real classification path, the same way ollama's own
// TestEmbed_StatusClassification does from inside that package.
func newOllamaPermanentError(t *testing.T) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid request shape"}`))
	}))
	t.Cleanup(server.Close)
	e, err := ollama.New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	_, embedErr := e.Embed(t.Context(), []string{"hello"})
	require.Error(t, embedErr)
	require.True(t, ollama.IsPermanent(embedErr), "test setup must actually produce a permanent-classified error")
	return embedErr
}

// newOllamaTransientError is newOllamaPermanentError's sibling for a
// genuinely retryable ollama classification (a 5xx), so abandonReason's
// transient tests exercise the real predicate rather than a stand-in.
func newOllamaTransientError(t *testing.T) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"server busy"}`))
	}))
	t.Cleanup(server.Close)
	e, err := ollama.New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	_, embedErr := e.Embed(t.Context(), []string{"hello"})
	require.Error(t, embedErr)
	require.False(t, ollama.IsPermanent(embedErr), "test setup must actually produce a non-permanent error")
	require.True(t, ollama.IsRetryable(embedErr), "test setup must actually produce a retryable error")
	return embedErr
}

// TestAbandonReason_PermanentClassificationStopsEvenOnTheFirstAttempt is
// the loam-eean core unit for the "skip retries entirely" decision: a
// permanently-classified embedder failure (bad credentials, a model that
// will never accept the input) must not be retried, even before the
// general attempts ceiling would ever fire -- burning the whole retry
// budget on a request already known to fail identically every time is
// pure waste, not caution.
func TestAbandonReason_PermanentClassificationStopsEvenOnTheFirstAttempt(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, nil, 1)
	reason := pool.abandonReason(1, newOllamaPermanentError(t))
	assert.NotEmpty(t, reason, "a permanently classified embedder failure must not be retried, even on attempt 1")
}

// TestAbandonReason_TransientEmbedderFailureRetriesBelowTheCeiling is the
// mutation-catching counterpart: without it, an abandonReason that stopped
// retrying for ANY ollama-produced error (not only the permanent ones)
// would still pass the test above.
func TestAbandonReason_TransientEmbedderFailureRetriesBelowTheCeiling(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, nil, 1)
	reason := pool.abandonReason(1, newOllamaTransientError(t))
	assert.Empty(t, reason, "a transient embedder failure must retry, not be abandoned")
}

// TestAbandonReason_UnrelatedErrorIsNotMisclassifiedAsPermanent pins the
// exact failure mode ollama.IsPermanent's own doc comment warns against: an
// error from a wholly different subsystem (a DB lock-contention failure, a
// git-mirror read failure) matches none of ollama's sentinels, so it must
// be treated as unclassified-but-retryable -- not silently abandoned on its
// first attempt the way a naive `!ollama.IsRetryable(err)` would, which
// would break the bead's "lock contention should retry" requirement.
func TestAbandonReason_UnrelatedErrorIsNotMisclassifiedAsPermanent(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, nil, 1)
	reason := pool.abandonReason(1, errors.New("lock contention acquiring the ingest_jobs row"))
	assert.Empty(t, reason, "an error this codebase's embedder never produced must not be treated as permanent")
}

// TestAbandonReason_CeilingBoundary_OneBelowRetriesExactlyAtStops is the
// off-by-one guard for the general attempts ceiling: at maxAttempts-1 the
// job must still retry (one more attempt is exactly what the ceiling
// allows), and at maxAttempts it must stop -- both directions asserted
// against the SAME unrelated, unclassified error so the only variable
// between the two assertions is attempts, not the error's classification.
func TestAbandonReason_CeilingBoundary_OneBelowRetriesExactlyAtStops(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, nil, 1, WithMaxAttempts(5))
	runErr := errors.New("embedder unreachable: connection refused")
	assert.Empty(t, pool.abandonReason(4, runErr), "attempts one below the ceiling must still retry")
	assert.NotEmpty(t, pool.abandonReason(5, runErr), "attempts at the ceiling must stop retrying")
}

// TestAbandonReason_OnePastTheCeilingAlsoStops is the mutation-catching
// companion to the boundary test above: without it, an off-by-one that
// compared attempts > maxAttempts (instead of >=) would still pass the
// exactly-at-ceiling case above, since it never checks past it.
func TestAbandonReason_OnePastTheCeilingAlsoStops(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, nil, 1, WithMaxAttempts(5))
	assert.NotEmpty(t, pool.abandonReason(6, errors.New("still failing")), "attempts past the ceiling must never retry again")
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

// capturingHandler is a minimal, thread-safe slog.Handler used in place of
// testLogger's io.Discard sink wherever a test must assert on LOG CONTENT
// -- not merely on survival. It captures every record verbatim (Clone, so
// a record's lazily-evaluated attrs are safe to read after Handle
// returns) for the test to inspect once the call under test has returned.
// Same shape as internal/mirrorsync/scheduler_test.go's own copy -- kept
// as a package-local duplicate rather than a shared test helper package,
// matching that file's precedent.
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

// findLevel reports whether any captured record has BOTH the given
// message and the given level.
//
// find (above) matches on message alone, which cannot express the
// assertion loam-54o.17 needs. That bead's whole subject is a behaviour
// that was already "logged" before the fix and is still "logged" after it
// -- what changed is the LEVEL, from work()'s ERROR (ordinary contention
// reported as a fault, the symptom operators learn to ignore) to DEBUG.
// A message-only assertion passes identically either way, so the
// deliberate decision would have been documented in a doc comment and
// guarded by nothing. Asserting the true case AND the false case at the
// wrong level pins it in both directions.
func (h *capturingHandler) findLevel(msg string, level slog.Level) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == msg && r.Level == level {
			return true
		}
	}
	return false
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

// TestWork_SurvivesAndLogsAPanicWhileClaiming is loam-jy0p's proof for the
// first of the two goroutines loam-lae's close notes flagged as sitting
// outside run()'s guard: work()'s claim loop runs BEFORE run() is ever
// called, so a panic in claim() (a driver error mishandled scanning a
// claimed row, say) has no per-job boundary underneath it. Before this
// fix, that panic unwound straight out of work() -- a goroutine root Run
// spawned directly -- and crashed the whole process.
//
// The injected fault is a nil *pgxpool.Pool, the same technique
// TestRun_SurvivesAPanicWhileRecordingTheOutcome above already uses to
// reach a panic inside this package's DB calls without a real Postgres:
// claim's very first statement is p.db.Begin(ctx), which dereferences the
// nil pool.
//
// require.NotPanics is what makes this test kill the mutation "delete the
// recover" with an ASSERTION rather than a crash of the whole `go test`
// binary (every sibling test in this package would go down with it,
// exactly the production failure mode this bead removes).
func TestWork_SurvivesAndLogsAPanicWhileClaiming(t *testing.T) {
	t.Parallel()
	handler, logger := newCapturingHandler()
	pool := NewPool(logger, nil, &OrchestratorMock{}, 1)
	pool.wg.Add(1)
	require.NotPanics(t, func() { pool.work(t.Context(), 3) },
		"a panic while claiming a job must not escape work() -- Run spawns work as a goroutine root with no guard of its own")
	rec, found := handler.find("recovered panic in ingest worker; this worker has permanently stopped claiming jobs")
	require.True(t, found, "a recovered worker panic must be logged, not silently dropped")
	assert.Equal(t, "3", attrString(rec, "worker_index"), "the log must identify WHICH worker died")
	assert.NotEmpty(t, attrString(rec, "panic"), "the log must carry the recovered value")
	assert.NotEmpty(t, attrString(rec, "stack"), "the log must carry a stack trace -- a recovered panic that loses its stack is harder to diagnose than a crash")
}

// TestScheduleRetry_SurvivesAndLogsAPanicRequeueing is loam-jy0p's proof
// for the second flagged goroutine: fail() spawns scheduleRetry as its own
// detached goroutine (`go p.scheduleRetry(...)`), so a panic in its UPDATE
// -- a driver error, say -- has no per-job boundary above this frame
// either, and previously took down the whole process just like work's.
//
// Same nil-*pgxpool.Pool technique as the test above: scheduleRetry's
// only DB call, p.db.Exec, dereferences the nil pool once the timer
// fires. delay is effectively zero so the test does not wait out a real
// backoff.
func TestScheduleRetry_SurvivesAndLogsAPanicRequeueing(t *testing.T) {
	t.Parallel()
	handler, logger := newCapturingHandler()
	pool := NewPool(logger, nil, &OrchestratorMock{}, 1)
	jobID := uuid.New()
	pool.wg.Add(1)
	require.NotPanics(t, func() { pool.scheduleRetry(t.Context(), jobID, time.Millisecond) },
		"a panic while scheduling a retry must not escape scheduleRetry -- fail() spawns it as a detached goroutine with no guard of its own")
	rec, found := handler.find("recovered panic scheduling ingest job retry; this job will not return to queued")
	require.True(t, found, "a recovered scheduleRetry panic must be logged, not silently dropped")
	assert.Equal(t, jobID.String(), attrString(rec, "job_id"), "the log must identify WHICH job's retry died")
	assert.NotEmpty(t, attrString(rec, "panic"), "the log must carry the recovered value")
	assert.NotEmpty(t, attrString(rec, "stack"), "the log must carry a stack trace -- a recovered panic that loses its stack is harder to diagnose than a crash")
}

// TestIsRunningPerRepoViolation_MatchesTheGuardConstraintByName is the
// classification loam-54o.17 turns on: claim treats one specific unique
// violation as ordinary contention and every other error as a failure, so
// this function deciding wrongly in either direction is the whole bug.
// A false negative puts a normal concurrent claim back on work()'s ERROR
// path (the production symptom the bead exists to prevent); a false
// positive silently swallows a genuine duplicate-key defect as "someone
// beat me", which is strictly worse than the error it replaced.
//
// The bare-SQLSTATE case is the one that matters most: 23505 with any
// OTHER constraint name must NOT match. ingest_jobs already has a primary
// key, and this repo classifies unique violations by name everywhere else
// (roles_name_key, repos_name_key), so "any 23505 during a claim is
// contention" would be both inconsistent and wrong.
func TestIsRunningPerRepoViolation_MatchesTheGuardConstraintByName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "the guard constraint's own unique violation",
			err:  &pgconn.PgError{Code: "23505", ConstraintName: "ingest_jobs_one_running_per_repo"},
			want: true,
		},
		{
			name: "wrapped in claimOnce's own error context, as claim actually receives it",
			err:  fmt.Errorf("marking ingest job %s running: %w", uuid.New(), &pgconn.PgError{Code: "23505", ConstraintName: "ingest_jobs_one_running_per_repo"}),
			want: true,
		},
		{
			name: "a unique violation on ingest_jobs' primary key is a real defect, not contention",
			err:  &pgconn.PgError{Code: "23505", ConstraintName: "ingest_jobs_pkey"},
			want: false,
		},
		{
			name: "a unique violation on an unrelated table's constraint",
			err:  &pgconn.PgError{Code: "23505", ConstraintName: "roles_name_key"},
			want: false,
		},
		{
			name: "a 23505 carrying no constraint name at all",
			err:  &pgconn.PgError{Code: "23505"},
			want: false,
		},
		{
			name: "the right constraint name under a different SQLSTATE",
			err:  &pgconn.PgError{Code: "23503", ConstraintName: "ingest_jobs_one_running_per_repo"},
			want: false,
		},
		{
			name: "a check-constraint violation on the same table",
			err:  &pgconn.PgError{Code: "23514", ConstraintName: "ingest_jobs_status_check"},
			want: false,
		},
		{
			name: "an ordinary non-pg error",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
		{
			name: "pgx.ErrNoRows, which claim handles on a different path entirely",
			err:  pgx.ErrNoRows,
			want: false,
		},
		{
			name: "no error at all",
			err:  nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isRunningPerRepoViolation(tt.err))
		})
	}
}

// TestClaimQuery_CarriesTheQueuedAtIDTiebreak pins loam-c94.18's fix into
// the statement text. A bare ORDER BY queued_at is not wrong in a way any
// single-row test can observe -- it only misbehaves when two rows share a
// queued_at, and then only by picking an arbitrary one -- so the
// integration test that forces a tie (TestClaim_QueuedAtTie_BreaksOnID)
// can pass by luck on a Postgres that happens to return them in id order.
// This assertion cannot.
func TestClaimQuery_CarriesTheQueuedAtIDTiebreak(t *testing.T) {
	t.Parallel()
	assert.Contains(t, claimQuery, "ORDER BY j.queued_at, j.id",
		"claimQuery must break a queued_at tie on id, matching ClaimIngestJob and ListIngestJobs")
}

// TestClaimQuery_FiltersReposWithARunningJob pins the cross-process
// avoidance filter, for the same reason as the test above: its absence is
// invisible to any test that does not race two writers, because the
// constraint would catch the collision anyway -- just one wasted round
// trip and one DEBUG line later.
func TestClaimQuery_FiltersReposWithARunningJob(t *testing.T) {
	t.Parallel()
	assert.Contains(t, claimQuery, "NOT EXISTS",
		"claimQuery must skip repos that already have a committed running job, whoever started it")
	assert.Contains(t, claimQuery, "running.status = 'running'",
		"the NOT EXISTS must key on status = 'running', the same predicate ingest_jobs_one_running_per_repo indexes")
}

// TestMaxClaimAttempts_IsBoundedAndSmall guards the two properties
// maxClaimAttempts' doc comment argues for: the retry is bounded at all
// (an unbounded loop against a constraint that keeps rejecting is a
// livelock, and this one holds mu while it spins), and the bound stays
// small enough that holding mu through it is not itself the outage. It is
// deliberately a range, not an equality: the exact number is a judgement
// call this test should not freeze, but "someone raised it to 500" and
// "someone set it to 0" both need to fail here.
func TestMaxClaimAttempts_IsBoundedAndSmall(t *testing.T) {
	t.Parallel()
	assert.GreaterOrEqual(t, maxClaimAttempts, 2,
		"one attempt is not a retry -- a single lost race would report nothing to claim while other repos' jobs sit queued")
	assert.LessOrEqual(t, maxClaimAttempts, 10,
		"claim holds mu for every attempt, blocking every other worker in this process from claiming anything")
}
