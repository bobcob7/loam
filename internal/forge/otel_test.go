package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
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
