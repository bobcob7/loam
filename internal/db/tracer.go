package db

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
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
// pgxpool.Pool.Ping (which execs a bare ";") and any hand-written statement
// land here.
const unnamedQuery = "unnamed"

// queryTracer is the pgx tracing hook this package attaches at pool
// construction (newPoolConfig), implementing pgx.QueryTracer for
// Query/QueryRow/Exec and pgx.CopyFromTracer for CopyFrom. pgx discovers the
// second by type-asserting the value in ConnConfig.Tracer, so one value
// covers both; CopyFrom is included deliberately rather than for
// completeness, because it is how internal/chunkstore writes chunks and
// internal/codegraph writes graph edges -- the two bulk paths of an ingest
// run, and exactly the wall-clock a QueryTracer-only implementation would
// leave as an unexplained gap under the ingest span.
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
	tracer trace.Tracer
}

// newQueryTracer builds the pgx tracing hook from tp. tp must not be nil;
// newPoolConfig only calls this when Config.TracerProvider is set, and
// telemetry.Provider hands out upstream's no-op provider rather than nil
// when telemetry is disabled.
func newQueryTracer(tp trace.TracerProvider) *queryTracer {
	return &queryTracer{tracer: tp.Tracer(tracerName)}
}

// TraceQueryStart opens the span for one Query/QueryRow/Exec, named for the
// sqlc query rather than the statement text (see queryName), and returns the
// context pgx threads through to TraceQueryEnd.
//
// data.Args is deliberately not read here. See queryTracer's doc comment.
func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	name := queryName(data.SQL)
	ctx, _ = t.tracer.Start(ctx, spanNamePrefix+name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBOperationName(name),
		),
	)
	return ctx
}

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
func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	recordOutcome(span, data.CommandTag, data.Err)
	span.End()
}

// TraceCopyFromStart opens the span for one CopyFrom. The table and column
// names are schema, fixed at compile time by sqlc's generated copyfrom.go
// and by internal/chunkstore -- they are not user input and cannot carry a
// row value, which is why they are safe to record when an argument is not.
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
