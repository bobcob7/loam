//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag. Run explicitly with:
//
//	go test -tags=integration ./internal/db/... -run TestQueryTracer -v
//
// On podman, also set TESTCONTAINERS_RYUK_DISABLED=true -- see
// pool_integration_test.go's header for why.
//
// # WHY THIS IS AN INTEGRATION TEST AND NOT A UNIT TEST
//
// A mocked pool cannot show that a pgx.QueryTracer FIRES. The wiring under
// test is a single assignment to pgxpool.Config.ConnConfig.Tracer, and
// everything interesting about it -- that pgx consults it on Query, on Exec
// and on CopyFrom; that the SQL string it hands over still carries sqlc's
// `-- name:` header by the time it arrives; that the context returned from
// TraceQueryStart is the one TraceQueryEnd is later given; that a real
// Postgres error reaches data.Err with the offending value inside it -- is a
// property of pgx and the server, not of this package's types. A unit test
// with a fake tracer would assert that the code calls the code.
package db

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/telemetry"
	"github.com/bobcob7/loam/internal/testdb"
)

// tracedPool starts a migrated Postgres and returns a pool built through the
// REAL NewPool with a recording tracer provider attached the way cmd/server
// attaches one, plus the recorder to read spans back from.
func tracedPool(ctx context.Context, t *testing.T, dsnParams ...string) (*pgxpool.Pool, trace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, container.Terminate(context.Background()))
	})
	// MIGRATE ON THE PLAIN DSN, POOL ON THE PARAMETERISED ONE. dsnParams
	// carries pgxpool-only settings such as pool_max_conns, which
	// pgxpool.ParseConfig consumes and strips. migrations.Migrate goes
	// through database/sql instead, which does NOT recognise them and
	// forwards them to the server as startup options -- where Postgres
	// rejects the connection outright with FATAL: unrecognized
	// configuration parameter "pool_max_conns" (SQLSTATE 42704). Handing
	// one DSN to both is the obvious shortcut and it does not work.
	baseDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, baseDSN, logger))
	dsn, err := container.ConnectionString(ctx, append([]string{"sslmode=disable"}, dsnParams...)...)
	require.NoError(t, err)
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	pool, err := NewPool(ctx, Config{DatabaseURL: dsn, EncryptionKey: "key", TracerProvider: tp}, logger)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// The pool arrives with one postgres.unnamed span already recorded, and
	// it is worth naming WHICH, because this comment used to get it wrong:
	// it is pgvector-go's `to_regtype(...)` query, run by the RegisterTypes
	// hook newPoolConfig installs as AfterConnect on every new connection.
	// It is NOT NewPool's Ping -- pgxpool.Pool.Ping never reaches this
	// tracer at all (see TestQueryTracer_PoolPingIsInvisibleToTheQueryTracer
	// and tracer.go's unnamedQuery).
	//
	// That distinction matters here rather than being trivia: a helper that
	// believes Ping is traced will happily "prove" a suppression that does
	// not exist, because Ping emits nothing either way. The span is still
	// the first proof the tracer is attached, so the check stays -- it just
	// now says what it is checking.
	require.NotEmpty(t, recorder.Ended(),
		"AfterConnect's pgvector registration query must already have produced a span; if not, the tracer is not attached")
	recorder.Reset()
	return pool, tp, recorder
}

// spanNames returns the names of every ended span, for set comparisons.
func spanNames(recorder *tracetest.SpanRecorder) []string {
	var names []string
	for _, s := range recorder.Ended() {
		names = append(names, s.Name())
	}
	return names
}

// findSpan returns the single ended span with the given name.
func findSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.Name() == name {
			require.Nil(t, found, "more than one span named %q", name)
			found = s
		}
	}
	require.NotNil(t, found, "no span named %q; got %v", name, spanNames(recorder))
	return found
}

// allSpanText returns every string a span carries -- its name, every
// attribute value, every event name and event attribute value -- so a leak
// scan covers the whole surface rather than the part the author remembered.
func allSpanText(span sdktrace.ReadOnlySpan) []string {
	out := []string{span.Name(), span.Status().Description}
	for _, kv := range span.Attributes() {
		out = append(out, string(kv.Key), kv.Value.Emit())
	}
	for _, event := range span.Events() {
		out = append(out, event.Name)
		for _, kv := range event.Attributes {
			out = append(out, string(kv.Key), kv.Value.Emit())
		}
	}
	return out
}

// TestQueryTracer_NamesSpansAfterTheSqlcQueryNotTheSQL is the central claim:
// pgx's QueryTracer sees only statement text, and sqlc's `-- name:` header
// is what makes a bounded operation name recoverable from it -- through the
// real generated code, the real pool wiring and a real server.
//
// THE FIXTURE IS DELIBERATELY NOT ONE QUERY. Three different sqlc queries
// run here, differing in name, in table and in sqlc kind, plus one
// unheadered statement. A single-query fixture would pass identically
// against `return "postgres.query"` -- the exact fixture blindness loam-p56y
// shipped, where every sampler test used ratio 1 and AlwaysSample passed the
// suite. The assertion that the three names are DISTINCT, and that none of
// them contains SQL, is what makes this test able to fail.
func TestQueryTracer_NamesSpansAfterTheSqlcQueryNotTheSQL(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _, recorder := tracedPool(ctx, t)
	queries := gen.New(pool)
	_, err := queries.ListRoles(ctx)
	require.NoError(t, err)
	_, err = queries.ListCredentialStatuses(ctx)
	require.NoError(t, err)
	_, err = queries.GetRoleByName(ctx, "author")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "SELECT 1")
	require.NoError(t, err)
	names := spanNames(recorder)
	assert.Contains(t, names, "postgres.ListRoles")
	assert.Contains(t, names, "postgres.ListCredentialStatuses")
	assert.Contains(t, names, "postgres.GetRoleByName")
	assert.Contains(t, names, "postgres."+unnamedQuery, "a statement with no sqlc header must fall back to the constant, never to the SQL")
	distinct := map[string]bool{}
	for _, n := range names {
		distinct[n] = true
	}
	assert.GreaterOrEqual(t, len(distinct), 4, "distinct queries must get distinct span names; a constant name would collapse these to 1")
	for _, span := range recorder.Ended() {
		assert.NotContains(t, span.Name(), "SELECT", "no span name may contain statement text")
		assert.NotContains(t, span.Name(), "FROM", "no span name may contain statement text")
		assert.NotContains(t, span.Name(), "$1", "no span name may contain statement text")
		assert.Less(t, len(span.Name()), 64, "a span name this long is statement text, not an operation name")
	}
}

// TestQueryTracer_NestsUnderTheCallerSpan proves a query span joins the
// caller's trace rather than starting an orphan root -- the property that
// makes "which queries did this RPC run, and for how long" answerable at
// all. Without it these spans are unattributable and the whole bead is
// decorative.
func TestQueryTracer_NestsUnderTheCallerSpan(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, tp, recorder := tracedPool(ctx, t)
	rpcCtx, rpc := tp.Tracer("test").Start(ctx, "rpc ListRoles")
	_, err := gen.New(pool).ListRoles(rpcCtx)
	require.NoError(t, err)
	rpc.End()
	span := findSpan(t, recorder, "postgres.ListRoles")
	assert.Equal(t, rpc.SpanContext().SpanID(), span.Parent().SpanID(), "the query span must be a child of the caller's span")
	assert.Equal(t, rpc.SpanContext().TraceID(), span.SpanContext().TraceID())
	assert.Equal(t, trace.SpanKindClient, span.SpanKind())
	assert.False(t, span.EndTime().IsZero(), "TraceQueryEnd must have ended the span TraceQueryStart opened")
}

// TestQueryTracer_NeverRecordsABoundArgument IS THE ASSERTION WITH THE
// LONGEST USEFUL LIFE IN THIS BEAD. It is written to fail the moment anyone
// adds TraceQueryStartData.Args to the span, in any form -- individually as
// db.query.parameter.<n>, joined into one string, or as a slice attribute --
// because it scans the ENTIRE span surface for the value rather than
// checking that a particular attribute key is absent.
//
// The sentinels are the three things that actually cross this seam and must
// never leave the process: a forge token (internal/credentialstore encrypts
// token_ciphertext under LOAM_ENCRYPTION_KEY precisely so it is not readable
// -- a span attribute would route around that), chunk text (repository
// source), and an embedding vector.
func TestQueryTracer_NeverRecordsABoundArgument(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _, recorder := tracedPool(ctx, t)
	const (
		tokenSentinel = "gto-l0am-s3cret-forge-token-DO-NOT-RECORD"
		hostSentinel  = "git.l0am-host-sentinel.invalid"
		chunkSentinel = "func withdrawEverything(acct Account) // l0am-chunk-sentinel"
	)
	queries := gen.New(pool)
	// A bound argument on a read: the host is the lookup key.
	_, err := queries.GetCredentialByHost(ctx, hostSentinel)
	require.Error(t, err, "no such credential exists; the point is that the argument was BOUND and reached the server")
	// A bound argument on a write, through the REAL sqlc query
	// internal/credentialstore uses -- so the ciphertext argument travels
	// exactly as it does in production.
	_, err = queries.UpsertCredentialToken(ctx, gen.UpsertCredentialTokenParams{
		ID:              pgUUID(uuid.New()),
		Host:            hostSentinel,
		TokenCiphertext: []byte(tokenSentinel),
	})
	require.NoError(t, err)
	// Chunk text and an embedding, through the extended query protocol.
	repoID := seedChunkRepo(ctx, t, pool, "group/tracer-arg-repo")
	_, err = pool.Exec(ctx,
		`INSERT INTO chunks (id, repo_id, target_branch, file, start_line, end_line, content, embedding)
		 VALUES ($1, $2, 'main', 'x.go', 1, 2, $3, $4)`,
		pgUUID(uuid.New()), repoID, chunkSentinel, pgvector.NewVector(unitEmbedding(0)),
	)
	require.NoError(t, err)
	spans := recorder.Ended()
	require.NotEmpty(t, spans)
	sawAttributes := false
	for _, span := range spans {
		if len(span.Attributes()) > 0 {
			sawAttributes = true
		}
		for _, text := range allSpanText(span) {
			for _, secret := range []string{tokenSentinel, hostSentinel, chunkSentinel} {
				assert.NotContains(t, text, secret, "span %q leaked a bound query argument", span.Name())
			}
		}
	}
	require.True(t, sawAttributes, "spans carrying no attributes at all would make this test vacuous")
}

// TestQueryTracer_ErrorPathNeverLeaksArgument closes the indirect route into
// the same leak, and it is the reason recordOutcome records a SQLSTATE
// instead of calling span.RecordError.
//
// Postgres echoes the offending VALUE back inside some of its error
// messages. `span.RecordError(data.Err)` is therefore an argument leak that
// no amount of reviewing TraceQueryStart would catch, because the value
// arrives from the SERVER on the way out rather than from the caller on the
// way in. The test asserts positively that the error genuinely does contain
// the sentinel -- otherwise it would be proving nothing -- and then that the
// span does not.
func TestQueryTracer_ErrorPathNeverLeaksArgument(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _, recorder := tracedPool(ctx, t)
	const sentinel = "l0am-argument-echo-sentinel"
	// $1 is bound as text and cast to uuid server-side, so the failure is a
	// real Postgres 22P02 whose message quotes the bound value back.
	_, err := pool.Exec(ctx, "SELECT $1::text::uuid", sentinel)
	require.Error(t, err)
	require.Contains(t, err.Error(), sentinel,
		"this test is only meaningful if Postgres really does echo the bound value into the error; if this stops holding, find another statement that does")
	span := findSpan(t, recorder, "postgres."+unnamedQuery)
	for _, text := range allSpanText(span) {
		assert.NotContains(t, text, sentinel, "the error path must not put a bound argument on the span")
	}
	assert.Equal(t, "22P02", statusCodeOf(t, span), "the SQLSTATE is what carries the diagnostic value without the payload")
	assert.Empty(t, span.Status().Description, "the status description must stay empty; err.Error() is the leak vector")
	assert.Empty(t, span.Events(), "RecordError would add an exception event carrying err.Error()")
}

// statusCodeOf returns the span's db.response.status_code attribute.
func statusCodeOf(t *testing.T, span sdktrace.ReadOnlySpan) string {
	t.Helper()
	for _, kv := range span.Attributes() {
		if kv.Key == "db.response.status_code" {
			return kv.Value.AsString()
		}
	}
	t.Fatalf("span %q carries no db.response.status_code attribute", span.Name())
	return ""
}

// TestQueryTracer_TracesCopyFromToo covers the bulk path. CopyFrom does NOT
// go through QueryTracer -- pgx dispatches it to CopyFromTracer, discovered
// by type assertion on the same value -- so a QueryTracer-only
// implementation would leave the two heaviest writes of an ingest run
// (chunks here, graph edges via internal/codegraph) as an unexplained gap
// under the ingest span, which is the opposite of what this bead is for.
//
// The chunk content is a sentinel, so this doubles as the leak assertion for
// the bulk path: CopyFrom's rows are the single largest concentration of
// repository source text this process ever hands to the database.
func TestQueryTracer_TracesCopyFromToo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, tp, recorder := tracedPool(ctx, t)
	const chunkSentinel = "func withdrawEverything(acct Account) // l0am-copyfrom-sentinel"
	repoID := seedChunkRepo(ctx, t, pool, "group/tracer-copyfrom-repo")
	rpcCtx, rpc := tp.Tracer("test").Start(ctx, "ingest job")
	rows := pgx.CopyFromRows([][]any{
		{pgUUID(uuid.New()), repoID, "main", "a.go", int32(1), int32(2), chunkSentinel, pgvector.NewVector(unitEmbedding(0))},
	})
	count, err := pool.CopyFrom(rpcCtx, pgx.Identifier{"chunks"}, chunkColumns, rows)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	rpc.End()
	span := findSpan(t, recorder, `postgres.copyfrom."chunks"`)
	assert.Equal(t, rpc.SpanContext().SpanID(), span.Parent().SpanID())
	assert.Equal(t, trace.SpanKindClient, span.SpanKind())
	for _, text := range allSpanText(span) {
		assert.NotContains(t, text, "l0am-copyfrom-sentinel", "CopyFrom rows must never reach a span attribute")
	}
	// The COLUMN names are schema, not row data, and are safe -- recording
	// them is what makes a copyfrom span legible. Asserting it here keeps
	// the leak scan above honest about what it is and is not excluding.
	joined := strings.Join(allSpanText(span), " ")
	assert.Contains(t, joined, "content", "column names are schema and are deliberately recorded")
}

// TestQueryTracer_AbsentWhenNoProviderConfigured is the disabled-path proof.
// Every integration suite in this tree builds its pool from a bare
// db.Config{DatabaseURL: dsn}; if that quietly started tracing, those suites
// would allocate a span per query against a provider nobody configured.
func TestQueryTracer_AbsentWhenNoProviderConfigured(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, container.Terminate(context.Background()))
	})
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	pool, err := NewPool(ctx, Config{DatabaseURL: dsn, EncryptionKey: "key"}, logger)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = gen.New(pool).ListRoles(ctx)
	require.NoError(t, err)
	assert.Empty(t, recorder.Ended(), "a pool built without a TracerProvider must emit no spans at all")
}

// TestQueryTracer_AcquireSpanOnRealPoolExhaustion is the half of the acquire
// story a unit test cannot tell: that pgxpool DISPATCHES to AcquireTracer at
// all, discovering it by type-asserting the same ConnConfig.Tracer value
// that carries the query and CopyFrom hooks, and that a real exhausted pool
// really does reach TraceAcquireEnd with an error.
//
// This is the case the instrumentation exists for. An acquire timeout means
// NO QUERY EVER RUNS, so before this hook the worst thing this pool can do
// to a request produced no span at all and was invisible in a trace.
//
// The three branch decisions (fast, slow, failed) are pinned deterministically
// in TestTraceAcquire_SpanOnlyWhenSlowOrFailed; this test deliberately does
// not re-assert them against wall-clock timing on a shared container.
func TestQueryTracer_AcquireSpanOnRealPoolExhaustion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _, recorder := tracedPool(ctx, t, "pool_max_conns=1")
	// Hold the pool's only connection, so the next acquire has to queue and
	// then time out.
	held, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer held.Release()
	timeoutCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	_, err = pool.Exec(timeoutCtx, "SELECT 1")
	require.Error(t, err, "the pool has exactly one connection and this test is holding it")
	span := findSpan(t, recorder, "postgres.acquire")
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Equal(t, trace.SpanKindClient, span.SpanKind())
	// Backdating proved against a REAL wait: a span constructed at
	// TraceAcquireEnd without trace.WithTimestamp would be ~instantaneous,
	// hiding the very queue time it exists to show.
	assert.GreaterOrEqual(t, span.EndTime().Sub(span.StartTime()), defaultAcquireSpanThreshold,
		"the acquire span must cover the real queue wait")
	// The failed acquire must NOT also produce a query span -- no query ran.
	for _, s := range recorder.Ended() {
		assert.NotEqual(t, "postgres."+unnamedQuery, s.Name(), "no query span should exist for a query that never reached the server")
	}
}

// TestQueryTracer_HealthProbeIsSilentUntilItIsNot is loam-om77 proven
// against a real pgx, a real pool and a real Postgres, rather than against
// this package's own types.
//
// The unit tests call TraceQueryStart and TraceQueryEnd directly, which
// assumes the very thing most likely to be wrong: that the context marked
// away up in internal/health's HTTP handler is still the context pgx hands
// the tracer, after pgxpool has acquired a connection and pgx has derived
// its own contexts along the way. A context VALUE surviving that path is a
// property of pgx, not of this package, and nothing but an integration test
// can observe it.
//
// The three phases are the whole argument for the design: the healthy probe
// is silent, the failing probe is not, and an unmarked query alongside them
// is untouched.
func TestQueryTracer_HealthProbeIsSilentUntilItIsNot(t *testing.T) {
	t.Parallel()
	ctx := context.WithoutCancel(t.Context())
	pool, _, recorder := tracedPool(ctx, t)
	probeCtx := telemetry.WithProbe(ctx)

	// PHASE 1 -- the case this bead removes. The statement is the one
	// /readyz's schema check actually issues, VERBATIM, because that is the
	// one readiness operation the tracer ever sees.
	//
	// It is deliberately NOT pool.Ping. Ping emits no span whether or not
	// the marker works (tracer.go's probeQuery explains why), so asserting
	// "no spans" after a Ping is a test that passes against an empty diff --
	// which is exactly what the first draft of this test did.
	const schemaCheckSQL = "SELECT version, dirty FROM schema_migrations"
	for range 5 {
		rows, err := pool.Query(probeCtx, schemaCheckSQL)
		require.NoError(t, err)
		rows.Close()
		require.NoError(t, rows.Err())
	}
	assert.Empty(t, spanNames(recorder),
		"a healthy health probe must leave no span at all; got %v", spanNames(recorder))

	// PHASE 2 -- the case the whole design exists to preserve. A real
	// server-side failure on a probe-marked query: the database is
	// reachable, the connection is fine, and the statement fails. That is
	// the shape of the pgvector-extension-missing failure /readyz's
	// databaseFailureReason exists to diagnose.
	_, err := pool.Exec(probeCtx, "SELECT 1/0")
	require.Error(t, err)
	span := findSpan(t, recorder, spanNamePrefix+unnamedQuery)
	assert.Equal(t, codes.Error, span.Status().Code)
	attrs := map[string]any{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	assert.Equal(t, "22012", attrs["db.response.status_code"],
		"the real SQLSTATE for division_by_zero must survive onto the deferred span")
	assert.Equal(t, true, attrs[probeAttribute],
		"a probe failure must be labelled, so a reader can tell it from a genuine unheadered query")
	assert.NotContains(t, strings.Join(allSpanText(span), " "), "1/0",
		"the deferred span must not start carrying statement text")
	recorder.Reset()

	// PHASE 3 -- the guard against overreach. The SAME pool, the SAME
	// connection, an unmarked context: still traced on success. A fix that
	// suppressed by pool, by connection or by statement shape rather than by
	// the caller's own marker would fail here.
	_, err = pool.Exec(ctx, "SELECT 1")
	require.NoError(t, err)
	assert.Equal(t, []string{spanNamePrefix + unnamedQuery}, spanNames(recorder),
		"an unmarked query must still be traced even though a probe just ran on the same pool")
	assert.NotContains(t, attrMap(findSpan(t, recorder, spanNamePrefix+unnamedQuery).Attributes()), probeAttribute)
}

// TestQueryTracer_ProbeMarkerNeverSuppressesTheAcquireFailure is why
// TraceAcquireEnd deliberately does not consult telemetry.IsProbe, checked
// against a pool that genuinely cannot connect rather than against a comment.
//
// It also pins a real asymmetry that is easy to get wrong when reasoning
// about this change: when the database is UNREACHABLE, there is no query
// span to defer, because pgx never gets far enough to trace a query. The
// acquire fails first. So if the probe marker had been extended to the
// acquire path "for consistency", a totally dead database would have
// produced NO span whatsoever from the probe -- which is precisely the
// outcome loam-om77 was required not to cause.
func TestQueryTracer_ProbeMarkerNeverSuppressesTheAcquireFailure(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	// Port 1 is reserved and refuses immediately, so this is a connect
	// failure rather than a timeout, and needs no container.
	poolCfg, err := newPoolConfig(Config{
		DatabaseURL:    "postgres://loam:loam@127.0.0.1:1/loam?sslmode=disable",
		TracerProvider: tp,
	})
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(context.WithoutCancel(t.Context()), poolCfg)
	require.NoError(t, err, "pgxpool is lazy: constructing a pool at a dead address must succeed")
	t.Cleanup(pool.Close)
	require.Error(t, pool.Ping(telemetry.WithProbe(context.WithoutCancel(t.Context()))))
	acquire := findSpan(t, recorder, spanNamePrefix+"acquire")
	assert.Equal(t, codes.Error, acquire.Status().Code,
		"a probe against a dead database must still produce the acquire failure span")
	assert.NotContains(t, spanNames(recorder), spanNamePrefix+unnamedQuery,
		"and no query span, because pgx never reached the query -- which is exactly why the acquire span must survive the marker")
}

// TestQueryTracer_PoolPingIsInvisibleToTheQueryTracer pins a property of pgx
// that this package cannot change and must not assume away.
//
// pgxpool.Pool.Ping bottoms out in pgconn.PgConn.Exec, a layer below the one
// that consults ConnConfig.Tracer, so no Ping produces a span -- traced
// pool, no marker, healthy database, still nothing. loam-om77's bead was
// written on the opposite assumption ("/readyz's DATABASE PING is traced"),
// and it is an easy assumption to keep, because every consequence of it
// looks the same as success: a suppression test that pings and asserts
// silence passes identically whether the suppression works, is broken, or
// was never written.
//
// So this asserts the fact directly, with NO marker anywhere, and pairs it
// with a statement that IS traced on the same pool in the same breath. If a
// future pgx moves Ping onto the traced path, this fails and every
// probe-suppression test in the tree needs rereading.
func TestQueryTracer_PoolPingIsInvisibleToTheQueryTracer(t *testing.T) {
	t.Parallel()
	ctx := context.WithoutCancel(t.Context())
	pool, _, recorder := tracedPool(ctx, t)
	require.NoError(t, pool.Ping(ctx))
	require.NoError(t, pool.Ping(ctx))
	assert.Empty(t, spanNames(recorder),
		"pgxpool.Pool.Ping must produce no span even completely unmarked; if it now does, the probe tests that ping and assert silence have become meaningful and must be rechecked")
	_, err := pool.Exec(ctx, "SELECT 1")
	require.NoError(t, err)
	assert.Equal(t, []string{spanNamePrefix + unnamedQuery}, spanNames(recorder),
		"the same pool must trace an ordinary unheadered statement, or the silence above proves only that the tracer is detached")
}
