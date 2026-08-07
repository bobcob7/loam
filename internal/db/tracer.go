package db

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/bobcob7/loam/internal/telemetry"
)

// tracerName is the instrumentation scope every span from this package
// carries. It is the import path, per OpenTelemetry's convention, so a
// backend can attribute these spans to this package rather than to loam as
// a whole.
const tracerName = "github.com/bobcob7/loam/internal/db"

// spanNamePrefix namespaces every span this tracer produces, so a trace
// view can separate database work from internal/forge's and
// internal/ingest/embed/ollama's outbound HTTP at a glance.
const spanNamePrefix = "postgres."

// unnamedQuery is the operation name used when SQL carries no sqlc `-- name:`
// header. It is a CONSTANT rather than the SQL itself, and that is the whole
// point: falling back to the statement text would reintroduce both problems
// this file exists to avoid -- unbounded span-name cardinality, and a span
// name derived from something a future caller might interpolate a value into.
//
// Every hand-written statement in the tree lands here: internal/db/
// migrations' schema-version SELECT, internal/chunkstore's SAVEPOINT trio,
// internal/ingest's job-claim query, and pgvector-go's `to_regtype` probe on
// each new connection (internal/db.newPoolConfig installs it as AfterConnect).
//
// pgxpool.Pool.Ping DOES NOT, and an earlier version of this comment said it
// did. Ping bottoms out in pgconn.PgConn.Exec, below the layer that consults
// ConnConfig.Tracer, so it produces no span of any name. See probeQuery.
const unnamedQuery = "unnamed"

// queryTracer is the pgx tracing hook this package attaches at pool
// construction (newPoolConfig), implementing pgx.QueryTracer for
// Query/QueryRow/Exec and pgx.CopyFromTracer for CopyFrom. pgx discovers the
// second by type-asserting the value in ConnConfig.Tracer, so one value
// covers both.
//
// CopyFrom is included deliberately rather than for completeness: it is a
// SEPARATE dispatch path that pgx.QueryTracer never sees, so omitting it
// would leave the work as an unexplained gap under the ingest span rather
// than as a slow span. Its users are internal/codegraph's four `:copyfrom`
// queries -- InsertSymbols, InsertSymbolReferences, InsertGraphEdges and
// InsertSymbolHistory.
//
// # THE CHUNK PATH IS NOT A CopyFrom, AND IS THE SPAN-VOLUME PROBLEM
//
// internal/chunkstore does NOT bulk-write. ReplaceFileChunks issues one
// InsertChunk per chunk inside its transaction, so chunk writing produces
// ONE SPAN PER CHUNK -- a 10k-chunk batch is 10k spans, not one bulk span.
// That makes it comfortably the highest span-count path in the system, and
// it is a sampling question rather than a naming one: internal/telemetry's
// sampler is ParentBased, so once an ingest job's root span is sampled every
// one of those child spans is kept. Whoever instruments the ingest pipeline
// next should decide what to do about that volume ON PURPOSE -- the honest
// options are a lower root ratio for ingest, a span per file batch with the
// per-chunk inserts left untraced, or moving the write to CopyFrom -- rather
// than discovering it from a collector bill. Do not read the CopyFrom
// support above as evidence that the chunk path is already bulk; it is not.
//
// # THIS TRACER NEVER RECORDS A BOUND QUERY ARGUMENT. DO NOT ADD ONE.
//
// pgx hands the arguments over in TraceQueryStartData.Args and they are
// ignored on purpose. Everything sensitive this process holds crosses this
// exact seam as a bound parameter: chunk text and its embedding vectors,
// and -- through internal/credentialstore -- forge tokens. That package
// encrypts token_ciphertext under LOAM_ENCRYPTION_KEY precisely so a token
// is not readable from the database; putting the same value on a span, in
// plaintext, on its way to an OTLP collector, routes around that protection
// entirely. The same reasoning rules out db.query.parameter.<key>, which the
// semantic conventions define as opt-in for exactly this reason.
//
// It also never records the error MESSAGE. Postgres echoes offending values
// back in some of them ("invalid input syntax for type uuid: \"...\""), so
// span.RecordError(data.Err) is an indirect way to leak an argument that no
// review of the start path would catch. The SQLSTATE code carries the
// diagnostic value without the payload, and
// TestQueryTracer_ErrorPathNeverLeaksArgument holds that line.
type queryTracer struct {
	tracer           trace.Tracer
	acquireThreshold time.Duration
}

// newQueryTracer builds the pgx tracing hook from tp. tp must not be nil;
// newPoolConfig only calls this when Config.TracerProvider is set, and
// telemetry.Provider hands out upstream's no-op provider rather than nil
// when telemetry is disabled.
func newQueryTracer(tp trace.TracerProvider, acquireThreshold time.Duration) *queryTracer {
	if acquireThreshold <= 0 {
		acquireThreshold = defaultAcquireSpanThreshold
	}
	return &queryTracer{tracer: tp.Tracer(tracerName), acquireThreshold: acquireThreshold}
}

// TraceQueryStart opens the span for one Query/QueryRow/Exec, named for the
// sqlc query rather than the statement text (see queryName), and returns the
// context pgx threads through to TraceQueryEnd.
//
// UNLESS the caller marked ctx with telemetry.WithProbe, in which case it
// opens no span and instead stashes what TraceQueryEnd would need to build
// one retrospectively. See probeQuery.
//
// data.Args is deliberately not read here. See queryTracer's doc comment.
func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	name := queryName(data.SQL)
	if telemetry.IsProbe(ctx) {
		return context.WithValue(ctx, probeQueryKey{}, probeQuery{name: name, started: time.Now()})
	}
	ctx, _ = t.tracer.Start(ctx, spanNamePrefix+name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBOperationName(name),
		),
	)
	return ctx
}

// probeQueryKey is the private context key carrying a probe query's pending
// span material from TraceQueryStart to TraceQueryEnd.
type probeQueryKey struct{}

// probeQuery is what TraceQueryStart records instead of starting a span when
// the caller marked the context as a health probe (telemetry.WithProbe).
//
// # WHY THIS EXISTS, AND WHAT THE MEASUREMENT ACTUALLY SAID
//
// Production carries ~1.40 postgres.unnamed spans per second -- ~121,000 a
// day, 65% of every ROOT trace loam emits -- at the deployed sample ratio of
// 1.0. Those numbers are measured (Tempo search counts, corroborated by
// spanmetrics in VictoriaMetrics), not estimated.
//
// /readyz is one contributor to that wall, and this type removes its share.
// Be precise about the size of that share, because the bead that prompted
// this work over-attributed it and the corrected figure is the honest one:
// the readiness probe runs every 10s (helm's readinessProbe periodSeconds)
// and emits ONE unnamed span per poll, so ~0.10/s -- about 8,600 a day, or
// 7% of the wall. THE REMAINING ~85% IS internal/ingest's IDLE JOB-CLAIM
// LOOP, which is a different bug with the same symptom; see the note at the
// end of this comment.
//
// # /readyz EMITS ONE SPAN PER POLL, NOT TWO. pgxpool.Pool.Ping IS INVISIBLE
//
// This is worth stating because the obvious reading is wrong and it changes
// what the fix has to cover. /readyz does two pieces of database work --
// Pool.Ping and internal/db/migrations' hand-written `SELECT version, dirty
// FROM schema_migrations` -- and only the SECOND is ever traced.
//
// pgx never offers Ping to this tracer. pgxpool.Pool.Ping reaches
// pgconn.PgConn.Exec, which is a layer BELOW the one that consults
// ConnConfig.Tracer: only pgx.Conn's Query/QueryRow/Exec/CopyFrom do that.
// So the ping is untraceable here no matter what this file does.
// TestQueryTracer_PoolPingIsInvisibleToTheQueryTracer pins it, because it is
// the kind of fact that silently turns an assertion into a false pass --
// "Ping produced no span" is true of a working suppression AND of no
// suppression at all.
//
// Neither poll had a parent, because internal/server's
// RegisterUnauthenticated does not instrument the health endpoints, so what
// did arrive arrived as a ROOT with no way to tell it from a genuine
// unheadered query by a real caller.
//
// # THE BIGGER SHARE IS NOT THIS PACKAGE'S TO FIX
//
// The ~1.18/s balance is internal/ingest's claim loop: each worker tickers
// every 5s and, against an empty queue, issues begin + the unheadered
// `FOR UPDATE SKIP LOCKED` claim + rollback. Three unheadered statements per
// worker per tick, arriving as a sub-millisecond burst of exactly
// 3 x LOAM_INGEST_WORKERS root spans every 5 seconds. That is the dominant
// source and it is real work polling an empty queue, not a health check --
// telemetry.WithProbe is the right instrument for it too, applied at the
// claim loop, but that is a separate bead against a package this one does
// not own.
//
// # THE SPAN IS DEFERRED, NOT DELETED
//
// Suppressing the probe outright was the obvious fix and is the wrong one: a
// pool that has gone bad would then produce NOTHING, and "no span" is
// indistinguishable from "no traffic" and from "instrumentation removed".
// The failure case is the entire reason a health check is instrumented.
//
// So the span is built RETROSPECTIVELY in TraceQueryEnd, once the outcome is
// known, and only when the operation FAILED -- the same shape, and for the
// same reason, as TraceAcquireEnd's threshold span above it. The healthy
// probe costs one context value and one time.Now(); the sick probe still
// produces a postgres.unnamed root carrying its SQLSTATE and its true
// duration, backdated to when the query actually started.
//
// # AND THE HEALTHY CASE IS NOT SILENT EITHER
//
// internal/health records the probe's outcome and duration as a METRIC on
// every poll, ready or not. That is the instrument this question wanted all
// along: "can I reach the database" is a per-interval yes/no, which is a
// time series, not a trace. A trace answers "what happened in THIS request",
// and every one of these 34k requests had the same answer.
//
// # NAMING WOULD NOT HAVE FIXED IT
//
// Giving Ping a sqlc-style name would make the spans identifiable and would
// not remove a single one of them; the volume is the cost. queryName's
// constant fallback stays a constant (see unnamedQuery for why the statement
// text must never become a span name).
type probeQuery struct {
	name    string
	started time.Time
}

// endProbeQuery closes out a query that TraceQueryStart declined to trace:
// nothing at all when it succeeded, and a backdated span when it did not.
//
// The span is started from ctx, which for a health probe carries no parent,
// so a failure still surfaces as a root trace -- exactly as it did before
// loam-om77, and now as the ONLY postgres.unnamed root in the stream rather
// than as one of 34,000.
func (t *queryTracer) endProbeQuery(ctx context.Context, probe probeQuery, data pgx.TraceQueryEndData) {
	if data.Err == nil {
		return
	}
	// BOTH timestamps are explicit, which is what makes the deferred span's
	// duration the QUERY's duration rather than the query plus however long
	// this method took to build a span. pgx calls TraceQueryEnd when the
	// query is done, so that is the end; everything after this line --
	// tracer.Start, the attribute set, recordOutcome -- is bookkeeping that
	// happened afterwards and must not be charged to the database.
	//
	// The gap is normally sub-microsecond and no diagnosis turns on it. It
	// is written this way because the alternative is only correct by
	// accident: a GC pause or a descheduled goroutine between here and
	// span.End() would silently inflate the recorded duration of the one
	// span this path emits, which is a failure span someone is reading
	// precisely to find out how slow the database got. It also matches
	// TraceAcquireEnd exactly, which this design claims to mirror; that
	// method captures `ended` before building for the same reason.
	ended := time.Now()
	_, span := t.tracer.Start(ctx, spanNamePrefix+probe.name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(probe.started),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBOperationName(probe.name),
			// The one thing that distinguishes this root from a genuine
			// unheadered query by a real caller, which was the OTHER half of
			// the reported problem: a reader had no way to tell them apart.
			attribute.Bool(probeAttribute, true),
		),
	)
	recordOutcome(span, data.CommandTag, data.Err)
	span.End(trace.WithTimestamp(ended))
}

// probeAttribute flags a span as having come from a health probe rather than
// from work someone asked for. Only failures carry it, since a healthy probe
// emits no span at all.
const probeAttribute = "loam.probe"

// TraceQueryEnd closes the span TraceQueryStart opened, recording the row
// count on success and the SQLSTATE on failure -- never the error message.
//
// For Query (as opposed to Exec/QueryRow) pgx defers this call until the
// resulting pgx.Rows is CLOSED, so the span's duration covers row iteration
// as well as the round trip -- which is what you want, and is also why a
// caller that leaks a pgx.Rows leaks a never-ended span with it. Every
// caller in this tree goes through sqlc-generated code, which always
// `defer rows.Close()`s, so that path does not exist today; a hand-written
// pool.Query would be the way to introduce it.
//
// THE PROBE BRANCH IS LOAD-BEARING, NOT AN OPTIMISATION. It keys off the
// value TraceQueryStart stored, not off telemetry.IsProbe, so the two halves
// are exactly paired: whatever Start did, End undoes. Take the branch away
// and the fallthrough is silently destructive rather than merely wrong --
// trace.SpanFromContext would return the caller's OWN enclosing span (or the
// no-op span when there is none), and this method would stamp a database
// row count on it and END IT, truncating a live parent trace. Any future
// early-return added to TraceQueryStart owes End the same treatment.
func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if probe, ok := ctx.Value(probeQueryKey{}).(probeQuery); ok {
		t.endProbeQuery(ctx, probe, data)
		return
	}
	span := trace.SpanFromContext(ctx)
	recordOutcome(span, data.CommandTag, data.Err)
	span.End()
}

// TraceCopyFromStart opens the span for one CopyFrom. The table and column
// names are schema, fixed at compile time by sqlc's generated copyfrom.go --
// they are not user input and cannot carry a row value, which is why they
// are safe to record when an argument is not.
func (t *queryTracer) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	table := data.TableName.Sanitize()
	ctx, _ = t.tracer.Start(ctx, spanNamePrefix+"copyfrom."+table,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBOperationName("copyfrom"),
			semconv.DBCollectionName(table),
			attribute.StringSlice("db.copyfrom.columns", data.ColumnNames),
		),
	)
	return ctx
}

// TraceCopyFromEnd closes the span TraceCopyFromStart opened, on the same
// terms as TraceQueryEnd.
func (t *queryTracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	span := trace.SpanFromContext(ctx)
	recordOutcome(span, data.CommandTag, data.Err)
	span.End()
}

// defaultAcquireSpanThreshold is how long a pool acquire has to take before
// it is worth a span of its own, when Config.AcquireSpanThreshold is unset.
// A pool with a free connection hands one over in microseconds; anything
// past this bound means the caller QUEUED, which is a different performance
// story from a slow query and is invisible in the query span, since pgxpool
// finishes acquiring before pgx starts tracing the query.
//
// It is a DEFAULT rather than a fixed constant because the bound that makes
// a saturated pool visible depends on the deployment: one sitting at a 30ms
// p50 is in trouble and would show nothing here. See
// Config.AcquireSpanThreshold and LOAM_OTEL_DB_ACQUIRE_THRESHOLD.
const defaultAcquireSpanThreshold = 50 * time.Millisecond

// acquireStartKey is the private context key carrying an acquire's start
// time from TraceAcquireStart to TraceAcquireEnd.
type acquireStartKey struct{}

// TraceAcquireStart records when a pgxpool.Acquire began. It starts no span
// -- see TraceAcquireEnd for why.
func (t *queryTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, acquireStartKey{}, time.Now())
}

// TraceAcquireEnd emits a span for a pool acquire ONLY when the acquire
// failed or took longer than the configured threshold. pgxpool discovers this
// method by type-asserting the same value in ConnConfig.Tracer that carries
// the query and CopyFrom hooks.
//
// # WHY CONDITIONAL, WHEN EVERYTHING ELSE HERE IS UNCONDITIONAL
//
// Acquire is what closes a real observability hole: a pool exhaustion
// timeout means NO QUERY EVER RUNS, so today the worst case this pool has
// produces no span at all and is invisible in a trace. That case must be
// visible.
//
// But pgxpool acquires a connection for EVERY Query/Exec, so tracing all of
// them unconditionally would emit a second span per query and double the
// span count of the whole process -- against the path this file already
// documents as the span-volume problem, where one file batch is one span per
// chunk. Paying double on every successful sub-millisecond acquire to make
// the rare slow one visible is the wrong trade, so the span is built
// RETROSPECTIVELY with explicit start and end timestamps once the duration
// is known. The timings are exact, not approximated: trace.WithTimestamp
// backdates the start to when the acquire actually began.
//
// The cost of this design is that a fast acquire leaves NO evidence it was
// traced, so "no acquire span" means "fast or not instrumented" rather than
// "fast". TestQueryTracer_AcquireSpanOnlyWhenSlowOrFailed pins both halves
// so the distinction is at least asserted somewhere.
//
// # THIS METHOD DELIBERATELY IGNORES telemetry.IsProbe
//
// loam-om77 taught the query path to stay quiet for health probes, and the
// symmetric-looking change here would be wrong. Acquire is ALREADY silent in
// the healthy case -- a probe against a working pool takes a free connection
// in microseconds and emits nothing -- so it contributes none of the volume
// that bead was about. What it does emit is a saturated or dead pool, which
// on the probe path is the single most valuable span this tracer produces:
// it is the first thing to break when the pool goes bad, and it breaks
// BEFORE any query runs, so nothing downstream would report it. Suppressing
// it to match the query path would trade the one signal for consistency.
func (t *queryTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	started, ok := ctx.Value(acquireStartKey{}).(time.Time)
	if !ok {
		return
	}
	ended := time.Now()
	if data.Err == nil && ended.Sub(started) < t.acquireThreshold {
		return
	}
	_, span := t.tracer.Start(ctx, spanNamePrefix+"acquire",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(started),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBOperationName("acquire"),
		),
	)
	if data.Err != nil {
		span.SetStatus(codes.Error, "")
		span.SetAttributes(semconv.DBResponseStatusCode(sqlState(data.Err)))
	}
	span.End(trace.WithTimestamp(ended))
}

// recordOutcome is the shared end-of-operation bookkeeping for both tracer
// pairs: rows affected on success, SQLSTATE on failure.
//
// The failure branch sets an EMPTY status description and does not call
// span.RecordError. That is not an oversight -- see queryTracer's doc
// comment for the argument-echo problem it closes.
func recordOutcome(span trace.Span, tag pgconn.CommandTag, err error) {
	if err != nil {
		span.SetStatus(codes.Error, "")
		span.SetAttributes(semconv.DBResponseStatusCode(sqlState(err)))
		return
	}
	// CommandTag.RowsAffected covers both senses: rows returned for a
	// SELECT, rows written for an INSERT/UPDATE/DELETE. It is a COUNT, so
	// unlike everything else pgx offers at this point it carries no row
	// content.
	span.SetAttributes(semconv.DBResponseReturnedRows(int(tag.RowsAffected())))
}

// sqlState extracts Postgres's five-character SQLSTATE from err, which is
// the whole of what this package records about a failure. Anything that is
// not a *pgconn.PgError -- a cancelled context, a dead connection -- has no
// SQLSTATE and is reported under a fixed placeholder rather than by message.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return "unknown"
}

// queryName extracts the sqlc query name from sql, which is what this
// package uses as a span name.
//
// sqlc emits every generated statement with its name as the first line --
// `-- name: DeleteChunksByFile :exec` -- and that comment travels with the
// SQL constant all the way into pgx, which is what makes a stable,
// bounded-cardinality operation name available at a seam that otherwise
// only sees statement text. SQL that does not carry the header (a
// hand-written statement, or pgxpool's own Ping) returns unnamedQuery; the
// statement itself is NEVER returned.
func queryName(sql string) string {
	rest, ok := strings.CutPrefix(strings.TrimLeft(sql, " \t\r\n"), "-- name: ")
	if !ok {
		return unnamedQuery
	}
	line, _, _ := strings.Cut(rest, "\n")
	name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	if name == "" {
		return unnamedQuery
	}
	return name
}
