package otelhttpclient

import (
	"context"
	"encoding/base64"
	"io"
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

// staticSpanName is the formatter used where the name is not what is under
// test. Each caller's own package tests its real formatter.
func staticSpanName(_ string, _ *http.Request) string { return "test span" }

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
// following slice-valued attributes into their elements, so a leak scan
// covers the whole surface rather than the string-typed part of it.
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

// TestInstrument_NeverRecordsCredentials is the assertion with the longest
// useful life in this package, and it now covers EVERY instrumented client
// in the tree rather than one package's copy of the wrapping.
//
// It is written to fail if anyone adds header capture, switches to
// otelhttptrace, replaces the formatter with something URL-derived, or bumps
// otelhttp to a version that stops stripping userinfo from url.full. It does
// not inspect the attribute list for a known-bad KEY -- that would only
// catch the leak someone thought to name. It plants a sentinel in every form
// a credential actually travels in this tree and asserts it appears NOWHERE
// on the span: not in the name, not in any attribute, not in any event.
func TestInstrument_NeverRecordsCredentials(t *testing.T) {
	t.Parallel()
	const sentinel = "gto-l0am-s3cret-forge-token-DO-NOT-RECORD"
	// SetBasicAuth base64s the credential, so the raw sentinel is not what
	// would appear on a leaking span for that row -- this encoded form is.
	// Scanning for both is what stops the Basic-auth case from passing
	// vacuously, and it generalises: any future encoding of the credential
	// needs its own entry here or this test silently stops covering it.
	basicEncoded := base64.StdEncoding.EncodeToString([]byte("loam:" + sentinel))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		require.NotEmpty(t, auth)
		assert.True(t, strings.Contains(auth, sentinel) || strings.Contains(auth, basicEncoded),
			"the credential must genuinely be on the wire, in one form or the other, or a span that omits it proves nothing")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	tests := []struct {
		name    string
		prepare func(req *http.Request)
	}{
		{
			name:    "token header, as internal/forge's forgejo.go and github.go send it",
			prepare: func(req *http.Request) { req.Header.Set("Authorization", "token "+sentinel) },
		},
		{
			name:    "basic auth, as internal/forge's receive-pack probe sets it",
			prepare: func(req *http.Request) { req.SetBasicAuth("loam", sentinel) },
		},
		{
			name: "credential embedded in the URL userinfo",
			prepare: func(req *http.Request) {
				req.URL.User = url.User(sentinel)
				req.Header.Set("Authorization", "token "+sentinel)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tp, recorder := recordingProvider(t)
			client := Instrument(server.Client(), tp, staticSpanName)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/repos/acme/widgets/pulls", nil)
			require.NoError(t, err)
			tt.prepare(req)
			resp, err := client.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			spans := recorder.Ended()
			require.Len(t, spans, 1)
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

// TestInstrument_NeverRecordsRequestBody guards the ollama embedder's payload
// -- repository source code sent as the text to embed, and its embedding
// vector coming back. Neither may reach a collector.
//
// It lives here rather than in the embedder because it is a property of the
// WRAPPING, not of that package: otelhttp instruments at the RoundTripper
// and never reads a body in either direction.
func TestInstrument_NeverRecordsRequestBody(t *testing.T) {
	t.Parallel()
	const sentinel = "func withdrawEverything(acct Account) // l0am-body-sentinel"
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		seen = body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	tp, recorder := recordingProvider(t)
	client := Instrument(server.Client(), tp, staticSpanName)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/embed", strings.NewReader(sentinel))
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Contains(t, string(seen), "l0am-body-sentinel", "the body must genuinely have been on the wire")
	spans := recorder.Ended()
	require.Len(t, spans, 1)
	require.NotEmpty(t, spans[0].Attributes())
	for _, kv := range spans[0].Attributes() {
		assert.NotContains(t, kv.Value.Emit(), "l0am-body-sentinel", "no span attribute may carry the request body")
	}
	for _, event := range spans[0].Events() {
		for _, kv := range event.Attributes {
			assert.NotContains(t, kv.Value.Emit(), "l0am-body-sentinel")
		}
	}
}

// TestInstrument_NeverInjectsTraceHeadersEvenWithAGlobalInstalled is the only
// assertion that can make the WithPropagators option non-decorative, and it
// is the reason this one test does NOT call t.Parallel().
//
// otelhttp calls propagators.Inject on every outbound request, falling back
// to the process-wide otel.GetTextMapPropagator() when the option is
// omitted. That global's DEFAULT is a no-op, so today no header is injected
// either way -- meaning a test that merely asserts "no traceparent reaches
// the server" would pass identically against a client that never passed the
// option at all. The only way to tell the two apart is to install the global
// this option exists to defend against, which is exactly what a future bead
// or dependency might do.
//
// This test's placement is the point of the whole package: it previously
// existed only in internal/forge, so the IDENTICAL option in the embedder's
// copy of the wrapping was pinned by nothing and could be deleted with that
// package staying green. Here it necessarily covers every caller.
//
// Mutating a process-wide global is why this test is sequential: Go pauses
// every t.Parallel() test in the package until the sequential ones finish,
// so the window in which the global is set cannot overlap them, and the
// original is restored on cleanup regardless of outcome. Do not add
// t.Parallel() here.
func TestInstrument_NeverInjectsTraceHeadersEvenWithAGlobalInstalled(t *testing.T) {
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
	guarded := Instrument(server.Client(), tp, staticSpanName)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err = guarded.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	span.End()
	require.NotNil(t, gotHeaders)
	assert.Empty(t, gotHeaders.Get("Traceparent"),
		"an outbound request must carry no trace header: a global propagator installed elsewhere in the process must not leak this deployment's trace IDs to a third-party service")
	assert.Empty(t, gotHeaders.Get("Tracestate"))
}

// TestInstrument_UsesTheSuppliedSpanNameFormatter pins the one parameter
// callers vary. Without it, a refactor that dropped WithSpanNameFormatter
// would leave every span named "HTTP GET" and be caught only by the two
// callers' own tests -- which is the arrangement this package exists to stop
// relying on.
func TestInstrument_UsesTheSuppliedSpanNameFormatter(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	tp, recorder := recordingProvider(t)
	client := Instrument(server.Client(), tp, func(_ string, r *http.Request) string {
		return "sentinel-formatter " + r.Method
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "sentinel-formatter GET", spans[0].Name())
	assert.NotEqual(t, "HTTP GET", spans[0].Name(), "otelhttp's default formatter must have been replaced")
}

// TestInstrument_NilProviderIsAPassthrough covers the composition root's
// telemetry-disabled path, and pins that the caller's own client is returned
// rather than a wrapper with a no-op inside -- so a build with no tracer
// provider has no extra RoundTripper on the stack at all.
func TestInstrument_NilProviderIsAPassthrough(t *testing.T) {
	t.Parallel()
	client := &http.Client{}
	assert.Same(t, client, Instrument(client, nil, staticSpanName))
}

// TestInstrument_DoesNotMutateTheCaller pins the shallow-copy contract. See
// Instrument's doc comment for the shared-client case in cmd/server that
// makes it load-bearing rather than merely tidy.
func TestInstrument_DoesNotMutateTheCaller(t *testing.T) {
	t.Parallel()
	tp, _ := recordingProvider(t)
	base := &http.Client{Timeout: 7}
	instrumented := Instrument(base, tp, staticSpanName)
	assert.Nil(t, base.Transport, "the caller's client must be left untouched")
	assert.NotNil(t, instrumented.Transport)
	assert.NotSame(t, base, instrumented)
	assert.Equal(t, base.Timeout, instrumented.Timeout, "the copy must preserve the caller's timeout")
}
