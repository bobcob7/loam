package ingest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// claimMeter builds a real SDK MeterProvider over a manual reader, so these
// tests read the metric back through the same aggregation production would,
// rather than through a fake counter that would only assert that the code
// calls the code.
func claimMeter(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { assert.NoError(t, mp.Shutdown(context.Background())) })
	return mp, reader
}

// claimCounts collects reader and returns the claim-cycle counter's value
// per outcome. A missing instrument returns an empty map rather than
// failing, so a caller can assert "nothing was recorded" -- which is a real
// expectation here, since every Pool built without WithMeterProvider holds
// no counter at all. cmd/server does pass the option (see WithMeterProvider's
// COMPOSITION ROOT note), but every other construction site in this package
// and its tests does not.
func claimCounts(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	counts := map[string]int64{}
	for _, scope := range rm.ScopeMetrics {
		if scope.Scope.Name != meterName {
			continue
		}
		for _, m := range scope.Metrics {
			if m.Name != claimCyclesMetric {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.Truef(t, ok, "%s must aggregate as an int64 sum, got %T", claimCyclesMetric, m.Data)
			assert.True(t, sum.IsMonotonic, "a claim-cycle count that can go down is not a count")
			for _, dp := range sum.DataPoints {
				outcome, ok := dp.Attributes.Value(attribute.Key(claimOutcomeAttribute))
				require.Truef(t, ok, "every data point must carry %s", claimOutcomeAttribute)
				assert.Equal(t, 1, dp.Attributes.Len(),
					"the claim counter has exactly one dimension; a second one is a cardinality decision nobody made")
				counts[outcome.AsString()] += dp.Value
			}
		}
	}
	return counts
}

// TestRecordClaim_SeparatesAllFourOutcomes is the shape of the replacement
// signal. The four values are asserted with DIFFERENT counts on purpose: a
// fixture that records one of each cannot tell a counter that attributes
// every increment to the right outcome apart from one that shuffles them.
func TestRecordClaim_SeparatesAllFourOutcomes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	mp, reader := claimMeter(t)
	pool := NewPool(testLogger(), nil, &OrchestratorMock{}, 1, WithMeterProvider(mp))
	require.NotNil(t, pool.claims, "WithMeterProvider must actually build the counter")
	for range 7 {
		pool.recordClaim(ctx, claimOutcomeEmpty)
	}
	for range 3 {
		pool.recordClaim(ctx, claimOutcomeClaimed)
	}
	pool.recordClaim(ctx, claimOutcomeContended)
	for range 2 {
		pool.recordClaim(ctx, claimOutcomeFailed)
	}
	assert.Equal(t, map[string]int64{
		claimOutcomeEmpty:     7,
		claimOutcomeClaimed:   3,
		claimOutcomeContended: 1,
		claimOutcomeFailed:    2,
	}, claimCounts(ctx, t, reader))
}

// TestRecordClaim_WithoutAMeterProviderIsANoOp pins the OPTION-LESS
// construction, which is not a hypothetical even though cmd/server now does
// pass a provider: every other NewPool call in this package and its tests
// omits the option, and any future caller may. A recordClaim that
// dereferenced a nil counter would then panic on the very first claim --
// inside work(), where it would be swallowed by recoverWorkerPanic and cost
// that worker permanently rather than crashing loudly.
func TestRecordClaim_WithoutAMeterProviderIsANoOp(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, &OrchestratorMock{}, 1)
	assert.Nil(t, pool.claims, "a Pool built without the option must hold no counter")
	assert.NotPanics(t, func() { pool.recordClaim(t.Context(), claimOutcomeEmpty) })
}

// TestWithMeterProvider_NilProviderIsTolerated covers the other way a caller
// can arrive with no meter: passing the option with a zero value, which a
// composition root reading an optional field can easily do.
func TestWithMeterProvider_NilProviderIsTolerated(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, &OrchestratorMock{}, 1, WithMeterProvider(nil))
	assert.Nil(t, pool.claims)
	assert.NotPanics(t, func() { pool.recordClaim(t.Context(), claimOutcomeClaimed) })
}

// TestWithMeterProvider_NoOpProviderStillBuildsACounter is the production
// telemetry-disabled path, and it is a different case from the two above:
// telemetry.Provider hands out upstream's no-op MeterProvider rather than
// nil, so the counter IS built and recorded into, and must not panic or
// allocate a data point anyone can read.
func TestWithMeterProvider_NoOpProviderStillBuildsACounter(t *testing.T) {
	t.Parallel()
	pool := NewPool(testLogger(), nil, &OrchestratorMock{}, 1, WithMeterProvider(metricnoop.NewMeterProvider()))
	require.NotNil(t, pool.claims)
	assert.NotPanics(t, func() { pool.recordClaim(t.Context(), claimOutcomeEmpty) })
}
