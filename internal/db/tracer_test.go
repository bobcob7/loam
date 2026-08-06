package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
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
