//go:build integration

// This file is loam-gp7m's wiring proof. See main_integration_test.go's
// header for the build tag, the podman note, and the harness
// (startServerWithEnv, newPostgres) it reuses rather than duplicating.
package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// claimCyclesMetric and claimOutcomeKey are internal/ingest's own metric and
// attribute names. They are restated here rather than exported from that
// package because this test is asserting the CONTRACT an operator's
// dashboard binds to: if a future edit renames either, this test must fail
// even though a shared constant would have silently followed the rename.
const (
	claimCyclesMetric = "loam.ingest.claim.cycles"
	claimOutcomeKey   = "loam.ingest.claim.outcome"
)

// otlpCollector is a minimal OTLP/HTTP receiver: it accepts the exporter's
// POSTs, decodes the metric ones, and records every data point it saw for a
// named metric. Trace exports land here too (same endpoint, /v1/traces) and
// are answered 200 and ignored, so the exporter never enters its retry loop
// and never muddies the server's stderr.
type otlpCollector struct {
	server *httptest.Server
	mu     sync.Mutex
	points map[string][]int64 // outcome attribute value -> values seen
}

// newOTLPCollector starts the receiver and returns it with cleanup
// registered.
func newOTLPCollector(t *testing.T) *otlpCollector {
	t.Helper()
	c := &otlpCollector{points: map[string][]int64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		c.consume(t, r)
		writeProto(w, &collectormetricspb.ExportMetricsServiceResponse{})
	})
	// Everything else -- /v1/traces above all -- is accepted and dropped.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	c.server = httptest.NewServer(mux)
	t.Cleanup(c.server.Close)
	return c
}

// writeProto answers an OTLP request with a marshalled protobuf body, which
// is what the exporter expects; a bare 200 with an empty body is treated as
// a protocol error and retried.
func writeProto(w http.ResponseWriter, msg proto.Message) {
	body, err := proto.Marshal(msg)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(body)
}

// consume decodes one metrics export and records the claim-cycle data
// points. Gzip is handled because the exporter's compression is an SDK
// default this test does not pin -- reading Content-Encoding rather than
// assuming keeps it correct if that default ever changes.
func (c *otlpCollector) consume(t *testing.T, r *http.Request) {
	t.Helper()
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return
		}
		defer gz.Close()
		body = gz
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return
	}
	var req collectormetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				if m.GetName() != claimCyclesMetric {
					continue
				}
				sum := m.GetSum()
				require.NotNil(t, sum, "%s must be exported as a Sum, not %T", claimCyclesMetric, m.GetData())
				assert.True(t, sum.GetIsMonotonic(), "a claim-cycle count that can go down is not a count")
				for _, dp := range sum.GetDataPoints() {
					for _, kv := range dp.GetAttributes() {
						if kv.GetKey() == claimOutcomeKey {
							c.points[kv.GetValue().GetStringValue()] = append(
								c.points[kv.GetValue().GetStringValue()], dp.GetAsInt())
						}
					}
				}
			}
		}
	}
}

// seen returns the values recorded for one outcome so far.
func (c *otlpCollector) seen(outcome string) []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.points[outcome]...)
}

// outcomes returns every outcome value seen, for failure messages that say
// what DID arrive rather than only what did not.
func (c *otlpCollector) outcomes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var got []string
	for outcome := range c.points {
		got = append(got, outcome)
	}
	return got
}

// TestServer_IngestClaimCounterReachesTheCollector is the evidence that
// loam-gp7m is not a net observability regression.
//
// # WHY THIS TEST EXISTS AT ALL, AND WHY NOT AT A SMALLER SEAM
//
// The bead suppressed the idle claim loop's spans -- ~121,000 root traces a
// day describing the absence of work -- on the promise that a counter
// replaces them. internal/ingest already proves the counter increments with
// the right outcome through a real MeterProvider against a real Postgres
// (TestClaim_IdleQueueIsSilentButWorkAndFailureAreNot). What that CANNOT
// prove is the composition root: that main.go actually hands the pool a
// meter. Miss that one argument and every unit test still passes, the spans
// are still gone, and nothing whatsoever reaches a collector -- which is
// strictly worse for an operator than not having done the work.
//
// So this drives the REAL COMPILED BINARY against a REAL OTLP receiver and
// asserts the metric arrives over the wire, with its dimension intact. A
// test that only checked the argument was present in the source would pass
// against a provider that was nil, no-op, or never exported.
//
// # THE INFLATION LEVER, WHICH IS WHAT MAKES IT FAST
//
// Two default intervals stand between a booted server and an observable
// counter, and both are turned down rather than waited out:
//
//   - OTEL_METRIC_EXPORT_INTERVAL. internal/telemetry builds its
//     PeriodicReader with no explicit interval, so the SDK reads this
//     environment variable and would otherwise export once a MINUTE.
//   - The claim itself needs no lever: every worker calls claim() the
//     instant Run starts, before its first ticker tick, so an idle server
//     has recorded outcome=empty within milliseconds of becoming ready.
//     That is why this asserts on "empty" -- it is the outcome an idle
//     deployment produces, and it is precisely the case whose traces were
//     removed.
func TestServer_IngestClaimCounterReachesTheCollector(t *testing.T) {
	dsn := newPostgres(t)
	collector := newOTLPCollector(t)
	srv := startServerWithEnv(t, dsn,
		"LOAM_OTEL_ENDPOINT="+collector.server.URL,
		// Sample everything, so nothing about this test depends on the
		// conservative default ratio.
		"LOAM_OTEL_SAMPLE_RATIO=1.0",
		"OTEL_METRIC_EXPORT_INTERVAL=250",
	)
	require.Eventually(t, func() bool {
		return len(collector.seen("empty")) > 0
	}, 30*time.Second, 100*time.Millisecond,
		"the ingest claim counter never reached the collector; outcomes seen: %v\nserver stderr: %s",
		collector.outcomes(), srv.stderr.String())
	values := collector.seen("empty")
	assert.GreaterOrEqual(t, values[len(values)-1], int64(1),
		"an idle server's workers each claim once at startup, so the counter must be at least 1")
	assert.NotContains(t, collector.outcomes(), "failed",
		"an idle server against a healthy database must never record a failed claim")
}
