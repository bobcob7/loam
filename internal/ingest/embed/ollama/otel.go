package ollama

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// spanNamePrefix namespaces every span produced by a client this package
// instrumented, so embedding time is separable from internal/forge's REST
// calls and internal/db's queries in a trace view. Embedding is called once
// per chunk batch during ingest and is a plausible dominant cost of a whole
// ingest run, which is the specific question these spans exist to answer.
const spanNamePrefix = "embedder "

// InstrumentHTTPClient returns a copy of client whose transport is wrapped
// with otelhttp, so every /api/embed round trip becomes a client span nested
// under whatever span its caller's context already carries. A nil tp returns
// client unchanged.
//
// The returned client is a shallow copy, so the caller's value is not
// mutated and its Timeout and redirect policy are preserved.
//
// # THE TEXT BEING EMBEDDED IS NEVER RECORDED, AND CANNOT BE
//
// The chunk text this client POSTs is repository source code, and the
// response is its embedding. Neither may reach a collector. That holds
// structurally rather than by care: otelhttp instruments at the
// RoundTripper, and its client attribute set (checked against v0.69.0's
// otelhttp/internal/semconv, and pinned by
// TestInstrumentHTTPClient_NeverRecordsRequestBody) is confined to the
// method, the userinfo-stripped url.full, server address/port, protocol and
// status -- it never reads a request or response BODY at all, and offers no
// option that would make it. The request body is a bytes.Reader over the
// marshalled batch and is not touched by the transport.
//
// What DOES need care is the span name, which is why the formatter below is
// a fixed route rather than anything derived from the batch.
//
// Both of otelhttp's global fallbacks are closed here for the same reasons
// internal/forge's equivalent closes them, and that file carries the long
// version. In short: an explicit no-op MeterProvider instead of
// otel.GetMeterProvider(), so HTTP metrics cannot appear as a side effect of
// adding spans; and an explicit EMPTY propagator instead of
// otel.GetTextMapPropagator(), so a future otel.SetTextMapPropagator
// anywhere in the process cannot silently start injecting trace headers into
// requests. The embedder's case for the second is weaker than the forge's --
// Ollama is a local service, not a third party, and does not understand
// traceparent -- but the asymmetry would be the surprising thing, and an
// LOAM_EMBEDDER_URL pointing at a shared or hosted inference endpoint is a
// perfectly ordinary deployment.
func InstrumentHTTPClient(client *http.Client, tp trace.TracerProvider) *http.Client {
	if tp == nil {
		return client
	}
	instrumented := *client
	instrumented.Transport = otelhttp.NewTransport(client.Transport,
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithMeterProvider(metricnoop.NewMeterProvider()),
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator()),
		otelhttp.WithSpanNameFormatter(spanNameForRequest),
	)
	return &instrumented
}

// spanNameForRequest names an embedder request "embedder <METHOD> <path>".
//
// The path is taken as-is because this client has exactly one endpoint --
// New pins it to <baseURL>/api/embed and nothing else in the package builds
// a URL -- so it carries no variable segment to template and no batch
// content to leak. otelhttp's default of "HTTP POST" is what this replaces:
// it would be indistinguishable from every other POST in the trace,
// including internal/forge's CreatePR.
func spanNameForRequest(_ string, r *http.Request) string {
	return spanNamePrefix + r.Method + " " + r.URL.Path
}
