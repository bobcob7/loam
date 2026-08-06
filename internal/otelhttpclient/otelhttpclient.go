// Package otelhttpclient is the single place this repository wraps an
// outbound *http.Client with OpenTelemetry tracing (loam-9v9s).
//
// It exists because there were briefly two copies of this wrapping --
// internal/forge's and internal/ingest/embed/ollama's -- whose bodies were
// identical apart from the span-name formatter. The copies were not merely
// redundant: the guarantees below are enforced by TESTS, and a second copy
// meant a second set of tests that could silently fall out of step. That is
// exactly what happened. internal/forge's WithPropagators option was pinned
// by a test; the identical option in the ollama copy was pinned by nothing,
// and could be deleted with the package staying green. Collapsing the two
// removes the class rather than patching the instance: there is now one
// implementation, and one set of tests that necessarily covers every caller.
//
// Callers supply only what genuinely differs between them -- the span-name
// formatter -- because that is the part that carries per-package meaning.
package otelhttpclient

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Instrument returns a copy of client whose transport is wrapped with
// otelhttp, so every request it makes becomes a client span nested under
// whatever span its caller's context already carries. spanName is passed
// to otelhttp's WithSpanNameFormatter. A nil tp returns client unchanged, so
// a build with telemetry disabled has no extra RoundTripper on the stack.
//
// The returned client is a SHALLOW COPY: the caller's Timeout, CookieJar and
// CheckRedirect are preserved, and the caller's value is not mutated. That
// matters because cmd/server's registerRepoAdminService shares ONE
// *http.Client between the forge resolver and repoadmin.ForgeChecker, so an
// implementation that assigned to client.Transport in place would silently
// instrument, or double-instrument, collaborators that never asked for it.
//
// # WHY THIS CANNOT LEAK A CREDENTIAL OR A REQUEST BODY
//
// The clients wrapped here carry the most sensitive material this process
// handles: internal/forge authenticates with `Authorization: token <pat>` or
// HTTP Basic, and the ollama embedder POSTs repository source code as the
// text to embed. Handing either to a tracing transport is one
// misconfiguration away from writing it into a span, so the claim was
// checked against otelhttp v0.69.0's source rather than its README:
//
//   - The client-side attribute set is CLOSED and enumerated in
//     otelhttp/internal/semconv's HTTPClient.RequestTraceAttrs /
//     ResponseTraceAttrs: http.request.method, http.request.method.original,
//     url.full, server.address, server.port, network.protocol.name,
//     network.protocol.version, http.response.status_code, error.type. No
//     request header is read except Host and User-Agent, no response header
//     is read at all, and no BODY is touched in either direction.
//   - There is no option to capture headers. All eleven of otelhttp's
//     exported Option constructors were checked; none takes a header
//     allow-list. This is unlike otelhttptrace, which does record them.
//   - url.full is userinfo-stripped upstream: RequestTraceAttrs nils
//     req.URL.User before stringifying and restores it afterwards. That is
//     load-bearing rather than reassuring, because internal/forge's
//     receivePackProbeOverGit derives its probe URL from a caller-supplied
//     upstream URL that MAY embed a "https://<token>@host/..." credential.
//
// The first and third are UPSTREAM behaviour, so they are pinned here by
// TestInstrument_NeverRecordsCredentials rather than left to survive a
// dependency bump on trust.
//
// # BOTH OF otelhttp's GLOBAL FALLBACKS ARE CLOSED
//
// otelhttp reaches for a process-wide global in two places when the
// corresponding option is omitted, and internal/telemetry deliberately
// installs neither:
//
//   - WithMeterProvider, or otel.GetMeterProvider(). An explicit no-op keeps
//     HTTP metrics -- a different bead -- from appearing as a side effect of
//     adding spans.
//   - WithPropagators, or otel.GetTextMapPropagator(). An explicit EMPTY
//     composite, which injects nothing.
//
// The second is the easier to miss and the more consequential. otelhttp
// calls propagators.Inject on every outbound request, writing headers into a
// request bound for a THIRD-PARTY forge -- git.example.com, api.github.com.
// Today the global default is a no-op, so nothing is injected and the call
// is inert. But it is a global: a single otel.SetTextMapPropagator anywhere
// in the process, in any future bead or dependency, would silently begin
// leaking this deployment's internal trace and span IDs to an external
// service that is not part of its trace domain, with no diff to this file to
// explain it. Passing it explicitly makes that a decision someone has to
// come here and make.
//
// If loam ever runs against a service inside its own trace domain and wants
// end-to-end traces through it, this is the line to change -- and it should
// change here, or per caller, never by installing a global.
func Instrument(client *http.Client, tp trace.TracerProvider, spanName func(operation string, r *http.Request) string) *http.Client {
	if tp == nil {
		return client
	}
	instrumented := *client
	instrumented.Transport = otelhttp.NewTransport(client.Transport,
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithMeterProvider(metricnoop.NewMeterProvider()),
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator()),
		otelhttp.WithSpanNameFormatter(spanName),
	)
	return &instrumented
}
