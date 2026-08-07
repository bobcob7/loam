//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon. On podman also
// set TESTCONTAINERS_RYUK_DISABLED=true:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/health/... -v
//
// # WHY loam-om77 NEEDS AN INTEGRATION TEST AND NOT ONLY THE UNIT ONES
//
// The unit tests in health_test.go drive /readyz through MOCK collaborators.
// A mock Pinger executes no SQL, so it cannot emit the spans this bead is
// about, and the assertion that the marker is applied
// (TestReadiness_MarksBothChecksAsProbes) proves only that a context value
// was set -- not that anything downstream acts on it.
//
// The claim being made is a joint property of FOUR packages that no one of
// them can check alone: internal/health marks the context, internal/db's
// queryTracer reads the marker, internal/db/migrations issues the second of
// the two statements, and pgx has to carry the value through pgxpool's
// acquisition and its own derived contexts to get from the first to the
// second. Any of those seams could break the fix while every unit test in
// the tree stayed green.
//
// So this drives the REAL handler, over a REAL pool, against a REAL
// Postgres, and counts what a collector would have received.
package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
)

// realReadiness builds the production wiring of /readyz -- the real pool
// with the real query tracer attached the way cmd/server attaches it, and
// the real schema check bound to that same pool -- plus the recorders to
// read spans and metrics back from.
func realReadiness(ctx context.Context, t *testing.T) (*Readiness, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, container.Terminate(context.WithoutCancel(t.Context()))) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		// AlwaysSample, because the production symptom was measured at a
		// deployed sample ratio of 1.0. Anything less here and "no spans"
		// could just be the sampler.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.WithoutCancel(t.Context()))) })
	pool, err := db.NewPool(ctx, db.Config{DatabaseURL: dsn, EncryptionKey: "key", TracerProvider: tp}, logger)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { assert.NoError(t, mp.Shutdown(context.WithoutCancel(t.Context()))) })
	// Pool construction leaves one span behind -- pgvector-go's `to_regtype`
	// registration query, which internal/db installs as AfterConnect and
	// which runs on every new connection. NOT NewPool's Ping: pgxpool's Ping
	// never reaches the query tracer (internal/db's
	// TestQueryTracer_PoolPingIsInvisibleToTheQueryTracer). It is unmarked
	// and SHOULD be traced, so it is cleared rather than prevented, keeping
	// the counts below about the probes this test actually drives.
	require.NotEmpty(t, recorder.Ended(),
		"pool construction must have produced a span; if not, the tracer is not attached and this test proves nothing")
	recorder.Reset()
	return NewReadiness(pool, migrations.NewSchemaCheck(pool), mp, logger), recorder, reader
}

// TestReadiness_HealthyProbesProduceNoSpansAtAll is the bead's headline
// claim, measured the way production measured the problem: count the spans a
// run of probes emits.
//
// TWENTY probes, not one. The production symptom was a RATE -- a wall of
// parentless postgres.unnamed roots arriving in bursts -- and a single-probe
// fixture could pass against an implementation that suppressed only the
// first. Twenty polls is over three minutes of a 10s readinessProbe, and the
// required answer is zero spans, not "fewer".
func TestReadiness_HealthyProbesProduceNoSpansAtAll(t *testing.T) {
	t.Parallel()
	ctx := context.WithoutCancel(t.Context())
	readiness, recorder, reader := realReadiness(ctx, t)
	const probes = 20
	for range probes {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/readyz", nil)
		rec := httptest.NewRecorder()
		readiness.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, readyBody, rec.Body.String())
	}
	var names []string
	for _, s := range recorder.Ended() {
		names = append(names, s.Name())
	}
	assert.Empty(t, names,
		"%d healthy probes must emit no span whatsoever; got %v", probes, names)

	// And the signal that replaces them is present and counts every one.
	assert.Equal(t, map[string]uint64{readyOutcome: probes}, readinessOutcomes(t, reader),
		"the metric must account for every probe the traces no longer do")
}

// TestReadiness_ABrokenDatabaseStillSignalsThroughBothInstruments is the
// requirement loam-om77 was not allowed to break, driven end to end: a fix
// that silenced the healthy case AND the sick one would have made the system
// less observable than before it.
//
// The database is stopped underneath a pool that connected cleanly, which is
// the real production shape -- a Postgres that goes away while loam keeps
// running -- rather than a pool that never worked.
//
// BOTH instruments are asserted, because they answer different questions and
// the trace alone is not sufficient. When the database is unreachable pgx
// fails at ACQUIRE, before it traces any query, so what survives is the
// acquire span (which internal/db deliberately never suppresses) rather than
// a deferred query span. The metric is what makes the outcome countable
// regardless of which of those two paths the failure took.
func TestReadiness_ABrokenDatabaseStillSignalsThroughBothInstruments(t *testing.T) {
	t.Parallel()
	ctx := context.WithoutCancel(t.Context())
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	terminated := false
	t.Cleanup(func() {
		if !terminated {
			assert.NoError(t, container.Terminate(context.WithoutCancel(t.Context())))
		}
	})
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.WithoutCancel(t.Context()))) })
	pool, err := db.NewPool(ctx, db.Config{DatabaseURL: dsn, EncryptionKey: "key", TracerProvider: tp}, logger)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { assert.NoError(t, mp.Shutdown(context.WithoutCancel(t.Context()))) })
	readiness := NewReadiness(pool, migrations.NewSchemaCheck(pool), mp, logger)
	// Building the pool already emitted one span -- pgvector-go's
	// `to_regtype` registration query, which internal/db installs as
	// AfterConnect and which runs on every new connection. It is unmarked
	// and SHOULD be traced. Clearing here is not cosmetic: without it the
	// healthy-probe assertion below fails on a span that predates the probe,
	// which is exactly how this test first failed.
	require.NotEmpty(t, recorder.Ended(), "pool construction must have produced a span, or the tracer is not attached")
	recorder.Reset()

	// Healthy first, so the "sick" assertions below cannot pass merely
	// because the wiring never worked.
	rec := httptest.NewRecorder()
	readiness.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, recorder.Ended(), "the healthy probe must still be silent")
	recorder.Reset()

	// Now take Postgres away.
	require.NoError(t, container.Terminate(context.WithoutCancel(t.Context())))
	terminated = true

	const sickProbes = 3
	for range sickProbes {
		rec := httptest.NewRecorder()
		readiness.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/readyz", nil))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code,
			"a stopped database must take this instance out of rotation")
		require.Equal(t, "not ready: "+databaseReason, rec.Body.String())
	}

	// THE METRIC: countable, attributed, and rising for as long as it is
	// broken. The healthy probe from before the database was stopped is
	// still in the stream, and that is the assertion worth making -- what an
	// operator needs is not "a failure happened" but a series that was
	// reporting ready and is now reporting a named failure, from the same
	// instrument, without a gap where the evidence used to be.
	assert.Equal(t, map[string]uint64{readyOutcome: 1, databaseReason: sickProbes}, readinessOutcomes(t, reader),
		"the broken database must produce a rising, alertable count alongside the healthy probe that preceded it -- absence is not an alarm")

	// THE TRACE: a broken pool is not silent either.
	var names []string
	for _, s := range recorder.Ended() {
		names = append(names, s.Name())
	}
	require.NotEmpty(t, names,
		"a probe against a dead database must leave SOME span behind; silencing the healthy case and the sick one alike would be worse than not fixing anything")
	assert.Contains(t, names, "postgres.acquire",
		"the surviving signal is specifically the ACQUIRE failure -- pgx never reaches a query against a dead database, which is why internal/db's TraceAcquireEnd deliberately ignores the probe marker")
}
