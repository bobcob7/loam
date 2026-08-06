package ollama

import (
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/bobcob7/loam/internal/otelhttpclient"
)

// spanNamePrefix namespaces every span produced by a client this package
// instrumented, so embedding time is separable from internal/forge's REST
// calls and internal/db's queries in a trace view. Embedding is called once
// per chunk batch during ingest and is a plausible dominant cost of a whole
// ingest run, which is the specific question these spans exist to answer.
const spanNamePrefix = "embedder "

// InstrumentHTTPClient wraps client's transport so every /api/embed round
// trip becomes a client span. A nil tp returns client unchanged.
//
// The wrapping lives in internal/otelhttpclient, which also carries the
// proof that the TEXT BEING EMBEDDED cannot reach a span: otelhttp
// instruments at the RoundTripper and never reads a request or response
// body at all, and offers no option that would make it. That reasoning is
// centralised on purpose -- an earlier version of this file was a
// copy-paste of internal/forge's, and its WithPropagators option was pinned
// by no test in this package while the identical option next door was.
//
// What remains here is the part that is genuinely this package's: the span
// name, which is the one place batch content could leak into a span if it
// were derived from anything other than the URL.
func InstrumentHTTPClient(client *http.Client, tp trace.TracerProvider) *http.Client {
	return otelhttpclient.Instrument(client, tp, spanNameForRequest)
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
