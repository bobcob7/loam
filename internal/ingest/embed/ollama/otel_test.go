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
