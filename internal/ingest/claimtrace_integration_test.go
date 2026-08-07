//go:build integration

// This file is loam-gp7m's proof, and it needs a real Postgres for the same
// reason internal/db's tracer_integration_test.go does: the property under
// test is that a context VALUE set in this package survives pgxpool's
// connection acquisition and pgx's own context derivation all the way to
// internal/db's queryTracer. That is a property of pgx, not of this package,
// and a unit test with a fake tracer would assert that the code calls the
// code.
//
// See integration_test.go's header for the build tag and the podman
// TESTCONTAINERS_RYUK_DISABLED note.
package ingest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
)

// probeSpanAttribute is internal/db's probeAttribute, which is unexported
// there and cannot be imported. It is duplicated deliberately and is the
// single most load-bearing string in this file: it is what separates "the
// failing claim produced a span" -- true of a correct fix AND of an empty
// diff -- from "the failing claim produced a DEFERRED span", which is only
// true when the marker actually reached the tracer.
const probeSpanAttribute = "loam.probe"

// unnamedSpanName is internal/db's spanNamePrefix + unnamedQuery, likewise
// unexported there. Every statement this file provokes is hand-written SQL
// with no sqlc `-- name:` header, so they ALL land on this one name -- which
// is why the assertions below turn on span COUNT, on the probe attribute,
// and on what a neighbouring phase proved, rather than on any span name
// telling one statement from another.
const unnamedSpanName = "postgres.unnamed"

// newTracedTestPool is newTestPool (integration_test.go) with a recording
// TracerProvider attached the way cmd/server attaches one, plus the recorder
// to read spans back from. It is a separate helper rather than a parameter
// on newTestPool so this bead adds no edit to a file five other test files
// depend on.
//
// TWO SETTINGS ARE LOAD-BEARING, and both exist to stop this test from
// reporting spans that have nothing to do with the claim loop:
//
//   - pool_max_conns=1. db.NewPool installs pgvector-go's RegisterTypes as
//     AfterConnect, which issues an unheadered `to_regtype('vector')` on
//     EVERY new connection -- so a pool that opened a second connection
//     mid-test would drop an extra postgres.unnamed span into the recorder
//     and fail an "idle claims are silent" assertion for a reason that is
//     not the code under test. One connection, opened and warmed by
//     NewPool's own Ping before the recorder is reset, closes that.
//   - AcquireSpanThreshold an hour. internal/db emits an acquire span for
//     any acquire slower than the threshold, which on a cold container is a
//     coin flip against the 50ms default. Raising it suppresses only the
//     SLOW-acquire span; a FAILED acquire is recorded regardless of this
//     value (internal/db's TraceAcquireEnd), so the safety net this bead
//     leans on in claimOnce's doc comment is still armed here.
func newTracedTestPool(ctx context.Context, t *testing.T) (*pgxpool.Pool, *tracetest.SpanRecorder) {
	t.Helper()
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
	// Migrate on the PLAIN dsn and pool on the parameterised one:
	// migrations.Migrate goes through database/sql, which forwards
	// pool_max_conns to the server as a startup option and is rejected with
	// SQLSTATE 42704. See internal/db's tracedPool for the same note.
	baseDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, baseDSN, testLogger()))
	dsn, err := container.ConnectionString(ctx, "sslmode=disable", "pool_max_conns=1")
	require.NoError(t, err)
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	pgPool, err := db.NewPool(ctx, db.Config{
		DatabaseURL:          dsn,
		TracerProvider:       tp,
		AcquireSpanThreshold: time.Hour,
	}, testLogger())
	require.NoError(t, err)
	t.Cleanup(pgPool.Close)
	return pgPool, recorder
}

// spanNames returns the recorded span names, for readable failure messages.
func spanNames(recorder *tracetest.SpanRecorder) []string {
	var names []string
	for _, span := range recorder.Ended() {
		names = append(names, span.Name())
	}
	return names
}

// returnedRows returns each recorded span's db.response.returned_rows,
// SORTED. It is a readability aid, not the discriminator -- and saying so is
// the point of this comment, because an earlier version of it claimed the
// opposite and the claim was false.
//
// WHAT IT CANNOT DO. internal/db deliberately records no db.query.text and
// every statement here is unheadered, so all of them share the span name
// postgres.unnamed and the row count is the only per-span detail left. But
// it is a multiset of three small integers: it cannot tell the two UPDATEs
// apart from each other or from the SELECT, nor Begin from COMMIT from
// Rollback. It admits ANY three-statement subset with two 1-row statements
// and one 0-row statement -- nine of them -- of which the write half is one.
// Review demonstrated this with a compiling mutant that marked from after
// the first UPDATE, silencing the sync_state write and the COMMIT: the
// traced set became Begin(0)+SELECT(1)+UPDATE(1), which sorts to {0,1,1}
// exactly as UPDATE(1)+UPDATE(1)+COMMIT(0) does, and this assertion did not
// notice.
//
// WHAT ACTUALLY PINS THE WRITE HALF is the pair of facts either side of it:
// PHASE 1 establishes that Begin, the SELECT and the Rollback are silent on
// an idle tick, and Begin and the SELECT are issued BEFORE the outcome is
// knowable -- so nothing can trace them on a work tick while keeping them
// silent on an idle one. Given "idle is silent" and "exactly three spans on
// a work tick", the three can only be the write half. That argument is
// carried by PHASE 1 plus the span-count assertion, and this function only
// makes the resulting failure message legible.
func returnedRows(t *testing.T, recorder *tracetest.SpanRecorder) []int64 {
	t.Helper()
	var rows []int64
	for _, span := range recorder.Ended() {
		got, ok := spanAttr(span.Attributes(), "db.response.returned_rows")
		require.Truef(t, ok, "every successful db span must carry a row count; %s did not", span.Name())
		rows = append(rows, got.AsInt64())
	}
	slices.Sort(rows)
	return rows
}

// spanAttr looks one attribute up on a recorded span.
func spanAttr(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// attrKeys is spanAttr's negative form, for asserting an attribute is ABSENT
// with a failure message that says what was there instead.
func attrKeys(attrs []attribute.KeyValue) []string {
	keys := make([]string, 0, len(attrs))
	for _, kv := range attrs {
		keys = append(keys, string(kv.Key))
	}
	return keys
}

// TestClaim_IdleQueueIsSilentButWorkAndFailureAreNot is the whole bead in
// one run, and it is deliberately ONE test rather than four: the assertion
// that matters is not "idle claims are silent" -- which passes against a
// working fix, a broken fix that suppresses everything, and an EMPTY DIFF
// that never traced in the first place -- but that silence and signal
// coexist on the same pool, in the same breath, minutes apart in code.
// loam-om77's first attempt made exactly that mistake and was caught in
// review.
//
// THE THREE POSITIVE CONTROLS, each killing a different wrong fix:
//
//	A. An unmarked statement on the SAME pool and the SAME connection is
//	   still traced. Kills any fix that suppressed by pool, by connection,
//	   by statement shape, or by turning the tracer off.
//	B. A claim that FINDS WORK is still traced, and traced as its WRITE
//	   half. Kills the per-tick marker -- the failure mode the bead calls
//	   worse than the bug -- which would take a successful claim dark and
//	   would be invisible in exactly the direction nobody checks. This is
//	   the ONLY control that catches that mutant. What identifies the three
//	   spans as the write half is not their row counts (see returnedRows
//	   for the mutant those miss) but PHASE 1 above: Begin and the SELECT
//	   run before the outcome is knowable, so nothing can trace them here
//	   while keeping them silent there.
//	C. A claim whose probe-marked SELECT FAILS still produces a span, and
//	   that span carries loam.probe=true. Kills the blanket suppression --
//	   the one thing telemetry.WithProbe's doc comment says is worse than
//	   not fixing anything -- and the attribute is what makes it fail
//	   against an empty diff, which would produce an unflagged span here.
//
// And the closing metric assertion is the fourth control on the bead's own
// terms: the signal MOVED, it did not disappear.
func TestClaim_IdleQueueIsSilentButWorkAndFailureAreNot(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool, recorder := newTracedTestPool(ctx, t)
	mp, reader := claimMeter(t)
	pool := NewPool(testLogger(), pgPool, &OrchestratorMock{}, 1, WithMeterProvider(mp))

	// PHASE 1 -- the bug. Five claims against an empty queue. In production
	// this is fifteen unheadered root spans every five seconds per worker;
	// here it must be none at all.
	recorder.Reset()
	for range 5 {
		job, claimed, err := pool.claim(ctx)
		require.NoError(t, err)
		require.False(t, claimed, "the queue is empty; nothing may be claimed")
		require.Zero(t, job.ID)
	}
	assert.Empty(t, spanNames(recorder),
		"an idle claim must leave no span at all -- begin, the FOR UPDATE SKIP LOCKED select and the rollback are all speculative; got %v",
		spanNames(recorder))

	// CONTROL A -- the guard against overreach. Same pool, same single
	// connection, an unmarked statement: still traced.
	recorder.Reset()
	repoID := seedRepo(ctx, t, pgPool, "group/claim-trace")
	assert.Equal(t, []string{unnamedSpanName}, spanNames(recorder),
		"an unmarked statement must still be traced even though idle claims just ran on the same connection")
	assert.NotContains(t, attrKeys(recorder.Ended()[0].Attributes()), probeSpanAttribute,
		"ordinary work must never be labelled as a probe")

	// CONTROL B -- the failure mode the bead calls worse than the bug. A
	// claim that finds work is not polling; it is a job starting, and
	// somebody wants that trace.
	jobID := insertQueuedJob(ctx, t, pgPool, repoID, "main", KindFull)
	recorder.Reset()
	job, claimed, err := pool.claim(ctx)
	require.NoError(t, err)
	require.True(t, claimed, "a queued job must be claimable")
	require.Equal(t, jobID, job.ID)
	// THIS is the assertion that pins the write half, in concert with PHASE
	// 1: Begin and the SELECT cannot be traced here while staying silent
	// there, because they run before the outcome is knowable. So "idle
	// silent" plus "exactly three" leaves only update+update+commit.
	assert.Equal(t, []string{unnamedSpanName, unnamedSpanName, unnamedSpanName}, spanNames(recorder),
		"a work-finding claim must still trace its write half: both UPDATEs and the COMMIT; got %v", spanNames(recorder))
	// A legibility check, NOT a discriminator -- see returnedRows for the
	// mutant it fails to notice. It is kept because it costs nothing and
	// makes a real failure readable.
	assert.Equal(t, []int64{0, 1, 1}, returnedRows(t, recorder),
		"the three surviving spans should read update(1 row) + update(1 row) + commit(0 rows)")
	for _, span := range recorder.Ended() {
		assert.NotContains(t, attrKeys(span.Attributes()), probeSpanAttribute,
			"a claim that found work is not a probe and must not be labelled as one")
		assert.Equal(t, codes.Unset, span.Status().Code)
	}

	// CONTROL C -- the property telemetry.WithProbe's doc comment calls
	// non-negotiable: the marker defers a span, it does not delete one. A
	// claim loop that cannot read its own table must stay diagnosable from
	// traces alone.
	//
	// Renaming the table makes the PROBE-MARKED select fail (42P01,
	// undefined_table), which is the statement whose success is suppressed
	// -- unlike TestClaim_NonGuardErrorStillSurfaces's dropped column, which
	// fails the unmarked UPDATE and would prove nothing here. This is last
	// because it is destructive; newTracedTestPool gives every test its own
	// container.
	_, err = pgPool.Exec(ctx, `ALTER TABLE ingest_jobs RENAME TO ingest_jobs_renamed_away`)
	require.NoError(t, err)
	recorder.Reset()
	_, claimed, err = pool.claim(ctx)
	require.Error(t, err, "a claim against a missing table is a defect, not contention")
	require.False(t, claimed)
	require.Len(t, recorder.Ended(), 1,
		"a failing probe-marked select must produce exactly one span -- the deferred one; got %v", spanNames(recorder))
	failure := recorder.Ended()[0]
	assert.Equal(t, unnamedSpanName, failure.Name())
	assert.Equal(t, codes.Error, failure.Status().Code)
	sqlstate, ok := spanAttr(failure.Attributes(), "db.response.status_code")
	require.True(t, ok, "the deferred span must carry the SQLSTATE, which is the whole of what internal/db records about a failure")
	assert.Equal(t, "42P01", sqlstate.AsString())
	probe, ok := spanAttr(failure.Attributes(), probeSpanAttribute)
	require.True(t, ok,
		"the failure span must be FLAGGED as a deferred probe span. Without this assertion the whole phase passes against an empty diff, which would trace this failure as an ordinary unflagged span")
	assert.True(t, probe.AsBool())

	// THE REPLACEMENT SIGNAL. Five idle polls, one claim, one failure --
	// every one of them still observable, as a time series instead of as a
	// wall of traces. "contended" is exercised separately, in
	// TestClaim_EveryCandidateRejected_ReportsNothingToClaimNotAnError,
	// which already owns the machinery that forces it deterministically.
	assert.Equal(t, map[string]int64{
		claimOutcomeEmpty:   5,
		claimOutcomeClaimed: 1,
		claimOutcomeFailed:  1,
	}, claimCounts(ctx, t, reader),
		"the traces were removed on the promise that this counter replaces them; it must count every poll the suppression made invisible")
}
