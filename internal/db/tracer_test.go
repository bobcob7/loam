package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/bobcob7/loam/internal/telemetry"
)

// TestQueryName_UsesSqlcHeaderNotStatementText is the property the span
// naming rests on: sqlc's `-- name:` header survives into the SQL constant
// pgx receives, so a bounded operation name is recoverable at a seam that
// otherwise sees only statement text.
//
// The inputs are copied verbatim from internal/db/gen -- header, blank
// spacing and all -- rather than hand-idealised, because the failure mode
// worth catching is sqlc changing its emitted preamble, and a
// hand-written "-- name: X :exec\nSELECT 1" fixture cannot see that.
//
// The cases deliberately differ from each other in name, in verb, and in
// sqlc kind (:exec, :one, :many, :copyfrom). A table where every row shared
// one query would pass just as happily against `return "DeleteChunksByFile"`,
// which is precisely the class of fixture blindness loam-p56y shipped.
func TestQueryName_UsesSqlcHeaderNotStatementText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "exec",
			sql: `-- name: DeleteChunksByFile :exec
DELETE FROM chunks
WHERE repo_id = $1 AND target_branch = $2 AND file = $3
`,
			want: "DeleteChunksByFile",
		},
		{
			name: "many",
			sql: `-- name: ListReposForBranch :many
SELECT id, forge_host FROM repos WHERE target_branch = $1
`,
			want: "ListReposForBranch",
		},
		{
			name: "one",
			sql: `-- name: GetCredentialForHost :one
SELECT token_ciphertext FROM credentials WHERE forge_host = $1
`,
			want: "GetCredentialForHost",
		},
		{
			name: "copyfrom kind still names the query",
			sql: `-- name: InsertGraphEdges :copyfrom
INSERT INTO graph_edges (repo_id, target_branch) VALUES ($1, $2)
`,
			want: "InsertGraphEdges",
		},
		{
			name: "leading whitespace before the header",
			sql:  "\n\t-- name: GetRepoByID :one\nSELECT id FROM repos WHERE id = $1\n",
			want: "GetRepoByID",
		},
		{
			name: "header with no sqlc kind",
			sql:  "-- name: BareName\nSELECT 1\n",
			want: "BareName",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, queryName(tt.sql))
		})
	}
}

// TestQueryName_UnheaderedSQLNeverReturnsTheStatement covers the fallback,
// and asserts the thing that actually matters about it rather than merely
// that it is non-empty: the returned name must not contain the statement
// text. A fallback of `return sql` would satisfy "returns something stable
// for a given input" while reintroducing both the cardinality explosion and
// the leak risk the whole file exists to prevent.
//
// pgxpool.Pool.Ping's bare ";" is in here because it is the one unheadered
// statement this repository is GUARANTEED to execute on every boot.
func TestQueryName_UnheaderedSQLNeverReturnsTheStatement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sql  string
	}{
		{name: "pgxpool ping", sql: ";"},
		{name: "empty", sql: ""},
		{name: "hand written select", sql: "SELECT token_ciphertext FROM credentials WHERE forge_host = 'git.example.com'"},
		{name: "comment that is not a sqlc header", sql: "-- just a comment\nSELECT 1"},
		{name: "header marker mid statement", sql: "SELECT 1 -- name: NotAHeader :one"},
		{name: "header keyword but wrong spacing", sql: "--name: NoSpace :one\nSELECT 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := queryName(tt.sql)
			assert.Equal(t, unnamedQuery, got)
			assert.NotContains(t, got, "SELECT", "the fallback must never be the statement text")
			assert.NotContains(t, got, "credentials", "the fallback must never be the statement text")
		})
	}
}

// TestTraceAcquire_SpanOnlyWhenSlowOrFailed pins all three branches of the
// conditional acquire span deterministically -- no database, and no sleep.
// The start time travels in the context, so backdating it is exact where a
// real slow acquire would be a timing race.
//
// The negative case is the one that matters most: pgxpool acquires a
// connection for EVERY query, so a regression that made this unconditional
// would silently double the process's span count.
func TestTraceAcquire_SpanOnlyWhenSlowOrFailed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		elapsed  time.Duration
		err      error
		wantSpan bool
	}{
		{name: "fast and successful emits nothing", elapsed: 0, err: nil, wantSpan: false},
		{name: "just under the threshold emits nothing", elapsed: defaultAcquireSpanThreshold - time.Millisecond, err: nil, wantSpan: false},
		{name: "past the threshold emits a span", elapsed: defaultAcquireSpanThreshold + time.Second, err: nil, wantSpan: true},
		{name: "a fast FAILURE still emits a span", elapsed: 0, err: context.DeadlineExceeded, wantSpan: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			recorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
			tracer := newQueryTracer(tp, 0)
			ctx := context.WithValue(t.Context(), acquireStartKey{}, time.Now().Add(-tt.elapsed))
			tracer.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{Err: tt.err})
			spans := recorder.Ended()
			if !tt.wantSpan {
				assert.Empty(t, spans, "no span should have been emitted")
				return
			}
			require.Len(t, spans, 1)
			assert.Equal(t, spanNamePrefix+"acquire", spans[0].Name())
			// The span must be BACKDATED to when the acquire began. Built
			// naively at TraceAcquireEnd it would have ~zero duration, and
			// the wait -- the entire point of the span -- would be invisible.
			assert.GreaterOrEqual(t, spans[0].EndTime().Sub(spans[0].StartTime()), tt.elapsed,
				"the span must cover the real wait, not the instant it was constructed")
			if tt.err != nil {
				assert.Equal(t, codes.Error, spans[0].Status().Code)
			}
		})
	}
}

// TestTraceAcquire_ThresholdIsConfigurable is what makes
// Config.AcquireSpanThreshold more than a field that is read and ignored.
// A deployment whose pool sits chronically just under the default sees
// nothing and, before this, could only recompile.
//
// The two rows are the same elapsed time on either side of a CUSTOM
// threshold, so a tracer that kept using defaultAcquireSpanThreshold would
// fail both: 10ms is under the 50ms default, so the first row would emit
// nothing where a 5ms threshold demands a span.
func TestTraceAcquire_ThresholdIsConfigurable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		threshold time.Duration
		elapsed   time.Duration
		wantSpan  bool
	}{
		{name: "a threshold below the default surfaces a wait the default would hide", threshold: 5 * time.Millisecond, elapsed: 10 * time.Millisecond, wantSpan: true},
		{name: "a threshold above the default hides a wait the default would surface", threshold: time.Second, elapsed: 100 * time.Millisecond, wantSpan: false},
		{name: "zero falls back to the default", threshold: 0, elapsed: defaultAcquireSpanThreshold + time.Millisecond, wantSpan: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			recorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
			tracer := newQueryTracer(tp, tt.threshold)
			ctx := context.WithValue(t.Context(), acquireStartKey{}, time.Now().Add(-tt.elapsed))
			tracer.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{})
			assert.Len(t, recorder.Ended(), map[bool]int{true: 1, false: 0}[tt.wantSpan])
		})
	}
}

// TestTraceAcquire_FailureIgnoresTheThreshold pins the one thing the
// threshold must NOT be able to silence. Pool exhaustion is the case these
// spans exist for -- no query ever runs, so nothing else records it -- and an
// operator who raises the threshold to quieten slow-acquire noise must not
// lose it.
func TestTraceAcquire_FailureIgnoresTheThreshold(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	tracer := newQueryTracer(tp, time.Hour)
	ctx := context.WithValue(t.Context(), acquireStartKey{}, time.Now())
	tracer.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{Err: context.DeadlineExceeded})
	require.Len(t, recorder.Ended(), 1, "a failed acquire must be recorded however high the threshold is set")
	assert.Equal(t, codes.Error, recorder.Ended()[0].Status().Code)
}

// TestTraceAcquireStart_CarriesTheStartTime pins the half of the pair that
// TestTraceAcquire_SpanOnlyWhenSlowOrFailed fakes, so the two together cover
// the real path rather than leaving a gap where the key is written with one
// type and read with another.
func TestTraceAcquireStart_CarriesTheStartTime(t *testing.T) {
	t.Parallel()
	tracer := newQueryTracer(tracenoop.NewTracerProvider(), 0)
	before := time.Now()
	ctx := tracer.TraceAcquireStart(t.Context(), nil, pgxpool.TraceAcquireStartData{})
	started, ok := ctx.Value(acquireStartKey{}).(time.Time)
	require.True(t, ok, "TraceAcquireEnd reads this key and silently does nothing if it is missing or the wrong type")
	assert.False(t, started.Before(before))
}

// TestSQLState_ReportsCodeNeverMessage is the unit-level half of the
// error-path leak guard (the integration half, against a real Postgres
// error that genuinely echoes its input, is
// TestQueryTracer_ErrorPathNeverLeaksArgument). Postgres puts offending
// VALUES in error messages; only the SQLSTATE is safe to record, so
// anything derived from err.Error() must never reach a span.
func TestSQLState_ReportsCodeNeverMessage(t *testing.T) {
	t.Parallel()
	t.Run("pg error yields its sqlstate", func(t *testing.T) {
		t.Parallel()
		err := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "s3cret-token-value"`}
		got := sqlState(err)
		assert.Equal(t, "22P02", got)
		assert.NotContains(t, got, "s3cret-token-value")
	})
	t.Run("wrapped pg error is still unwrapped", func(t *testing.T) {
		t.Parallel()
		wrapped := errors.Join(errors.New("acquiring connection"), &pgconn.PgError{Code: "23505"})
		assert.Equal(t, "23505", sqlState(wrapped))
	})
	t.Run("non-pg error yields a fixed placeholder", func(t *testing.T) {
		t.Parallel()
		got := sqlState(errors.New(`context deadline exceeded while binding "s3cret-token-value"`))
		assert.Equal(t, "unknown", got)
		assert.NotContains(t, got, "s3cret-token-value")
	})
}

// TestTraceQuery_ProbeIsSilentWhenHealthyAndSpeaksWhenItFails is the whole
// of loam-om77 in one table.
//
// The healthy row is the volume fix: a probe query that succeeds emits
// NOTHING, which is what removes the parentless postgres.unnamed roots from
// Tempo. The failing row is the constraint that made "just skip it" the
// wrong fix -- the case a health check exists to report must still produce a
// signal, and a change that silenced both would have made the system less
// observable, not more.
//
// Both rows run through the SAME pair of calls in the same order pgx makes
// them, so neither can pass by accident of how the test set the context up.
func TestTraceQuery_ProbeIsSilentWhenHealthyAndSpeaksWhenItFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantSpan bool
	}{
		{name: "a healthy probe emits nothing at all", err: nil, wantSpan: false},
		{name: "a FAILING probe still emits its span", err: &pgconn.PgError{Code: "08006"}, wantSpan: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			recorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
			tracer := newQueryTracer(tp, 0)
			// ";" is verbatim what pgxpool.Pool.Ping execs, and is the
			// statement that produced the production roots.
			ctx := tracer.TraceQueryStart(telemetry.WithProbe(t.Context()), nil, pgx.TraceQueryStartData{SQL: ";"})
			tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: tt.err})
			spans := recorder.Ended()
			if !tt.wantSpan {
				assert.Empty(t, spans, "a successful health probe must leave no span behind")
				return
			}
			require.Len(t, spans, 1)
			assert.Equal(t, spanNamePrefix+unnamedQuery, spans[0].Name())
			assert.Equal(t, codes.Error, spans[0].Status().Code)
			assert.Contains(t, attrMap(spans[0].Attributes()), probeAttribute,
				"a probe failure must be distinguishable from a real caller's unheadered query")
			assert.Equal(t, "08006", attrMap(spans[0].Attributes())["db.response.status_code"],
				"the SQLSTATE is the whole of what a failure records")
		})
	}
}

// TestTraceQuery_ProbeFailureSpanIsBackdated proves the deferred span still
// reports the real duration. Built naively at TraceQueryEnd it would have
// ~zero duration, and a probe that took 1900ms against a dying database --
// the most diagnostic number available about it -- would be indistinguishable
// from one that failed instantly.
func TestTraceQuery_ProbeFailureSpanIsBackdated(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	tracer := newQueryTracer(tp, 0)
	ctx := tracer.TraceQueryStart(telemetry.WithProbe(t.Context()), nil, pgx.TraceQueryStartData{SQL: ";"})
	const slept = 20 * time.Millisecond
	time.Sleep(slept)
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("connection refused")})
	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.GreaterOrEqual(t, spans[0].EndTime().Sub(spans[0].StartTime()), slept,
		"the deferred span must cover the real query, not the instant it was constructed")
}

// TestTraceQuery_ProbeNeverEndsTheCallersSpan is the mutation guard for the
// destructive half of this change, and it is the reason TraceQueryEnd keys
// off the value TraceQueryStart stored rather than off telemetry.IsProbe.
//
// TraceQueryStart returns a context with NO new span for a probe. If
// TraceQueryEnd then fell through to its normal path,
// trace.SpanFromContext would hand back the CALLER'S enclosing span and this
// tracer would stamp a row count on it and END it -- truncating a live trace
// that has nothing to do with the database. Deleting the probe branch from
// TraceQueryEnd is a mutation that still compiles and still passes every
// other test in this file, so it needs its own.
func TestTraceQuery_ProbeNeverEndsTheCallersSpan(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	tracer := newQueryTracer(tp, 0)
	// A caller span that is STILL RUNNING when the probe query happens
	// underneath it -- the shape a future /readyz-under-an-RPC would have.
	callerCtx, caller := tp.Tracer("test").Start(t.Context(), "caller")
	ctx := tracer.TraceQueryStart(telemetry.WithProbe(callerCtx), nil, pgx.TraceQueryStartData{SQL: ";"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	assert.Empty(t, recorder.Ended(), "the caller's span must not have been ended by the probe's TraceQueryEnd")
	assert.True(t, caller.IsRecording(), "the caller's span must still be live after a probe query completes under it")
	caller.End()
	ended := recorder.Ended()
	require.Len(t, ended, 1)
	assert.NotContains(t, attrMap(ended[0].Attributes()), "db.response.returned_rows",
		"the probe must not have written a database row count onto the caller's span")
}

// TestTraceQuery_UnmarkedWorkIsTracedEvenWithNoParent is the anti-overreach
// guard, and it pins the option loam-om77 REJECTED.
//
// The cheap fix considered was to skip any query with no parent span and no
// sqlc name. This is that exact shape -- a root, unheadered query -- arriving
// from work nobody marked, which is what the sync scheduler and ingest look
// like from inside this tracer. Their root traces are the only record those
// jobs leave, so this must still be a span. An implementation that inferred
// "probe" from the absence of a parent would fail here.
func TestTraceQuery_UnmarkedWorkIsTracedEvenWithNoParent(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	tracer := newQueryTracer(tp, 0)
	ctx := tracer.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: "SAVEPOINT file_0"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	spans := recorder.Ended()
	require.Len(t, spans, 1, "unmarked background work must still be traced on success, parent or no parent")
	assert.Equal(t, spanNamePrefix+unnamedQuery, spans[0].Name())
	assert.False(t, spans[0].Parent().IsValid(), "the fixture must genuinely be a root, or it proves nothing")
	assert.NotContains(t, attrMap(spans[0].Attributes()), probeAttribute,
		"work that never opted in must not be labelled a probe")
}

// attrMap indexes a span's attributes by key so an assertion can name the
// one it cares about instead of depending on their order.
func attrMap(attrs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}
