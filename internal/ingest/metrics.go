package ingest

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the instrumentation scope this package's metrics carry: the
// import path, per OpenTelemetry convention, matching internal/health's
// meterName and internal/db's tracerName.
const meterName = "github.com/bobcob7/loam/internal/ingest"

// claimCyclesMetric and claimOutcomeAttribute name the claim-loop counter
// and its one dimension.
//
// # WHY THIS COUNTER EXISTS, AND WHAT IT IS NOT
//
// It is the replacement signal for loam-gp7m. Before that bead, the only
// evidence an ingest worker was alive and reaching Postgres was the burst of
// root traces its idle claim emitted every poll interval -- ~121,000 a day
// from a replica with nothing to do (claimOnce's PROBE BOUNDARY section has
// the measurement). Those are now suppressed on the idle path, and
// suppressing them WITHOUT this counter would have traded a noise problem
// for a blind spot: "we stopped seeing evidence of claiming" would then be
// indistinguishable from a dead worker, a collector outage, and the
// instrumentation having been deleted.
//
// "Is the ingest worker alive and claiming" is asked on a fixed interval and
// answered identically thousands of times a day. That is a TIME SERIES, not
// a trace, and it is a strictly better instrument for the question than the
// wall of traces ever was: the rate of this counter IS the claim rate, its
// disappearance is alertable, and outcome=claimed separates "polling" from
// "working" -- a distinction the traces could not express at all, since an
// idle claim and a working one produced the same unnamed spans.
//
// # WHAT IT IS NOT, so loam-e9vh does not duplicate it
//
// loam-e9vh is the open bead for ingest-PIPELINE metrics, built from the
// counters loam-c94.20/21/24 already maintain: files rejected by the store
// per repo, files skipped as binary or chunkless, job duration and outcome,
// ingest_jobs queue depth, embedding batch latency. Every one of those
// measures what a RUNNING JOB did. This one measures only whether the claim
// loop is turning and what it found, which is upstream of all of them and is
// on none of e9vh's lists. The two are complementary; the seam they share is
// WithMeterProvider, which this bead adds and e9vh should reuse rather than
// introduce a second time.
//
// # CARDINALITY
//
// One dimension, four compile-time-fixed values (the claimOutcome constants
// below). Nothing derived from a repo, a job id, an error message or any
// other input reaches this metric -- e9vh's own bead text states the rule
// this package follows: identities belong on SPANS, counts belong on
// METRICS. Repo identity in particular is deliberately absent: it is bounded
// by enrolment rather than by anything this package controls, and the
// question this counter answers ("is the loop turning") is not per-repo.
const (
	claimCyclesMetric     = "loam.ingest.claim.cycles"
	claimOutcomeAttribute = "loam.ingest.claim.outcome"
)

// The four outcomes of one Pool.claim call, and the complete value set of
// claimOutcomeAttribute. They are the four ways that function returns, which
// is what makes the set exhaustive by construction rather than by comment:
//
//   - claimed: a job was claimed and is about to run. The rate of this one
//     is the only view of "work is starting" that survives the idle
//     suppression, and it is the numerator an operator compares against
//     queue depth (loam-e9vh) to see a stalled pool.
//   - empty: the queue held nothing this worker could take. The dominant
//     outcome on a healthy idle deployment, and the one whose TRACES this
//     bead removed -- so its continued presence here is what proves the
//     loop is still turning.
//   - contended: every candidate up to maxClaimAttempts was already running
//     elsewhere. Distinguished from empty on purpose: they are identical in
//     claim's return values (no job, no error) but mean opposite things --
//     nothing to do, versus plenty to do and someone else doing it. It is
//     the metric counterpart of claimExhaustedMsg's WARN.
//   - failed: the claim hit an error that is not the guard constraint, and
//     work() logged it at ERROR. A non-zero rate here is a defect, not
//     contention.
const (
	claimOutcomeClaimed   = "claimed"
	claimOutcomeEmpty     = "empty"
	claimOutcomeContended = "contended"
	claimOutcomeFailed    = "failed"
)

// WithMeterProvider gives a Pool the meter it records claim-loop metrics
// into. Without it a Pool records nothing at all and behaves exactly as it
// did before loam-gp7m -- which is why every existing construction site,
// and the several dozen tests that build a Pool directly, need no change.
//
// It is an Option rather than a constructor parameter for the reason
// internal/health's NewReadiness takes mp as a plain interface: telemetry is
// not a dependency of doing the work, and a Pool that cannot create its
// counter must still claim and run jobs. A nil provider is tolerated for the
// same reason -- in production it is never nil, because telemetry.Provider
// hands out upstream's no-op MeterProvider when telemetry is disabled, so
// the normal disabled path records into nothing rather than branching.
//
// COMPOSITION ROOT: cmd/server/main.go resolves
// telemetryProvider.MeterProvider() into cfg.MeterProvider (it is what
// /readyz's metric is built from) a few lines above where it constructs
// this pool, and passes it here as
// `ingest.WithMeterProvider(cfg.MeterProvider)`. THE SERVER BINARY
// THEREFORE FEEDS THIS COUNTER TODAY -- loam-gp7m wired it, because
// suppressing the idle claim loop's spans while nothing reached a
// collector would have been a net observability regression rather than a
// fix. cmd/server's TestServer_IngestClaimCounterReachesTheCollector
// drives the real binary against a real OTLP receiver and asserts these
// data points arrive, so an edit that drops that argument fails a test
// rather than silently going dark.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(p *Pool) {
		if mp == nil {
			return
		}
		claims, err := mp.Meter(meterName).Int64Counter(claimCyclesMetric,
			metric.WithDescription("Ingest job claim cycles -- one per Pool.claim call, i.e. per worker wake or poll -- by outcome."),
			metric.WithUnit("{cycle}"),
		)
		if err != nil {
			// A metric that cannot be created must not stop a pool from
			// ingesting, for the same reason internal/health serves on
			// unmetered: that would make observability a dependency of the
			// work rather than a view onto it.
			p.logger.Warn("ingest claim metric unavailable", "metric", claimCyclesMetric, "error", err)
			return
		}
		p.claims = claims
	}
}

// recordClaim increments the claim-cycle counter for one outcome, or does
// nothing when no MeterProvider was supplied.
//
// Callers hold mu (claim does, for its whole duration). That is deliberate
// rather than tolerated: the alternative is recording after the unlock,
// which would let a second worker's outcome interleave between the decision
// and the increment for no benefit, since Add on a no-op counter is free and
// on a real one is a map lookup against a four-value attribute set -- orders
// of magnitude below the database round trips this function is already
// holding the lock across.
func (p *Pool) recordClaim(ctx context.Context, outcome string) {
	if p.claims == nil {
		return
	}
	p.claims.Add(ctx, 1, metric.WithAttributes(attribute.String(claimOutcomeAttribute, outcome)))
}
