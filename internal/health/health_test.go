package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// okPinger and okSchema are the passing halves, so a test that varies one
// dependency states only that one.
func okPinger() *PingerMock {
	return &PingerMock{PingFunc: func(context.Context) error { return nil }}
}

func okSchema() *SchemaCheckerMock {
	return &SchemaCheckerMock{CheckSchemaFunc: func(context.Context) error { return nil }}
}

// get drives handler with an unauthenticated GET and returns the recorder.
func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestLive_Returns200AndTouchesNoDependency is the liveness contract from
// this package's doc comment, asserted rather than merely documented:
// Live() takes no dependency at all, so there is no argument by which a
// downstream outage could ever make it fail. The proof is structural --
// the constructor has no parameters to pass a failing collaborator
// through -- and the assertion here pins the response it produces.
func TestLive_Returns200AndTouchesNoDependency(t *testing.T) {
	t.Parallel()
	rec := get(t, Live(), "/healthz")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, liveBody, rec.Body.String())
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

// TestLive_StaysOKWhileReadinessFails is the asymmetry that keeps a
// dependency outage from becoming a restart loop: the SAME process, at
// the SAME moment, reports live but not ready. If a future change ever
// wires a dependency into /healthz, this fails.
func TestLive_StaysOKWhileReadinessFails(t *testing.T) {
	t.Parallel()
	down := &PingerMock{PingFunc: func(context.Context) error { return errors.New("connection refused") }}
	ready := NewReadiness(down, okSchema(), nil, testLogger())
	assert.Equal(t, http.StatusServiceUnavailable, get(t, ready, "/readyz").Code)
	assert.Equal(t, http.StatusOK, get(t, Live(), "/healthz").Code,
		"a failed readiness check must never make the process report not-live: restarting it would not fix a downstream outage")
}

// TestReadiness_BothChecksPass_Returns200 is the happy path, and it also
// pins that BOTH checks actually ran -- a handler that returned 200
// without consulting its collaborators would pass a status-code-only
// assertion.
func TestReadiness_BothChecksPass_Returns200(t *testing.T) {
	t.Parallel()
	pinger, schema := okPinger(), okSchema()
	rec := get(t, NewReadiness(pinger, schema, nil, testLogger()), "/readyz")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, readyBody, rec.Body.String())
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Len(t, pinger.PingCalls(), 1, "readiness must actually ping the pool, not assume it")
	assert.Len(t, schema.CheckSchemaCalls(), 1, "readiness must actually check the migration state, not assume it")
}

// TestReadiness_PingFails_Returns503NamingTheDatabase covers the
// "Postgres reachable" half of docs/server-spec.md -> Health: a pool that
// has lost its backend AFTER startup (startup's own connect already
// succeeded, by definition, or nothing would be answering this request at
// all) must take the instance out of rotation.
//
// It also pins the short-circuit: with the database unreachable the
// schema check can only fail for a derived reason, so it is not run.
func TestReadiness_PingFails_Returns503NamingTheDatabase(t *testing.T) {
	t.Parallel()
	pinger := &PingerMock{PingFunc: func(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: connect: connection refused") }}
	schema := okSchema()
	rec := get(t, NewReadiness(pinger, schema, nil, testLogger()), "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "not ready: "+databaseReason, rec.Body.String())
	assert.Empty(t, schema.CheckSchemaCalls(), "an unreachable database must short-circuit before the schema query, which runs over that same pool")
}

// TestReadiness_PgvectorRegistrationFails_NamesTheExtensionNotTheNetwork
// is this bead's readiness-side proof. pgxpool is lazy: internal/db.NewPool
// installs pgvector-go's type registration as AfterConnect, which runs on
// every connection the pool opens, so dropping the extension out from under
// a long-running process makes Ping fail while Postgres is up, reachable
// and authenticating perfectly. Reporting that as "database unreachable"
// sent operators to the network for a schema problem -- and ServeHTTP's
// short-circuit means the schema check never gets to say otherwise.
//
// The error is wrapped, deliberately: pgxpool returns AfterConnect's error
// through its own acquisition path, so the message an operator's /readyz
// actually reflects is never the bare string.
func TestReadiness_PgvectorRegistrationFails_NamesTheExtensionNotTheNetwork(t *testing.T) {
	t.Parallel()
	pinger := &PingerMock{PingFunc: func(context.Context) error {
		return fmt.Errorf("acquiring connection: %w", errors.New(pgvectorRegistrationMessage))
	}}
	schema := okSchema()
	rec := get(t, NewReadiness(pinger, schema, nil, testLogger()), "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "not ready: "+pgvectorReason, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "unreachable",
		"Postgres is up and answering here; only the pool's per-connection type registration failed")
	assert.Empty(t, schema.CheckSchemaCalls(),
		"the schema check runs over the same pool and would fail at the same acquisition, so the ping's reason is the whole of what the operator gets")
}

// TestReadiness_UnrecognisedPingFailure_StillReportsTheBroadDatabaseReason
// pins the direction the pgvector carve-out is allowed to work in: it
// REFINES, it never widens. Everything not positively identified keeps the
// old, deliberately broad reason -- including an authentication failure,
// which docs/deployment-spec.md and helm/loam's postgres-statefulset
// comment both tell operators to expect as "database unreachable".
func TestReadiness_UnrecognisedPingFailure_StillReportsTheBroadDatabaseReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{name: "dial failure", err: errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")},
		{name: "authentication failure", err: errors.New(`failed to connect: FATAL: password authentication failed for user "loam" (SQLSTATE 28P01)`)},
		{name: "context deadline", err: context.DeadlineExceeded},
		{name: "unknown", err: errors.New("boom")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pinger := &PingerMock{PingFunc: func(context.Context) error { return tt.err }}
			rec := get(t, NewReadiness(pinger, okSchema(), nil, testLogger()), "/readyz")
			assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.Equal(t, "not ready: "+databaseReason, rec.Body.String())
		})
	}
}

// TestReadiness_SchemaCheckFails_Returns503NamingMigrations covers the
// "migrations current" half: a reachable database whose schema does not
// match this binary's embedded set is not something this process can
// serve correctly, however healthy the connection is.
func TestReadiness_SchemaCheckFails_Returns503NamingMigrations(t *testing.T) {
	t.Parallel()
	schema := &SchemaCheckerMock{CheckSchemaFunc: func(context.Context) error {
		return errors.New("migration state is dirty: version 2")
	}}
	rec := get(t, NewReadiness(okPinger(), schema, nil, testLogger()), "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "not ready: "+migrationsReason, rec.Body.String())
	assert.Len(t, schema.CheckSchemaCalls(), 1)
}

// TestReadiness_FailureBodyCarriesNoUnderlyingErrorDetail is a
// disclosure guard, not a formatting preference. /readyz is
// unauthenticated (docs/web-spec.md -> Auth: "the only such exemption"),
// so anything reaching its body reaches an anonymous caller; a pgx
// connection error routinely names the database host, port and user. The
// detail must go to the log instead -- asserted here in the same test, so
// "stopped leaking" can never be silently achieved by dropping the
// detail entirely.
func TestReadiness_FailureBodyCarriesNoUnderlyingErrorDetail(t *testing.T) {
	t.Parallel()
	const secretish = "dial tcp 10.11.12.13:5432: FATAL: password authentication failed for user loam"
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	pinger := &PingerMock{PingFunc: func(context.Context) error { return errors.New(secretish) }}
	rec := get(t, NewReadiness(pinger, okSchema(), nil, logger), "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.NotContains(t, rec.Body.String(), "10.11.12.13", "the 503 body is served to an unauthenticated caller and must not carry connection detail")
	assert.NotContains(t, rec.Body.String(), "password")
	assert.Contains(t, logged.String(), secretish, "the detail the body withholds must still reach the operator's log")
}

// TestReadiness_LogsTheFailingCheckStructurally pins the log record a
// running server emits when it goes unready -- the observable an
// integration test drives the compiled binary against. Parsing the JSON,
// rather than substring-matching the whole line, keeps the assertion on
// the record's structure (message plus a `check` attribute) rather than
// on slog's formatting.
func TestReadiness_LogsTheFailingCheckStructurally(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		readiness *Readiness
		wantCheck string
	}{
		{
			name: "database",
			readiness: NewReadiness(
				&PingerMock{PingFunc: func(context.Context) error { return errors.New("boom") }},
				okSchema(), nil, nil,
			),
			wantCheck: databaseReason,
		},
		{
			name: "migrations",
			readiness: NewReadiness(
				okPinger(),
				&SchemaCheckerMock{CheckSchemaFunc: func(context.Context) error { return errors.New("boom") }},
				nil, nil,
			),
			wantCheck: migrationsReason,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logged bytes.Buffer
			tt.readiness.logger = slog.New(slog.NewJSONHandler(&logged, nil))
			require.Equal(t, http.StatusServiceUnavailable, get(t, tt.readiness, "/readyz").Code)
			var record struct {
				Level   string `json:"level"`
				Message string `json:"msg"`
				Check   string `json:"check"`
			}
			require.NoError(t, json.NewDecoder(strings.NewReader(logged.String())).Decode(&record))
			assert.Equal(t, "readiness check failed", record.Message)
			assert.Equal(t, tt.wantCheck, record.Check)
			assert.Equal(t, "WARN", record.Level, "an unready instance is a warning, not an error: it is the designed response to a dependency outage")
		})
	}
}

// TestReadiness_ChecksRunPerRequest is the property that makes /readyz
// worth more than startup's own fail-fast: the verdict is re-derived on
// every request, so an instance that recovers goes back into rotation
// without a restart, and one that degrades leaves it without one.
func TestReadiness_ChecksRunPerRequest(t *testing.T) {
	t.Parallel()
	var healthy bool
	pinger := &PingerMock{PingFunc: func(context.Context) error {
		if healthy {
			return nil
		}
		return errors.New("connection refused")
	}}
	readiness := NewReadiness(pinger, okSchema(), nil, testLogger())
	assert.Equal(t, http.StatusServiceUnavailable, get(t, readiness, "/readyz").Code)
	healthy = true
	assert.Equal(t, http.StatusOK, get(t, readiness, "/readyz").Code, "readiness must be re-evaluated per request, never cached from startup or from a previous probe")
	healthy = false
	assert.Equal(t, http.StatusServiceUnavailable, get(t, readiness, "/readyz").Code)
	assert.Len(t, pinger.PingCalls(), 3)
}

// TestReadiness_BoundsHowLongAChecktakes proves a Postgres that accepts
// the connection but never answers produces a prompt 503 rather than a
// hanging probe: the handler's own deadline, not the caller's patience,
// is what ends the wait. The fake blocks until its ctx is done, which
// can only happen via checkTimeout here -- the request context this test
// passes is never canceled.
func TestReadiness_BoundsHowLongACheckTakes(t *testing.T) {
	t.Parallel()
	pinger := &PingerMock{PingFunc: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	rec := get(t, NewReadiness(pinger, okSchema(), nil, testLogger()), "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "not ready: "+databaseReason, rec.Body.String())
}

// TestReadiness_PassesADeadlineToItsChecks is the same property observed
// from the collaborator's side: the context a check receives already
// carries a deadline, so a check that respects ctx cannot outlive the
// probe regardless of what it does internally.
func TestReadiness_PassesADeadlineToItsChecks(t *testing.T) {
	t.Parallel()
	var pingDeadline, schemaDeadline bool
	pinger := &PingerMock{PingFunc: func(ctx context.Context) error {
		_, pingDeadline = ctx.Deadline()
		return nil
	}}
	schema := &SchemaCheckerMock{CheckSchemaFunc: func(ctx context.Context) error {
		_, schemaDeadline = ctx.Deadline()
		return nil
	}}
	require.Equal(t, http.StatusOK, get(t, NewReadiness(pinger, schema, nil, testLogger()), "/readyz").Code)
	assert.True(t, pingDeadline, "the pool ping must run under the handler's own deadline")
	assert.True(t, schemaDeadline, "the schema check must run under the handler's own deadline")
}
