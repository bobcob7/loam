package forge

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// recordingProvider returns a TracerProvider that keeps every ended span in
// memory, plus the recorder to read them back from.
//
// AlwaysSample is correct here and is not the mistake loam-p56y made:
// sampling is internal/telemetry's property and is tested there. What would
// repeat that mistake is a fixture that cannot tell the behaviour under test
// from a constant, which is why the span-name tables below vary the method,
// the path shape AND the endpoint rather than exercising one request five
// times.
func recordingProvider(t *testing.T) (trace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	return tp, recorder
}

// attributeStrings flattens every attribute on span into "key=value" form,
// following slice-valued attributes into their elements, so a leak test can
// scan the whole surface rather than the string-typed part of it.
func attributeStrings(span sdktrace.ReadOnlySpan) []string {
	var out []string
	for _, kv := range span.Attributes() {
		out = append(out, string(kv.Key)+"="+kv.Value.Emit())
		if kv.Value.Type() == attribute.STRINGSLICE {
			for _, s := range kv.Value.AsStringSlice() {
				out = append(out, string(kv.Key)+"="+s)
			}
		}
	}
	return out
}

// TestTemplatePath_CoversEveryEndpointThisPackageBuilds enumerates every URL
// path the Provider implementations in this package construct, and pins the
// templated form each collapses to.
//
// The table is the inventory: a new forge call that adds a path shape should
// add a row here, and the ones with variable segments are what prove the
// template is doing work. The final assertion is the one that would catch a
// formatter reduced to a constant -- distinct endpoints must produce
// distinct names, which a `return "forge request"` implementation cannot
// satisfy no matter how many rows the table grows.
func TestTemplatePath_CoversEveryEndpointThisPackageBuilds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "forgejo validate-token probe", path: "/api/v1/repos/" + probeOwner + "/" + probeRepo + "/pulls", want: "/api/v1/repos/{owner}/{repo}/pulls"},
		{name: "forgejo create PR", path: "/api/v1/repos/acme/widgets/pulls", want: "/api/v1/repos/{owner}/{repo}/pulls"},
		{name: "forgejo PR by number", path: "/api/v1/repos/acme/widgets/pulls/4711", want: "/api/v1/repos/{owner}/{repo}/pulls/{number}"},
		{name: "github validate token", path: "/user", want: "/user"},
		{name: "github enterprise create PR", path: "/api/v3/repos/acme/widgets/pulls", want: "/api/v3/repos/{owner}/{repo}/pulls"},
		{name: "github PR by number", path: "/repos/acme/widgets/pulls/9", want: "/repos/{owner}/{repo}/pulls/{number}"},
		{name: "receive-pack probe", path: "/acme/widgets.git/info/refs", want: "/{owner}/{repo}/info/refs"},
		{name: "receive-pack probe under a subgroup", path: "/acme/team/nested/widgets/info/refs", want: "/{owner}/{repo}/info/refs"},
		{name: "zero-padded PR number is still a variable", path: "/api/v1/repos/acme/widgets/pulls/007", want: "/api/v1/repos/{owner}/{repo}/pulls/{number}"},
		{name: "PR number wider than an int is still a variable", path: "/api/v1/repos/acme/widgets/pulls/99999999999999999999999", want: "/api/v1/repos/{owner}/{repo}/pulls/{number}"},
	}
	seen := map[string]string{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, templatePath(tt.path))
		})
		seen[tt.want] = tt.name
	}
	assert.GreaterOrEqual(t, len(seen), 6, "distinct endpoints must map to distinct templates; a formatter that returned a constant would collapse this to 1")
}

// TestTemplatePath_DoesNotTemplateWhatIsNotAVariable is the other half: a
// too-eager rule that replaced any segment would make every span name
// identical and useless. "repos" itself, the API version, and the literal
// "pulls" all have to survive.
func TestTemplatePath_DoesNotTemplateWhatIsNotAVariable(t *testing.T) {
	t.Parallel()
	got := templatePath("/api/v1/repos/acme/widgets/pulls/12")
	assert.Contains(t, got, "/api/v1/")
	assert.Contains(t, got, "/repos/")
	assert.Contains(t, got, "/pulls/")
	assert.NotContains(t, got, "acme")
	assert.NotContains(t, got, "widgets")
	assert.NotContains(t, got, "12")
}

// TestSpanNameForRequest_NamesTheOperationNotJustTheMethod pins the reason
// WithSpanNameFormatter is passed at all. otelhttp's default is "HTTP " +
// method, under which CreatePR and GetPRState are the same span, and this
// instrumentation answers nothing.
func TestSpanNameForRequest_NamesTheOperationNotJustTheMethod(t *testing.T) {
	t.Parallel()
	create := requestForTest(t, http.MethodPost, "https://git.example.com/api/v1/repos/acme/widgets/pulls")
	get := requestForTest(t, http.MethodGet, "https://git.example.com/api/v1/repos/acme/widgets/pulls/7")
	createName := spanNameForRequest("", create)
	getName := spanNameForRequest("", get)
	assert.Equal(t, "forge POST /api/v1/repos/{owner}/{repo}/pulls", createName)
	assert.Equal(t, "forge GET /api/v1/repos/{owner}/{repo}/pulls/{number}", getName)
	assert.NotEqual(t, "HTTP POST", createName, "the otelhttp default must have been replaced")
	assert.NotEqual(t, createName, getName)
}

func requestForTest(t *testing.T, method, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, nil)
	require.NoError(t, err)
	return req
}

// TestInstrumentHTTPClient_NeverRecordsCredentials is the assertion with the
// longest useful life in this file, and it is written to FAIL if anyone
// later adds header capture, switches to otelhttptrace, drops
// WithSpanNameFormatter for something URL-derived, or bumps otelhttp to a
// version that stops stripping userinfo from url.full.
//
// It does not inspect the attribute list for a known-bad KEY -- that would
// only catch the leak someone thought to name. It plants a sentinel in every
// place a forge credential actually travels in this package (the
// `Authorization: token <pat>` header used by forgejo.go and github.go, the
// Basic credential the receive-pack probe sets, and the userinfo form
// "https://<token>@host/..." that redactUserinfo exists to defend against)
// and then asserts the sentinel appears NOWHERE on the span: not in the
// name, not in any attribute value, not in any event.
func TestInstrumentHTTPClient_NeverRecordsCredentials(t *testing.T) {
	t.Parallel()
	const sentinel = "gto-l0am-s3cret-forge-token-DO-NOT-RECORD"
	// SetBasicAuth base64s the credential, so the raw sentinel is not what
	// would appear on a leaking span for that row -- this encoded form is.
	// Scanning for both is what stops the Basic-auth case from passing
	// vacuously, and it generalises: any future encoding of the credential
	// needs its own entry here or this test silently stops covering it.
	basicEncoded := base64.StdEncoding.EncodeToString([]byte(gitUsername + ":" + sentinel))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Proves the credential really was on the wire, so a span that
		// omits it is omitting something that was genuinely available to
		// record rather than something that was never there.
		auth := r.Header.Get("Authorization")
		require.NotEmpty(t, auth)
		assert.True(t, strings.Contains(auth, sentinel) || strings.Contains(auth, basicEncoded),
			"the credential must genuinely be on the wire, in one form or the other, or a span that omits it proves nothing")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"html_url":"http://example.invalid/pulls/1","number":1,"state":"open"}`))
	}))
	// t.Cleanup, not defer: the subtests below are parallel, so a deferred
	// Close would shut the server down while they are still dialling it.
	t.Cleanup(server.Close)
	tests := []struct {
		name    string
		prepare func(t *testing.T, req *http.Request)
	}{
		{
			name: "bearer-style token header, as forgejo.go and github.go send it",
			prepare: func(_ *testing.T, req *http.Request) {
				req.Header.Set("Authorization", "token "+sentinel)
			},
		},
		{
			name: "basic auth, as the receive-pack probe sets it",
			prepare: func(_ *testing.T, req *http.Request) {
				req.SetBasicAuth(gitUsername, sentinel)
			},
		},
		{
			name: "credential embedded in the URL userinfo, the redactUserinfo case",
			prepare: func(t *testing.T, req *http.Request) {
				req.URL.User = url.User(sentinel)
				// net/http would derive the header from userinfo anyway;
				// setting it explicitly keeps the handler's assertion
				// meaningful for this row too.
				req.Header.Set("Authorization", "token "+sentinel)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tp, recorder := recordingProvider(t)
			client := InstrumentHTTPClient(server.Client(), tp)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/repos/acme/widgets/pulls", nil)
			require.NoError(t, err)
			tt.prepare(t, req)
			resp, err := client.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			spans := recorder.Ended()
			require.Len(t, spans, 1, "the instrumented transport must have produced exactly one client span")
			span := spans[0]
			attrs := attributeStrings(span)
			require.NotEmpty(t, attrs, "a span with no attributes at all would pass this test vacuously")
			for _, secret := range []string{sentinel, basicEncoded} {
				assert.NotContains(t, span.Name(), secret, "the span NAME must not carry the credential")
				for _, attr := range attrs {
					assert.NotContains(t, attr, secret, "no span attribute may carry the credential")
				}
				for _, event := range span.Events() {
					assert.NotContains(t, event.Name, secret)
					for _, kv := range event.Attributes {
						assert.NotContains(t, kv.Value.Emit(), secret, "no span event attribute may carry the credential")
					}
				}
			}
			assert.NotContains(t, urlFullOf(t, span), "@", "url.full must be userinfo-stripped")
		})
	}
}

// urlFullOf returns the span's url.full attribute, failing the test if it is
// absent -- its absence would make the "@" assertion above vacuous.
func urlFullOf(t *testing.T, span sdktrace.ReadOnlySpan) string {
	t.Helper()
	for _, kv := range span.Attributes() {
		if kv.Key == "url.full" {
			return kv.Value.AsString()
		}
	}
	t.Fatal("span carries no url.full attribute; the leak assertions that read it would be vacuous")
	return ""
}

// TestInstrumentHTTPClient_NestsUnderTheCallerSpan proves the transport
// joins the caller's trace rather than starting an orphan root. Without
// this, a forge call would show up as an unrelated trace and could never be
// attributed to the RPC that made it -- which is the entire point of
// instrumenting the client rather than timing it.
func TestInstrumentHTTPClient_NestsUnderTheCallerSpan(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"html_url":"http://example.invalid/pulls/1","number":1,"state":"open"}`))
	}))
	t.Cleanup(server.Close)
	tp, recorder := recordingProvider(t)
	f := NewForgejo(server.URL, "tok", InstrumentHTTPClient(server.Client(), tp), testLogger())
	ctx, parent := tp.Tracer("test").Start(t.Context(), "rpc AcceptProposal")
	_, _, err := f.CreatePR(ctx, "acme/widgets", "head", "main", "title", "body")
	require.NoError(t, err)
	parent.End()
	spans := recorder.Ended()
	require.Len(t, spans, 2)
	var child sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() != "rpc AcceptProposal" {
			child = s
		}
	}
	require.NotNil(t, child)
	assert.Equal(t, "forge POST /api/v1/repos/{owner}/{repo}/pulls", child.Name(), "the span must be named for the real CreatePR call path, through the real provider")
	assert.Equal(t, parent.SpanContext().SpanID(), child.Parent().SpanID(), "the forge span must be a child of the caller's span")
	assert.Equal(t, parent.SpanContext().TraceID(), child.SpanContext().TraceID())
	assert.Equal(t, trace.SpanKindClient, child.SpanKind())
}

// TestInstrumentHTTPClient_DistinctProviderCallsGetDistinctSpans drives the
// real Provider methods rather than synthetic requests, so the span names
// are proven against the URLs forgejo.go actually builds -- not against a
// test author's recollection of them. If forgejo.go's paths change, this
// fails; a table of hand-written paths would not.
func TestInstrumentHTTPClient_DistinctProviderCallsGetDistinctSpans(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"open","merged":false}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"html_url":"http://example.invalid/pulls/1","number":1,"state":"open"}`))
	}))
	t.Cleanup(server.Close)
	tp, recorder := recordingProvider(t)
	f := NewForgejo(server.URL, "tok", InstrumentHTTPClient(server.Client(), tp), testLogger())
	_, _, err := f.CreatePR(t.Context(), "acme/widgets", "head", "main", "title", "body")
	require.NoError(t, err)
	_, err = f.GetPRState(t.Context(), "acme/widgets", 7)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, s := range recorder.Ended() {
		names[s.Name()] = true
	}
	assert.Len(t, names, 2, "CreatePR and GetPRState must not share a span name; otelhttp's default formatter would give them both 'HTTP POST'/'HTTP GET' and the list-vs-fetch distinction would be lost")
	assert.Contains(t, names, "forge POST /api/v1/repos/{owner}/{repo}/pulls")
	assert.Contains(t, names, "forge GET /api/v1/repos/{owner}/{repo}/pulls/{number}")
}

// TestInstrumentHTTPClient_NeverInjectsTraceHeadersEvenWithAGlobalInstalled
// is the only assertion that can make the WithPropagators option
// non-decorative, and it is the reason this one test does NOT call
// t.Parallel().
//
// otelhttp calls propagators.Inject on every outbound request, falling back
// to the process-wide otel.GetTextMapPropagator() when the option is
// omitted. That global's DEFAULT is a no-op, so today no header is injected
// either way -- meaning a test that merely asserts "no traceparent reaches
// the server" would pass identically against a client that never passed the
// option at all. The only way to tell the two apart is to install the
// global this option exists to defend against, which is exactly what a
// future bead or dependency might do.
//
// Mutating a process-wide global is why this test is sequential: Go pauses
// every t.Parallel() test in the package until the sequential ones finish,
// so the window in which the global is set cannot overlap them, and the
// original is restored on cleanup regardless of outcome. Do not add
// t.Parallel() here.
func TestInstrumentHTTPClient_NeverInjectsTraceHeadersEvenWithAGlobalInstalled(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	original := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(original) })
	otel.SetTextMapPropagator(propagation.TraceContext{})
	// Sanity check the fixture itself: with the global installed, a
	// transport that did NOT pass WithPropagators injects a traceparent. If
	// this stops holding, the assertion below becomes vacuous and this test
	// stops protecting anything.
	tp, _ := recordingProvider(t)
	unguarded := &http.Client{Transport: otelhttp.NewTransport(server.Client().Transport, otelhttp.WithTracerProvider(tp))}
	ctx, span := tp.Tracer("test").Start(t.Context(), "parent")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := unguarded.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.NotEmpty(t, gotHeaders.Get("Traceparent"),
		"fixture check: with a global propagator installed, an unguarded otelhttp transport must inject traceparent")
	// The real assertion: this package's client does not, because it passes
	// an explicit empty propagator rather than inheriting the global.
	gotHeaders = nil
	guarded := InstrumentHTTPClient(server.Client(), tp)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err = guarded.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	span.End()
	require.NotNil(t, gotHeaders)
	assert.Empty(t, gotHeaders.Get("Traceparent"),
		"a forge request must carry no trace header: a global propagator installed elsewhere in the process must not leak this deployment's trace IDs to a third-party forge")
	assert.Empty(t, gotHeaders.Get("Tracestate"))
}

// TestInstrumentHTTPClient_NilProviderIsAPassthrough covers the composition
// root's disabled path cheaply, and pins that the caller's own client is
// returned rather than a wrapped one with a no-op inside -- so a build with
// no tracer provider has no extra RoundTripper on the stack at all.
func TestInstrumentHTTPClient_NilProviderIsAPassthrough(t *testing.T) {
	t.Parallel()
	client := &http.Client{}
	assert.Same(t, client, InstrumentHTTPClient(client, nil))
}

// TestInstrumentHTTPClient_DoesNotMutateTheCaller pins the shallow-copy
// contract. cmd/server passes a fresh client today, but internal/gittransport
// and repoadmin.ForgeChecker share ONE *http.Client with the resolver
// (registerRepoAdminService), so a version of this that assigned to
// client.Transport in place would silently instrument, or double-instrument,
// collaborators that never asked for it.
func TestInstrumentHTTPClient_DoesNotMutateTheCaller(t *testing.T) {
	t.Parallel()
	tp, _ := recordingProvider(t)
	base := &http.Client{Timeout: 7}
	instrumented := InstrumentHTTPClient(base, tp)
	assert.Nil(t, base.Transport, "the caller's client must be left untouched")
	assert.NotNil(t, instrumented.Transport)
	assert.NotSame(t, base, instrumented)
	assert.Equal(t, base.Timeout, instrumented.Timeout, "the copy must preserve the caller's timeout")
}
