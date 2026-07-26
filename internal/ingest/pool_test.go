package ingest

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
