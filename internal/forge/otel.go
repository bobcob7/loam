package forge

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
)

// spanNamePrefix namespaces every span produced by a client this package
// instrumented, so a trace view separates forge REST traffic from
// internal/ingest/embed/ollama's outbound HTTP and from internal/db's
// queries at a glance.
const spanNamePrefix = "forge "

// InstrumentHTTPClient returns a copy of client whose transport is wrapped
// with otelhttp, so every request the Provider implementations in this
// package make (forgejo.go, github.go, and the git-protocol probes in
// forgejo_git.go) becomes a client span nested under whatever span its
// caller's context already carries. A nil tp returns client unchanged.
//
// The returned client is a SHALLOW COPY: client's own Timeout, CookieJar and
// CheckRedirect are preserved, and the caller's value is not mutated, so one
// *http.Client can be instrumented for this package without silently
// changing behaviour for anything else that happens to hold it.
//
// # WHY THIS CANNOT LEAK THE FORGE TOKEN, ESTABLISHED RATHER THAN ASSUMED
//
// Every Provider method here authenticates, either with `Authorization:
// token <pat>` (forgejo.go, github.go) or HTTP Basic over the git smart-HTTP
// probe (forgejo_git.go). Handing that request to a tracing transport is one
// misconfiguration away from writing a live credential into a span, so the
// claim was checked against otelhttp v0.69.0's source rather than its README:
//
//   - The client-side attribute set is closed and enumerated in
//     otelhttp/internal/semconv's HTTPClient.RequestTraceAttrs /
//     ResponseTraceAttrs: http.request.method, http.request.method.original,
//     url.full, server.address, server.port, network.protocol.name,
//     network.protocol.version, http.response.status_code, error.type. No
//     request header is read except Host and User-Agent, and no response
//     header is read at all.
//   - There is no option to capture headers. All eleven of otelhttp's
//     exported Option constructors were checked; none takes a header
//     allow-list, and the only header the transport touches is the
//     traceparent it WRITES via propagators.Inject. This is unlike
//     otelhttptrace, which does record them -- hence the explicit option
//     list below rather than a bare NewTransport(nil).
//   - url.full is userinfo-stripped upstream: RequestTraceAttrs nils
//     req.URL.User before stringifying and restores it afterwards. That is
//     load-bearing here and not merely reassuring, because
//     receivePackProbeOverGit derives its probe URL from a caller-supplied
//     upstream URL that MAY embed a "https://<token>@host/..." credential --
//     the exact form redactUserinfo exists for.
//
// The first and third of those are upstream behaviour, so they are pinned
// here by TestInstrumentHTTPClient_NeverRecordsCredentials rather than left
// to survive a dependency bump on trust.
//
// A no-op MeterProvider is passed deliberately. otelhttp otherwise falls
// back to otel.GetMeterProvider(), a process-wide global that
// internal/telemetry pointedly never installs; naming the no-op keeps this
// package's behaviour independent of whether some other package ever does,
// and keeps HTTP metrics -- which are a different bead -- from appearing as
// a side effect of adding spans.
func InstrumentHTTPClient(client *http.Client, tp trace.TracerProvider) *http.Client {
	if tp == nil {
		return client
	}
	instrumented := *client
	instrumented.Transport = otelhttp.NewTransport(client.Transport,
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithMeterProvider(metricnoop.NewMeterProvider()),
		otelhttp.WithSpanNameFormatter(spanNameForRequest),
	)
	return &instrumented
}

// spanNameForRequest names a forge request "forge <METHOD> <route>", where
// route is the templated path (see templatePath).
//
// otelhttp's own default is "HTTP " + method, which collapses all seven
// Provider methods into two names and answers none of the questions this
// instrumentation exists for -- "why is AcceptProposal slow" needs to
// distinguish opening a PR from polling its state.
func spanNameForRequest(_ string, r *http.Request) string {
	return spanNamePrefix + r.Method + " " + templatePath(r.URL.Path)
}

// templatePath replaces the variable segments of a forge API path with
// placeholders, so the span name stays bounded no matter how many repos are
// enrolled. A repo owner, a repo name and a PR number are all unbounded in
// practice; a metrics or trace backend that groups by span name would
// otherwise get one series per repo.
//
// The rules cover every path this package constructs today:
//
//	/api/v1/repos/<owner>/<repo>/pulls        (Forgejo: validate probe, create, list)
//	/api/v1/repos/<owner>/<repo>/pulls/<n>    (Forgejo: get state, close)
//	/repos/<owner>/<repo>/pulls[/<n>]         (GitHub, under /api/v3 or api.github.com)
//	/user                                     (GitHub: token validation)
//	<any>/info/refs                           (the git smart-HTTP receive-pack probe)
//
// A path matching none of them passes through unchanged rather than being
// collapsed to a constant: that keeps a newly added endpoint readable, and
// makes the missing rule obvious to whoever adds it. Every endpoint that
// exists is enumerated in TestTemplatePath_CoversEveryEndpointThisPackageBuilds,
// which is the thing to extend alongside a new call.
func templatePath(path string) string {
	if strings.HasSuffix(path, "/info/refs") {
		// The git probe's path is the upstream repo's own, whose shape is
		// the forge's business and not this package's -- subgroups make it
		// arbitrarily deep. Only the suffix is ours to name.
		return "/{owner}/{repo}/info/refs"
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == "repos" && i+2 < len(segments) {
			segments[i+1] = "{owner}"
			segments[i+2] = "{repo}"
		}
	}
	for i, segment := range segments {
		if isAllDigits(segment) {
			segments[i] = "{number}"
		}
	}
	return strings.Join(segments, "/")
}

// isAllDigits reports whether s is a non-empty run of ASCII digits. It is
// deliberately not strconv.Atoi: a segment of "007" or one longer than an
// int is still a path variable, and Atoi would let both through.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
