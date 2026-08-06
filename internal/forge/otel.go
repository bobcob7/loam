package forge

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/bobcob7/loam/internal/otelhttpclient"
)

// spanNamePrefix namespaces every span produced by a client this package
// instrumented, so a trace view separates forge REST traffic from
// internal/ingest/embed/ollama's outbound HTTP and from internal/db's
// queries at a glance.
const spanNamePrefix = "forge "

// InstrumentHTTPClient wraps client's transport so every request the
// Provider implementations in this package make (forgejo.go, github.go, and
// the git-protocol probes in forgejo_git.go) becomes a client span. A nil tp
// returns client unchanged.
//
// The wrapping itself, and the reasoning about why it cannot leak the forge
// token this package's every method carries, live in
// internal/otelhttpclient -- deliberately in ONE place, so the tests that
// enforce those guarantees cannot fall out of step with a second copy. What
// stays here is the only part that is genuinely forge-specific: the span
// name.
func InstrumentHTTPClient(client *http.Client, tp trace.TracerProvider) *http.Client {
	return otelhttpclient.Instrument(client, tp, spanNameForRequest)
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
