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
