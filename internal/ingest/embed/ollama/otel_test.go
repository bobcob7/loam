package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// recordingProvider returns a TracerProvider that keeps every ended span in
// memory, plus the recorder to read them back from.
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

// embedServer starts a stub Ollama that answers /api/embed with one
// zero-vector per input, and reports back the raw request body it received.
func embedServer(t *testing.T, dimension int, seen *[]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		*seen = body
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		embeddings := make([][]float32, len(req.Input))
		for i := range embeddings {
			embeddings[i] = make([]float32, dimension)
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings}))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestInstrumentHTTPClient_NeverRecordsRequestBody is the embedder's
// counterpart to internal/forge's credential test, and it guards the thing
// that is actually sensitive here: the chunk TEXT being embedded is
// repository source code, and the response is its embedding vector.
//
// The sentinel is planted in the text handed to Embed, so it reaches the
// wire inside the real marshalled /api/embed body (the handler asserts as
// much, so the span's silence is about something that was genuinely there).
// It must then appear nowhere on the span. This fails the day someone wraps
// the transport in something body-reading, or adds a formatter or attribute
// derived from the batch.
func TestInstrumentHTTPClient_NeverRecordsRequestBody(t *testing.T) {
	t.Parallel()
	const sentinel = "func withdrawEverything(acct Account) // l0am-chunk-sentinel"
	var seen []byte
	server := embedServer(t, 768, &seen)
	tp, recorder := recordingProvider(t)
	embedder, err := New(server.URL, "nomic-embed-text", InstrumentHTTPClient(server.Client(), tp), testLogger())
	require.NoError(t, err)
	vectors, err := embedder.Embed(t.Context(), []string{sentinel, "package main"})
	require.NoError(t, err)
	require.Len(t, vectors, 2)
	require.Contains(t, string(seen), "l0am-chunk-sentinel", "the chunk text must genuinely have been on the wire")
	spans := recorder.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.NotContains(t, span.Name(), "l0am-chunk-sentinel", "the span name must not be derived from the batch")
	attrs := span.Attributes()
	require.NotEmpty(t, attrs, "a span with no attributes would pass this test vacuously")
	for _, kv := range attrs {
		assert.NotContains(t, kv.Value.Emit(), "l0am-chunk-sentinel", "no span attribute may carry the text being embedded")
	}
	for _, event := range span.Events() {
		for _, kv := range event.Attributes {
			assert.NotContains(t, kv.Value.Emit(), "l0am-chunk-sentinel")
		}
	}
}

// TestInstrumentHTTPClient_NamesTheEndpointAndNestsUnderTheCaller pins both
// halves of what makes an embedder span useful during an ingest run: it has
// to be distinguishable from every other POST in the trace, and it has to
// hang under the ingest span rather than float as its own root.
func TestInstrumentHTTPClient_NamesTheEndpointAndNestsUnderTheCaller(t *testing.T) {
	t.Parallel()
	var seen []byte
	server := embedServer(t, 768, &seen)
	tp, recorder := recordingProvider(t)
	embedder, err := New(server.URL, "nomic-embed-text", InstrumentHTTPClient(server.Client(), tp), testLogger())
	require.NoError(t, err)
	ctx, parent := tp.Tracer("test").Start(t.Context(), "ingest job")
	_, err = embedder.Embed(ctx, []string{"package main"})
	require.NoError(t, err)
	parent.End()
	spans := recorder.Ended()
	require.Len(t, spans, 2)
	var child sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() != "ingest job" {
			child = s
		}
	}
	require.NotNil(t, child)
	assert.Equal(t, "embedder POST /api/embed", child.Name())
	assert.NotEqual(t, "HTTP POST", child.Name(), "otelhttp's default would be indistinguishable from internal/forge's CreatePR")
	assert.Equal(t, parent.SpanContext().SpanID(), child.Parent().SpanID())
	assert.Equal(t, parent.SpanContext().TraceID(), child.SpanContext().TraceID())
	assert.Equal(t, trace.SpanKindClient, child.SpanKind())
}

// TestSpanNameForRequest_TracksTheEndpointNotAConstant is the anti-fixture-
// blindness check. This package has exactly one endpoint, so every
// integration-shaped test above would pass identically against
// `return "embedder POST /api/embed"` hard-coded. Feeding the formatter a
// path it has never seen is the only thing that distinguishes "the name is
// built from the request" from "the name is a constant that happens to
// match".
func TestSpanNameForRequest_TracksTheEndpointNotAConstant(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		rawURL string
		want   string
	}{
		{name: "the real endpoint", method: http.MethodPost, rawURL: "http://localhost:11434/api/embed", want: "embedder POST /api/embed"},
		{name: "a base URL with a path prefix, as a reverse-proxied Ollama would have", method: http.MethodPost, rawURL: "http://gpu-box:11434/ollama/api/embed", want: "embedder POST /ollama/api/embed"},
		{name: "a different method", method: http.MethodGet, rawURL: "http://localhost:11434/api/embed", want: "embedder GET /api/embed"},
		{name: "the legacy endpoint a misconfigured base URL could reach", method: http.MethodPost, rawURL: "http://localhost:11434/api/embeddings", want: "embedder POST /api/embeddings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(tt.method, tt.rawURL, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, spanNameForRequest("", req))
		})
	}
}

// TestInstrumentHTTPClient_NilProviderIsAPassthrough covers the composition
// root's telemetry-disabled path: no wrapper on the transport stack at all.
func TestInstrumentHTTPClient_NilProviderIsAPassthrough(t *testing.T) {
	t.Parallel()
	client := &http.Client{}
	assert.Same(t, client, InstrumentHTTPClient(client, nil))
}

// TestInstrumentHTTPClient_DoesNotMutateTheCaller pins the shallow-copy
// contract, matching internal/forge's equivalent.
func TestInstrumentHTTPClient_DoesNotMutateTheCaller(t *testing.T) {
	t.Parallel()
	tp, _ := recordingProvider(t)
	base := &http.Client{Timeout: 11}
	instrumented := InstrumentHTTPClient(base, tp)
	assert.Nil(t, base.Transport)
	assert.NotNil(t, instrumented.Transport)
	assert.NotSame(t, base, instrumented)
	assert.Equal(t, base.Timeout, instrumented.Timeout)
}
