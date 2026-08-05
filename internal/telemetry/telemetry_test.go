package telemetry

import (
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// blackHoleCollector binds a real TCP listener and never accepts on it, so a
// connection completes at the OS level and then hangs forever waiting for an
// HTTP response. This is the shape an unreachable collector actually has in
// production -- a Service whose endpoints have gone away, a collector that
// is up but wedged -- and it is deliberately NOT the same thing as a closed
// port. A closed port fails fast with ECONNREFUSED, which would let a
// shutdown with no bound at all still return promptly and make a
// "returns within its timeout" assertion pass for the wrong reason.
func blackHoleCollector(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	return "http://" + listener.Addr().String()
}

// tracesPath is the OTLP/HTTP signal path the trace exporter POSTs to.
// otlpRecorder MUST filter on it, and the exact shape of what gets through
// without the filter matters, because it decides WHICH assertion is the
// guard.
//
// ExportMetricsServiceRequest.resource_metrics carries the same protobuf
// field number as ExportTraceServiceRequest.resource_spans, and
// ResourceMetrics.resource is likewise field-identical to
// ResourceSpans.resource. So the metric exporter's POST to /v1/metrics does
// not fail to decode and does not decode to something empty: it yields a
// ResourceSpans with a COMPLETE, CORRECT resource -- all seven attributes,
// right schema URL -- and no scope_spans.
//
// The consequence: TestNew_ExportsSpansStampedWithTheExpectedResourceAttributes
// still PASSES, because every attribute it checks is present on the wrong
// payload too. TestNew_SampleRatioActuallyReachesTheSampler also passes,
// since it counts spans inside scope_spans and the impostor contributes
// none. The only assertion that fails is TestShutdown_IsIdempotent's request
// COUNT ("should have 1 item(s), but has 2"). If that count assertion is
// ever relaxed, this filter loses its last guard -- do not assume the
// attribute test covers it.
//
// To reproduce that, remove ONLY the early return and the two response lines
// below, keeping the otherSignals counter. Deleting the whole branch is a
// different experiment and gives a different answer: the counter goes with
// it, so TestNew_SampleRatioActuallyReachesTheSampler's traces-vs-metrics
// assertion fails too and TWO tests go red rather than one. Both measured.
const tracesPath = "/v1/traces"

// otlpRecorder is a minimal OTLP/HTTP collector: it decodes the
// ExportTraceServiceRequest protobuf loam actually puts on the wire, rather
// than merely counting requests, so a test can assert on the resource
// attributes that reached it.
type otlpRecorder struct {
	mu           sync.Mutex
	requests     []*coltracepb.ExportTraceServiceRequest
	otherSignals int
}

func (r *otlpRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != tracesPath {
		r.mu.Lock()
		r.otherSignals++
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(nil)
		return
	}
	body := io.Reader(req.Body)
	if req.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(req.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var decoded coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.requests = append(r.requests, &decoded)
	r.mu.Unlock()
	response, err := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(response)
}

func (r *otlpRecorder) snapshot() []*coltracepb.ExportTraceServiceRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*coltracepb.ExportTraceServiceRequest(nil), r.requests...)
}

// nonTraceRequests counts POSTs to any signal path other than /v1/traces --
// in practice /v1/metrics. It exists so the documented asymmetry between the
// two signals can be asserted rather than assumed.
func (r *otlpRecorder) nonTraceRequests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.otherSignals
}

// settledGoroutines returns runtime.NumGoroutine() once it has stopped
// moving, so a measurement is not taken in the middle of some other
// goroutine's teardown. It gives up and returns the last reading rather than
// failing, because a never-settling runtime is a property of the whole test
// binary, not of the code under test, and the amplification in
// TestNew_EndpointPresenceDecidesWhetherAnySDKMachineryExists makes the
// assertion tolerant of a reading or two of noise.
func settledGoroutines() int {
	last := runtime.NumGoroutine()
	stable := 0
	for range 200 {
		time.Sleep(5 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == last {
			stable++
			if stable == 5 {
				return current
			}
			continue
		}
		last = current
		stable = 0
	}
	return last
}

// TestNew_EndpointPresenceDecidesWhetherAnySDKMachineryExists is this bead's
// inertness proof, and it is written to be a DISCRIMINATOR rather than an
// assertion: every measurement below is taken the same way for both
// configurations, and the two must come out opposite. A test that only
// looked at the disabled side could not tell "no exporter was created" from
// "an exporter was created and simply has not sent anything yet".
//
// Three independent measurements, each of which alone would be weak:
//
//  1. Concrete provider type. tracenoop.TracerProvider and
//     *sdktrace.TracerProvider are different types; no amount of buffering,
//     timing, or luck can make one look like the other. This is the
//     measurement that cannot be fooled.
//  2. Goroutine count, AMPLIFIED. One disabled provider leaking one
//     goroutine is within the noise of a running test binary, so this
//     constructs 64 of them: a single background goroutine per provider
//     would show up as a delta of 64 against a tolerance of 4.
//  3. Behaviour of Shutdown under a dead context, in the sibling test
//     below -- weaker than the other two, and honest about it there.
//
// What it would fail on: adding a batch processor, a periodic reader, an
// exporter, or a "just a metrics-only" provider to the disabled path breaks
// (1) immediately and (2) as soon as any of them starts a goroutine.
//
// Deliberately NOT parallel: runtime.NumGoroutine() is a process-wide
// reading, and Go runs a package's serial tests to completion before any of
// its parallel ones start, so serial is what makes this measurement mean
// anything.
func TestNew_EndpointPresenceDecidesWhetherAnySDKMachineryExists(t *testing.T) {
	const disabledProviderCount = 64
	const goroutineNoiseTolerance = 4
	logger := testLogger()
	baseline := settledGoroutines()
	disabledProviders := make([]*Provider, 0, disabledProviderCount)
	for range disabledProviderCount {
		p, err := New(t.Context(), Config{ServiceName: "loam", SampleRatio: 0.1}, logger)
		require.NoError(t, err)
		disabledProviders = append(disabledProviders, p)
	}
	afterDisabled := settledGoroutines()
	enabled, err := New(t.Context(), Config{
		Endpoint:        blackHoleCollector(t),
		ServiceName:     "loam",
		ServiceVersion:  "v0.0.0-test",
		SampleRatio:     1,
		ShutdownTimeout: 100 * time.Millisecond,
	}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = enabled.Shutdown(context.Background()) })
	afterEnabled := settledGoroutines()

	assert.LessOrEqual(t, afterDisabled-baseline, goroutineNoiseTolerance,
		"%d disabled providers added %d goroutines; a genuinely inert provider starts none, and even one per provider would show up as %d here",
		disabledProviderCount, afterDisabled-baseline, disabledProviderCount)
	assert.GreaterOrEqual(t, afterEnabled-afterDisabled, 2,
		"one ENABLED provider added %d goroutines; if this is 0 the measurement above proves nothing, because it would read the same either way",
		afterEnabled-afterDisabled)

	assert.False(t, disabledProviders[0].Enabled())
	assert.True(t, enabled.Enabled())
	assert.IsType(t, tracenoop.NewTracerProvider(), disabledProviders[0].TracerProvider())
	assert.IsType(t, metricnoop.NewMeterProvider(), disabledProviders[0].MeterProvider())
	assert.IsType(t, &sdktrace.TracerProvider{}, enabled.TracerProvider())
	assert.IsType(t, &sdkmetric.MeterProvider{}, enabled.MeterProvider())
}

// TestShutdown_DisabledDoesNoWorkEvenUnderADeadContext is the third
// inertness measurement, and the one that needs no timing and no goroutine
// counting at all: an already-cancelled context is handed to both
// providers. The disabled one has nothing to flush, so it must return nil.
// The enabled one propagates that dead context into the SDK's own shutdown
// and must therefore report an error. Same input, opposite outcome.
//
// What this one does NOT catch, stated plainly so it is not mistaken for
// more than it is: Shutdown short-circuits on the `enabled` flag, so a
// mutant that built a real SDK provider while still setting enabled=false
// passes here. That mutant was tried, and it is caught -- twice -- by the
// test above, which reads the concrete provider type and the goroutine
// count rather than the flag. This test's job is the narrower one of
// pinning that a disabled Shutdown is unconditionally free.
func TestShutdown_DisabledDoesNoWorkEvenUnderADeadContext(t *testing.T) {
	t.Parallel()
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	disabledProvider, err := New(t.Context(), Config{ServiceName: "loam"}, testLogger())
	require.NoError(t, err)
	assert.NoError(t, disabledProvider.Shutdown(dead))
	enabled, err := New(t.Context(), Config{
		Endpoint:        blackHoleCollector(t),
		ServiceName:     "loam",
		SampleRatio:     1,
		ShutdownTimeout: 100 * time.Millisecond,
	}, testLogger())
	require.NoError(t, err)
	assert.Error(t, enabled.Shutdown(dead),
		"an enabled provider must notice a dead context; if it does not, the disabled provider's nil above proves nothing")
}

// TestShutdown_WithAnUnreachableCollectorReturnsWithinItsOwnBound covers the
// case that actually happens in production. The caller's context is
// deliberately context.Background() -- unbounded -- so the ONLY thing that
// can stop this returning is internal/telemetry's own ShutdownTimeout. If
// the bound lived solely in cmd/server, this test could not exist, and an
// unreachable collector would hold SIGTERM open for the exporter's whole
// retry budget: in Kubernetes that means the pod is SIGKILLed and the
// graceful drain cmd/server/serve.go carefully performs is thrown away.
func TestShutdown_WithAnUnreachableCollectorReturnsWithinItsOwnBound(t *testing.T) {
	t.Parallel()
	const shutdownTimeout = 250 * time.Millisecond
	provider, err := New(t.Context(), Config{
		Endpoint:        blackHoleCollector(t),
		ServiceName:     "loam",
		ServiceVersion:  "v0.0.0-test",
		SampleRatio:     1,
		ShutdownTimeout: shutdownTimeout,
	}, testLogger())
	require.NoError(t, err)
	_, span := provider.TracerProvider().Tracer("telemetry-test").Start(t.Context(), "span-that-can-never-be-exported")
	span.End()
	start := time.Now()
	shutdownErr := provider.Shutdown(context.Background())
	elapsed := time.Since(start)
	// The generous ceiling is the point: it is far above shutdownTimeout and
	// far below both the exporter's 10s retry budget and its default 60s
	// one, so this fails loudly if the bound is removed and does not flake
	// on a loaded CI runner if it is not.
	assert.Less(t, elapsed, 4*time.Second, "shutdown against a black-holed collector took %s", elapsed)
	// It must also actually have TRIED. A shutdown that returned instantly
	// would satisfy the ceiling above while proving nothing about the bound.
	assert.GreaterOrEqual(t, elapsed, shutdownTimeout, "shutdown returned in %s, before its own timeout elapsed", elapsed)
	assert.Error(t, shutdownErr, "a flush that could not reach the collector must be reported, not swallowed")
}

// TestShutdown_IsIdempotent pins the property cmd/server depends on: run()
// defers a Shutdown to cover every startup-failure path, and serve() calls
// the same Shutdown at the one point in the shutdown sequence where the
// ordering is correct. On the path that reaches serve, BOTH fire. Without
// the sync.Once, the second call would reach sdkmetric's already-stopped
// reader and return ErrReaderShutdown, turning a clean shutdown into a
// logged error on every single graceful stop.
func TestShutdown_IsIdempotent(t *testing.T) {
	t.Parallel()
	recorder := &otlpRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	provider, err := New(t.Context(), Config{
		Endpoint:       server.URL,
		ServiceName:    "loam",
		ServiceVersion: "v0.0.0-test",
		SampleRatio:    1,
	}, testLogger())
	require.NoError(t, err)
	_, span := provider.TracerProvider().Tracer("telemetry-test").Start(t.Context(), "span")
	span.End()
	require.NoError(t, provider.Shutdown(context.Background()))
	assert.NoError(t, provider.Shutdown(context.Background()))
	assert.NoError(t, provider.Shutdown(context.Background()))
	assert.Len(t, recorder.snapshot(), 1, "the flush must happen exactly once, not once per Shutdown call")
}

// TestNew_ExportsSpansStampedWithTheExpectedResourceAttributes is the
// enabled-side counterpart to the inertness proof: it decodes the actual
// OTLP protobuf loam puts on the wire and asserts on the resource that
// arrived, rather than on the *Resource value the constructor happened to
// build. Those are different claims -- a provider can be constructed with a
// perfectly good resource and still not attach it -- and only the second one
// is what an operator sees in their backend.
func TestNew_ExportsSpansStampedWithTheExpectedResourceAttributes(t *testing.T) {
	t.Parallel()
	recorder := &otlpRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	provider, err := New(t.Context(), Config{
		Endpoint:       server.URL,
		ServiceName:    "loam-under-test",
		ServiceVersion: "v9.9.9-fixture",
		SampleRatio:    1,
	}, testLogger())
	require.NoError(t, err)
	_, span := provider.TracerProvider().Tracer("telemetry-test").Start(t.Context(), "exported-span")
	span.End()
	require.NoError(t, provider.Shutdown(context.Background()))
	requests := recorder.snapshot()
	require.NotEmpty(t, requests, "no OTLP request reached the collector")
	require.NotEmpty(t, requests[0].GetResourceSpans())
	attrs := map[string]string{}
	for _, attr := range requests[0].GetResourceSpans()[0].GetResource().GetAttributes() {
		attrs[attr.GetKey()] = attr.GetValue().GetStringValue()
	}
	assert.Equal(t, "loam-under-test", attrs[string(semconv.ServiceNameKey)])
	assert.Equal(t, "v9.9.9-fixture", attrs[string(semconv.ServiceVersionKey)])
	assert.NotEmpty(t, attrs[string(semconv.ServiceInstanceIDKey)],
		"service.instance.id is what tells two replicas of the same deployment apart")
	assert.NotEmpty(t, attrs["telemetry.sdk.name"], "resource.Default()'s contribution was dropped by the merge")
}

// TestNew_RejectsAnOutOfRangeSampleRatio keeps the range check at the
// package boundary as well as in internal/config. The two are not redundant:
// config guards the ENV VAR, this guards every caller, and a later bead
// wiring a second telemetry construction (an ingest-side provider, say)
// would otherwise reach TraceIDRatioBased with an unvalidated value.
func TestNew_RejectsAnOutOfRangeSampleRatio(t *testing.T) {
	t.Parallel()
	for _, ratio := range []float64{-0.1, 1.1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		provider, err := New(t.Context(), Config{ServiceName: "loam", SampleRatio: ratio}, testLogger())
		require.ErrorIs(t, err, errSampleRatioRange, "ratio %v", ratio)
		assert.Nil(t, provider)
	}
}

// TestNewResource_UsesTheSameSchemaURLAsTheSDK exists to fail on an SDK
// bump. resource.Merge does not error loudly on a schema mismatch in any way
// a running server would notice: it returns a SCHEMALESS resource together
// with an error, so a bump that moved go.opentelemetry.io/otel/sdk's own
// semconv version past this package's import would ship attributes with no
// schema URL if the error were ever ignored, and fail startup if it were
// not. Either way the fix is the same one-line import change, and this test
// is what names it.
func TestNewResource_UsesTheSameSchemaURLAsTheSDK(t *testing.T) {
	t.Parallel()
	res, err := newResource(t.Context(), "loam", "v0.0.0-test")
	require.NoError(t, err)
	assert.Equal(t, semconv.SchemaURL, res.SchemaURL())
	assert.Equal(t, resource.Default().SchemaURL(), res.SchemaURL(),
		"this package's semconv import has drifted from the one go.opentelemetry.io/otel/sdk/resource uses")
}

// TestNewResource_ServiceNameOverridesTheOTelEnvironmentConvention pins the
// merge ORDER. resource.Default() honours OTEL_SERVICE_NAME, which is what
// lets a deployment add attributes without this repo growing a LOAM_
// variable per attribute -- but LOAM_OTEL_SERVICE_NAME is the documented
// knob, and an operator who sets it and sees something else in their backend
// has a genuinely mysterious problem. Swap the two arguments to
// resource.Merge and this test fails.
func TestNewResource_ServiceNameOverridesTheOTelEnvironmentConvention(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	t.Setenv("OTEL_SERVICE_NAME", "set-by-the-otel-convention")
	res, err := newResource(t.Context(), "set-by-loam", "v0.0.0-test")
	require.NoError(t, err)
	found := ""
	for _, attr := range res.Attributes() {
		if attr.Key == semconv.ServiceNameKey {
			found = attr.Value.AsString()
		}
	}
	assert.Equal(t, "set-by-loam", found)
}

// TestNewResource_EmptyVersionDegradesToUnknown pins that the attribute is
// always present. Omitting it instead would make "this build carried no
// version" indistinguishable, in a backend query, from "this build predates
// the attribute existing".
func TestNewResource_EmptyVersionDegradesToUnknown(t *testing.T) {
	t.Parallel()
	res, err := newResource(t.Context(), "loam", "")
	require.NoError(t, err)
	found := ""
	for _, attr := range res.Attributes() {
		if attr.Key == semconv.ServiceVersionKey {
			found = attr.Value.AsString()
		}
	}
	assert.Equal(t, unknownVersion, found)
}

// TestResolveVersion covers the three shapes debug.ReadBuildInfo actually
// produces for this repository, which is why it takes a *debug.BuildInfo
// rather than calling BuildVersion: reproducing them for real would need
// three differently-built binaries.
func TestResolveVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{
			name: "no build info at all",
			want: unknownVersion,
		},
		{
			name: "a tagged release, which is what `go build` from a git checkout produces",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.0.6-0.20260805165912-72244cfcdf88"}},
			ok:   true,
			want: "v0.0.6-0.20260805165912-72244cfcdf88",
		},
		{
			name: "go test and go run, which stamp (devel) but still carry a vcs revision",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "72244cfcdf88b9a703472c64bceeafdf539fdde7"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			ok:   true,
			want: "72244cfcdf88",
		},
		{
			name: "a dirty working tree is marked as such rather than passed off as the commit",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "72244cfcdf88b9a703472c64bceeafdf539fdde7"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			ok:   true,
			want: "72244cfcdf88+dirty",
		},
		{
			name: "a build from a source tree with no .git -- which is what the shipped container image is, because .dockerignore excludes it",
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			ok:   true,
			want: unknownVersion,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, resolveVersion(tc.info, tc.ok))
		})
	}
}

// TestBuildVersion_IsNeverEmpty is the one assertion that can be made about
// the real build stamp without pinning this test to how the test binary
// happened to be compiled: service.version must always have a value, because
// an empty resource attribute is dropped by some backends and silently
// changes the shape of a query.
func TestBuildVersion_IsNeverEmpty(t *testing.T) {
	t.Parallel()
	assert.NotEmpty(t, BuildVersion())
}

// countExportedSpans runs a provider at the given sample ratio against a
// recording collector, starts rootSpanCount ROOT spans, and returns how many
// actually reached the wire. Root spans specifically: the sampler under test
// is ParentBased, so a span with a sampled parent is kept regardless of the
// ratio and would tell us nothing about the ratio at all.
func countExportedSpans(t *testing.T, ratio float64, rootSpanCount int) (int, *otlpRecorder) {
	t.Helper()
	recorder := &otlpRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	provider, err := New(t.Context(), Config{
		Endpoint:       server.URL,
		ServiceName:    "loam",
		ServiceVersion: "v0.0.0-test",
		SampleRatio:    ratio,
	}, testLogger())
	require.NoError(t, err)
	tracer := provider.TracerProvider().Tracer("telemetry-test")
	for range rootSpanCount {
		_, span := tracer.Start(context.Background(), "root-span")
		span.End()
	}
	require.NoError(t, provider.Shutdown(context.Background()))
	exported := 0
	for _, request := range recorder.snapshot() {
		for _, resourceSpans := range request.GetResourceSpans() {
			for _, scopeSpans := range resourceSpans.GetScopeSpans() {
				exported += len(scopeSpans.GetSpans())
			}
		}
	}
	return exported, recorder
}

// TestNew_SampleRatioActuallyReachesTheSampler closes the hole every other
// enabled-path test in this file leaves open: they all run at ratio 1, where
// TraceIDRatioBased(1) and AlwaysSample() are INDISTINGUISHABLE. Delete the
// WithSampler option entirely -- so cfg.SampleRatio is read, validated,
// logged, and then ignored -- and every one of them stays green.
//
// That is the shape of defect this repository has paid for most often: a
// fixture whose seed value lets two different code paths produce identical
// output, so a thorough-looking battery passes over unverified wiring. It is
// especially acute here because LOAM_OTEL_SAMPLE_RATIO's VALIDATION is the
// most heavily tested code on this branch (nine boundary rows, including the
// NaN guard) while the thing that validation exists to protect was, until
// this test, not exercised at all.
//
// The two endpoints of the range are what make this a discriminator rather
// than an assertion, and both are needed: ratio 0 alone would pass against a
// mutant hardcoding NeverSample, and ratio 1 alone would pass against one
// hardcoding AlwaysSample. Only requiring both to hold pins the value
// through.
func TestNew_SampleRatioActuallyReachesTheSampler(t *testing.T) {
	t.Parallel()
	const rootSpanCount = 50
	atZero, zeroRecorder := countExportedSpans(t, 0, rootSpanCount)
	assert.Equal(t, 0, atZero,
		"at ratio 0 no root span may be sampled; a non-zero count here means the ratio never reached the sampler")
	atOne, _ := countExportedSpans(t, 1, rootSpanCount)
	assert.Equal(t, rootSpanCount, atOne,
		"at ratio 1 every root span must be sampled; a short count here means the sampler is hardcoded to drop")
	// The asymmetry LOAM_OTEL_SAMPLE_RATIO's doc comment in internal/config
	// warns about, asserted rather than assumed: the sampler is a TRACE
	// concept, sdkmetric has no equivalent, so ratio 0 silences traces and
	// leaves the metric exporter pushing on its periodic reader exactly as
	// before. An operator reaching for ratio 0 as an off switch gets half of
	// one.
	assert.NotZero(t, zeroRecorder.nonTraceRequests(),
		"ratio 0 must NOT stop metrics; if it does, internal/config's argument for declining LOAM_OTEL_ENABLED needs rewriting, not just its doc comment")
}

// TestNew_SamplerIsParentBasedSoASampledParentsChildrenSurvive pins the
// other half of the sampler wiring, which the ratio test above cannot see.
// Config.SampleRatio's doc comment claims the ratio is "applied through a
// ParentBased sampler so a sampled parent's children are always kept", and
// that claim is what makes a sampled trace a WHOLE trace rather than a
// randomly-punctured one once loam sits downstream of another instrumented
// service.
//
// Ratio 0 is the only setting where the two candidate wirings are
// GUARANTEED to diverge -- guaranteed being the operative word, and the
// reason the fixture uses 0 rather than anything more realistic.
//
// The two wirings actually disagree over a whole interval, but where that
// interval ENDS depends on the trace ID. TraceIDRatioBased samples when
// (traceID[8:16] >> 1) < uint64(ratio * (1<<63)), so for the trace ID
// hardcoded below -- whose low 8 bytes are 0x090a0b0c0d0e0f10 -- the bare
// sampler starts keeping this span at ratio 0x090a0b0c0d0e0f10 / 2^64 =
// 0.0353, and above that the two wirings agree. Measured directly against
// both samplers: divergence holds up to 0.035300 and the first agreement is
// at 0.035350, matching the arithmetic to five decimals.
//
// Two things follow, and both are why this comment is longer than the test.
// First, the trace-ID constant below is LOAD-BEARING at every ratio except
// 0; it is not an arbitrary filler value to be swapped for convenience.
// Second -- the trap one layer down -- at the DEFAULT ratio of 0.1 both
// wirings keep this span, so the identical test written at the default would
// pass against both and catch nothing. Do not "simplify" this fixture to use
// the default. Ratio 0 is the one setting where the trace ID cannot matter:
// the upper bound is 0, so the bare sampler drops everything, and only the
// ParentBased wrapper can keep a span whose remote parent is flagged
// sampled.
//
// Drop the ParentBased wrapper and this test fails while every other test in
// the file, including the ratio one above, stays green.
func TestNew_SamplerIsParentBasedSoASampledParentsChildrenSurvive(t *testing.T) {
	t.Parallel()
	recorder := &otlpRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	provider, err := New(t.Context(), Config{
		Endpoint:       server.URL,
		ServiceName:    "loam",
		ServiceVersion: "v0.0.0-test",
		SampleRatio:    0,
	}, testLogger())
	require.NoError(t, err)
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), parent)
	_, span := provider.TracerProvider().Tracer("telemetry-test").Start(ctx, "child-of-a-sampled-remote-parent")
	span.End()
	require.NoError(t, provider.Shutdown(context.Background()))
	exported := 0
	for _, request := range recorder.snapshot() {
		for _, resourceSpans := range request.GetResourceSpans() {
			for _, scopeSpans := range resourceSpans.GetScopeSpans() {
				exported += len(scopeSpans.GetSpans())
			}
		}
	}
	assert.Equal(t, 1, exported,
		"a child of a sampled remote parent must be kept even at ratio 0, or loam punctures traces that arrive already sampled")
}
