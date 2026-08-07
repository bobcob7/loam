// Package health implements docs/server-spec.md -> Health: the two
// unauthenticated endpoints, and only those two, that a load balancer or
// container orchestrator probes.
//
// The two are deliberately asymmetric, and the asymmetry is the whole
// design:
//
// GET /healthz (Live) is LIVENESS. It checks nothing and depends on
// nothing -- it answers 200 for as long as this process is accepting
// connections at all. That is not laziness: a liveness probe's failure
// mode is "kill and restart the process", and restarting a Loam server
// does not repair a Postgres outage, a wedged embedder, or an unreachable
// forge. Wiring any dependency into liveness converts a downstream
// incident into a restart loop that makes the incident worse while
// destroying the in-flight sync/ingest work docs/server-spec.md ->
// Shutdown otherwise drains. It is also what makes /healthz usable as the
// startup readiness poll every integration test and every `task demo:*`
// target in this repo already relies on: reaching it at all means Startup
// got past migrate, pool connect, mirror reconcile, orphan requeue and
// the policy-socket bind, all of which fail fast BEFORE the HTTP listener
// binds (cmd/server/main.go's run()).
//
// GET /readyz (Readiness) is READINESS: 200 only if Postgres is reachable
// AND the applied migrations match this binary's embedded set
// (docs/server-spec.md -> Health names exactly those two checks), 503
// otherwise. Its failure mode is "take this instance out of rotation",
// which is the honest response to "this process cannot serve a correct
// answer to any request".
//
// What readiness deliberately does NOT check, and why:
//
//   - The EMBEDDER (Ollama). Named as excluded by docs/server-spec.md ->
//     Health and by docs/ingestion-spec.md -> Consistency & Failure: an
//     embedder outage stalls ingest jobs, which retry, and leaves every
//     RPC except semantic search fully serviceable. Draining the whole
//     server for it would be strictly worse than serving stale search.
//   - The FORGE / upstream git hosts. Same argument, one layer further
//     out, and worse: an upstream outage is exactly when an operator most
//     needs the admin console reachable to see what is failing.
//   - The POLICY SOCKET, INGEST POOL and SYNC SCHEDULER. All three are
//     in-process background components started before the listener; their
//     health is a liveness question about this process, not a readiness
//     question about its dependencies, and there is no rotation decision
//     a load balancer could usefully make from them.
//
// The line drawn is therefore: readiness checks only what makes THIS
// process unable to answer ANY request correctly, never a dependency
// whose outage merely degrades a subset of the surface. That is the
// standing guard against the cascade shape -- a readyz that checks a
// downstream turns that downstream's outage into a total outage, and,
// under a load balancer with a shared dependency, takes every replica out
// at once.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/bobcob7/loam/internal/telemetry"
)

// liveBody and readyBody are the exact bodies the two endpoints write on
// success. They are short, fixed, and machine-greppable; the probes that
// read them (this repo's Taskfile demo targets and integration harnesses)
// key on the status code, and these give a human tailing curl output
// something to read.
const (
	liveBody  = "live"
	readyBody = "ready"
)

// databaseReason, pgvectorReason and migrationsReason name WHICH readiness
// check failed in the 503 body. They are fixed strings, never the
// underlying error: an error from pgx can carry the database host, port and
// user, and this body is served to an unauthenticated caller. The full
// error is logged instead, where the operator who needs it can see it and a
// probe cannot.
//
// databaseReason stays the answer for every failure to reach Postgres,
// including an authentication failure -- docs/deployment-spec.md and
// helm/loam/templates/postgres-statefulset.yaml both tell operators to
// expect exactly that for a password that disagrees with the DSN's.
// pgvectorReason is the one failure carved out of it, because it is not a
// reachability problem at all: see databaseFailureReason.
const (
	databaseReason   = "database unreachable"
	pgvectorReason   = "pgvector extension missing or not on the search_path"
	migrationsReason = "migrations not current"
)

// pgvectorRegistrationMessage is the exact text pgvector-go's
// pgx.RegisterTypes returns when `to_regtype('vector')` comes back NULL
// (pgvector-go/pgx/register.go). internal/db.NewPool installs that function
// as the pool's AfterConnect hook, so this error surfaces from Ping -- and
// from every other acquisition -- rather than from anything vector-shaped
// the caller did.
//
// It is matched as a STRING because pgvector-go offers nothing else: the
// value is a bare fmt.Errorf with no sentinel, no type and no wrapped
// cause, so errors.Is and errors.As have nothing to bind to. Matching a
// dependency's message text is normally the wrong thing, and what makes it
// acceptable here is that the match can only REFINE the answer, never widen
// it -- an error that does not match still reports databaseReason, exactly
// as every failure did before. If pgvector-go rewords this, /readyz
// degrades to today's vaguer 503, never to a wrong one.
const pgvectorRegistrationMessage = "vector type not found in the database"

// checkTimeout bounds one whole /readyz evaluation. Without it a Postgres
// that accepts the TCP connection but never answers (a hung host, a
// blackholed network path) leaves the probe hanging instead of reporting
// not-ready -- and a probe that hangs is read as neither ready nor
// unready by most orchestrators until their own timeout fires. Answering
// 503 promptly is the useful behaviour, so this is deliberately well
// under a typical probe timeout.
const checkTimeout = 2 * time.Second

// Live returns the GET /healthz handler: unconditional 200 for as long as
// the process is serving. See this package's doc comment for why it
// checks nothing.
func Live() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writePlain(w, http.StatusOK, liveBody)
	})
}

// meterName is the instrumentation scope this package's metric carries: the
// import path, per OpenTelemetry convention.
const meterName = "github.com/bobcob7/loam/internal/health"

// durationMetric, outcomeAttribute and readyOutcome name the readiness
// metric and its one dimension.
//
// readyOutcome is a SEPARATE constant from readyBody even though the two
// currently hold the same word. They answer to different audiences: the body
// is served to a probe and may be reworded freely, while the attribute value
// is what an alert rule and a dashboard match on, and silently renaming a
// metric dimension by editing an HTTP response body is a trap worth not
// leaving behind.
//
// The FAILING values are the reason constants above (databaseReason,
// pgvectorReason, migrationsReason), so the cardinality of this dimension is
// four and is fixed at compile time. Nothing derived from an error message
// or from request input ever reaches it -- see the reason constants for why
// those strings are fixed in the first place.
const (
	durationMetric   = "loam.readiness.check.duration"
	outcomeAttribute = "loam.readiness.outcome"
	readyOutcome     = "ready"
)

// durationBuckets are this histogram's explicit bucket boundaries, in
// SECONDS. They are supplied rather than defaulted because OpenTelemetry's
// default boundaries (0, 5, 10, 25 ... 10000) are sized for MILLISECONDS: a
// readiness check bounded at checkTimeout=2s would land every observation in
// the first bucket, and the histogram would carry no distribution at all --
// a failure mode that still produces a healthy-looking metric, which is the
// worst kind.
//
// The range is chosen around the two numbers that matter: a local Postgres
// answers in single-digit milliseconds, and checkTimeout cuts the check off
// at 2s. The bound past it exists so a check that somehow overruns is
// visible as an overflow rather than merged into the last real bucket.
var durationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5}

// Readiness is the GET /readyz handler.
type Readiness struct {
	db       Pinger
	schema   SchemaChecker
	duration metric.Float64Histogram
	logger   *slog.Logger
}

// NewReadiness builds the /readyz handler over the live pool (db) and the
// migration-status check bound to that same pool (schema).
//
// # WHY THIS TAKES A METER, AND WHY A METRIC RATHER THAN A TRACE
//
// Before loam-om77 the only record that a readiness check had happened was
// the SPAN its database ping incidentally produced, and there were ~34,000
// of them a day from an idle replica (internal/db's probeQuery has the
// measurement). Those spans are now deferred until a probe fails, which
// closes the volume problem but would, on its own, leave the healthy case
// with no signal whatsoever -- and "we stopped seeing evidence of health" is
// not something anyone can alert on, because it is indistinguishable from a
// collector outage or from the instrumentation having been deleted.
//
// A metric is the instrument this question always wanted. "Can this process
// reach its database" is asked on a fixed interval and answered identically
// thousands of times a day; that is a TIME SERIES. A trace answers "what
// happened during THIS request", which is a question nobody has about the
// 8,000th successful poll. The metric is emitted on EVERY probe, ready or
// not, so the healthy stream is present and its disappearance or its flip to
// a failing outcome is a legitimate alarm -- which is exactly what the wall
// of root traces could never be used for.
//
// mp is never nil in production: telemetry.Provider hands out upstream's
// no-op MeterProvider when telemetry is disabled, so this records into
// nothing rather than branching. A nil mp is tolerated anyway (the handler
// simply records nothing) so the many tests that construct a Readiness
// directly do not all have to grow a provider.
func NewReadiness(db Pinger, schema SchemaChecker, mp metric.MeterProvider, logger *slog.Logger) *Readiness {
	rd := &Readiness{db: db, schema: schema, logger: logger}
	if mp == nil {
		return rd
	}
	duration, err := mp.Meter(meterName).Float64Histogram(durationMetric,
		metric.WithDescription("Duration of one /readyz evaluation, by outcome."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(durationBuckets...),
	)
	if err != nil {
		// A metric that cannot be created must not stop a process from
		// reporting its readiness -- that would make observability a
		// dependency of serving, which is the inversion this whole endpoint
		// exists to avoid. Log it and serve on, unmetered.
		logger.Warn("readiness metric unavailable", "metric", durationMetric, "error", err)
		return rd
	}
	rd.duration = duration
	return rd
}

// ServeHTTP implements http.Handler.
//
// The two checks are ordered and short-circuiting, not collected: the
// schema check is itself a query over the same pool, so whatever stopped
// the ping from getting a usable connection stops it too, and it could
// only fail for a second, derived reason. Reporting the first, causal
// failure is more useful to an operator than reporting both.
//
// That short-circuit is exactly why the ping's own failure has to be NAMED
// rather than assumed: it is the only reason this endpoint will ever
// report, so whatever it claims is the whole of what the operator gets.
// See databaseFailureReason for the claim that had to stop being made.
//
// The context handed to both checks is marked with telemetry.WithProbe, and
// that marker is the load-bearing half of loam-om77. BOTH checks are
// database work -- Ping execs a bare ";" and CheckSchema runs a hand-written
// SELECT, neither carrying an sqlc name -- so before the marker existed each
// poll emitted two parentless postgres.unnamed root traces, roughly 34,000 a
// day at a 5s interval. Marking here rather than guessing in internal/db is
// deliberate: this handler is the only thing in the process that KNOWS the
// work is a health probe, and the same inference that would catch it would
// also silence the sync scheduler and ingest, whose root traces are the only
// record they leave. See telemetry.WithProbe.
//
// It marks the whole evaluation rather than just the ping, so a check added
// here later inherits the policy instead of quietly reopening the problem.
func (rd *Readiness) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(telemetry.WithProbe(r.Context()), checkTimeout)
	defer cancel()
	started := time.Now()
	reason, err := rd.check(ctx)
	rd.record(ctx, reason, time.Since(started))
	if err != nil {
		rd.notReady(ctx, w, reason, err)
		return
	}
	writePlain(w, http.StatusOK, readyBody)
}

// check runs the two ordered, short-circuiting readiness checks and reports
// the NAMED reason for the first failure alongside the underlying error.
// Split out of ServeHTTP so the outcome is a value the metric can be
// recorded from on every path, including the failing ones -- the previous
// shape returned from inside each branch, and a metric bolted onto that
// would have been recorded on the healthy path and forgotten on one of the
// failing ones sooner or later.
func (rd *Readiness) check(ctx context.Context) (string, error) {
	if err := rd.db.Ping(ctx); err != nil {
		return databaseFailureReason(err), err
	}
	if err := rd.schema.CheckSchema(ctx); err != nil {
		return migrationsReason, err
	}
	return readyOutcome, nil
}

// record observes one completed readiness evaluation. It is called for EVERY
// probe, healthy or not: see NewReadiness for why the healthy observations
// are the point rather than the overhead.
//
// ctx is the (possibly already cancelled or expired) check context. That is
// intentional and safe -- the OpenTelemetry metric API does not abort a
// Record on a cancelled context, and passing the real context keeps whatever
// baggage or exemplar linkage it carries -- but it is the reason the elapsed
// time is measured by the caller rather than here.
func (rd *Readiness) record(ctx context.Context, outcome string, elapsed time.Duration) {
	if rd.duration == nil {
		return
	}
	rd.duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(attribute.String(outcomeAttribute, outcome)))
}

// databaseFailureReason names WHICH database failure Ping reported, and
// exists because "the ping failed" and "Postgres is unreachable" are not
// the same statement.
//
// PGXPOOL IS LAZY. internal/db.NewPool's AfterConnect hook runs on EVERY
// connection the pool opens, not just the first, and pgxpool fails the
// whole acquisition -- Ping included -- when it errors. So a pool that
// connected cleanly at startup begins failing every later acquisition the
// moment the pgvector extension is dropped, a backup is restored without
// it, a failover lands on a database that lacks it, or a search_path change
// hides the type. Postgres is up, reachable, authenticating and serving;
// what this process cannot do is finish setting up a connection to it.
//
// Reporting that as "database unreachable" pointed operators at the
// network for a problem that lives in the schema, and the short-circuit in
// ServeHTTP means the schema check never gets to say anything either. So
// the one failure that is diagnosable from the error itself is diagnosed
// here. Everything else keeps the old, deliberately broad reason: see the
// constants above for why an authentication failure in particular must
// stay under databaseReason.
func databaseFailureReason(err error) string {
	if strings.Contains(err.Error(), pgvectorRegistrationMessage) {
		return pgvectorReason
	}
	return databaseReason
}

// notReady logs the full failure and serves the short, reason-naming 503.
func (rd *Readiness) notReady(ctx context.Context, w http.ResponseWriter, reason string, err error) {
	rd.logger.WarnContext(ctx, "readiness check failed", "check", reason, "error", err)
	writePlain(w, http.StatusServiceUnavailable, "not ready: "+reason)
}

// writePlain writes a plain-text health response. Cache-Control: no-store
// is not decoration: a probe response cached by any intermediary would
// report a stale verdict, which for /readyz means routing traffic to an
// instance that already said it could not serve it.
func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
