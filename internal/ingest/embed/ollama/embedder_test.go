package ollama

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bobcob7/loam/internal/ingest/embed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ embed.Embedder = (*Embedder)(nil)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// serveEmbed builds an httptest server whose /api/embed handler decodes the
// request and calls build to produce the response body and status. It also
// returns an *int32 call counter so tests can assert a request was (or was
// not) made.
func serveEmbed(t *testing.T, build func(req embedRequest) (status int, body string)) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		require.Equal(t, "/api/embed", r.URL.Path)
		var req embedRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		status, body := build(req)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func TestNew_UnknownModel_ReturnsError(t *testing.T) {
	t.Parallel()
	_, err := New("http://localhost:11434", "some-unpublished-model", http.DefaultClient, testLogger())
	require.ErrorIs(t, err, errUnknownModel)
}

func TestNew_MissingURL_ReturnsError(t *testing.T) {
	t.Parallel()
	_, err := New("", "nomic-embed-text", http.DefaultClient, testLogger())
	require.ErrorIs(t, err, errMissingURL)
}

func TestNew_MissingHTTPClient_ReturnsError(t *testing.T) {
	t.Parallel()
	_, err := New("http://localhost:11434", "nomic-embed-text", nil, testLogger())
	require.ErrorIs(t, err, errMissingHTTPClient)
}

func TestNew_ModelTagSuffix_ResolvesBaseDimension(t *testing.T) {
	t.Parallel()
	e, err := New("http://localhost:11434", "nomic-embed-text:latest", http.DefaultClient, testLogger())
	require.NoError(t, err)
	assert.Equal(t, 768, e.Dimension())
	assert.Equal(t, "ollama/nomic-embed-text:latest", e.ModelID())
}

func TestEmbed_HappyPath_ReturnsWellFormedVector(t *testing.T) {
	t.Parallel()
	server, calls := serveEmbed(t, func(req embedRequest) (int, string) {
		vec := make([]float32, 768)
		vec[0] = 1
		resp := embedResponse{Embeddings: [][]float32{vec}}
		out, err := json.Marshal(resp)
		require.NoError(t, err)
		return http.StatusOK, string(out)
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{"hello world"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.Len(t, vectors[0], 768)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

func TestEmbed_BatchOrdering_MapsEachInputToItsOwnVector(t *testing.T) {
	t.Parallel()
	// Each response vector's [0] element is tagged with the *length* of the
	// corresponding request input, not its position, so a reordering bug
	// (e.g. a reversed Embeddings slice) would be caught: the assertion
	// below checks vectors[i] against len(texts[i]) for the *original*
	// texts order, not merely that i distinct vectors came back.
	server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
		embeddings := make([][]float32, len(req.Input))
		for i, text := range req.Input {
			vec := make([]float32, 768)
			vec[0] = float32(len(text))
			embeddings[i] = vec
		}
		out, err := json.Marshal(embedResponse{Embeddings: embeddings})
		require.NoError(t, err)
		return http.StatusOK, string(out)
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	texts := []string{"a", "bb", "ccc", "dddd"}
	vectors, err := e.Embed(t.Context(), texts)
	require.NoError(t, err)
	require.Len(t, vectors, len(texts))
	for i, text := range texts {
		assert.Equalf(t, float32(len(text)), vectors[i][0], "vectors[%d] should correspond to texts[%d]=%q", i, i, text)
	}
}

func TestEmbed_WrongDimension_ReturnsError(t *testing.T) {
	t.Parallel()
	server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
		resp := embedResponse{Embeddings: [][]float32{make([]float32, 384)}}
		out, err := json.Marshal(resp)
		require.NoError(t, err)
		return http.StatusOK, string(out)
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{"hello"})
	require.ErrorIs(t, err, errDimensionMismatch)
	assert.Nil(t, vectors)
}

func TestEmbed_BatchCountMismatch_ReturnsError(t *testing.T) {
	t.Parallel()
	server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
		resp := embedResponse{Embeddings: [][]float32{make([]float32, 768)}}
		out, err := json.Marshal(resp)
		require.NoError(t, err)
		return http.StatusOK, string(out)
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{"one", "two"})
	require.ErrorIs(t, err, errMalformedResponse)
	assert.Nil(t, vectors)
}

func TestEmbed_MalformedJSON_ReturnsError(t *testing.T) {
	t.Parallel()
	server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
		return http.StatusOK, "{not valid json"
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{"hello"})
	require.ErrorIs(t, err, errMalformedResponse)
	assert.Nil(t, vectors)
}

func TestEmbed_NonOKStatus_ReturnsServerErrorNotRequestFailed(t *testing.T) {
	t.Parallel()
	server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
		return http.StatusInternalServerError, `{"error":"model not found"}`
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{"hello"})
	require.ErrorIs(t, err, errServerError)
	assert.NotErrorIs(t, err, errRequestFailed)
	assert.Nil(t, vectors)
}

func TestEmbed_UnreachableServer_ReturnsRequestFailedNotServerError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close() // now nothing is listening at url
	e, err := New(url, "nomic-embed-text", http.DefaultClient, testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{"hello"})
	require.ErrorIs(t, err, errRequestFailed)
	assert.NotErrorIs(t, err, errServerError)
	assert.Nil(t, vectors)
}

func TestEmbed_AlreadyCanceledContext_ReturnsPromptlyWithoutRequest(t *testing.T) {
	t.Parallel()
	server, calls := serveEmbed(t, func(req embedRequest) (int, string) {
		return http.StatusOK, `{"embeddings":[]}`
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	vectors, err := e.Embed(ctx, []string{"hello"})
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, vectors)
	assert.Equal(t, int32(0), atomic.LoadInt32(calls), "a canceled context must not reach the server")
}

func TestEmbed_ContextTimeoutMidRequest_ReturnsPromptlyWithDeadlineExceeded(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embeddings":[[0]]}`))
	}))
	t.Cleanup(server.Close)
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	vectors, err := e.Embed(ctx, []string{"hello"})
	elapsed := time.Since(start)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	// The ctx-preference branch (embedder.go, after httpClient.Do fails)
	// must classify this as a context error, not a connectivity failure:
	// without it, a timed-out ingest would be indistinguishable from an
	// unreachable embedder to the caller's retry-vs-hard-fail decision
	// (loam-c94.13). net/http's *url.Error already unwraps to
	// context.DeadlineExceeded on its own, so require.ErrorIs above holds
	// even with that branch deleted; this assertion is what actually pins
	// the classification.
	assert.NotErrorIs(t, err, errRequestFailed)
	assert.Nil(t, vectors)
	assert.Lessf(t, elapsed, 400*time.Millisecond, "Embed should return promptly on context deadline, not wait for the slow handler")
}

// TestEmbed_ContextCanceledMidRequest exercises genuine mid-flight
// cancellation (cancel() called from another goroutine after the request
// has reached the server), as opposed to TestEmbed_AlreadyCanceledContext
// (canceled before Embed is ever called). It must still classify as
// context.Canceled, not errRequestFailed.
func TestEmbed_ContextCanceledMidRequest_ReturnsCanceledNotRequestFailed(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embeddings":[[0]]}`))
	}))
	t.Cleanup(server.Close)
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	vectors, err := e.Embed(ctx, []string{"hello"})
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, errRequestFailed)
	assert.Nil(t, vectors)
}

// TestEmbed_ContextTimeoutDuringBodyRead covers the equivalent
// ctx-preference branch on the io.ReadAll path (embedder.go, after the
// body-read fails): headers are flushed immediately so httpClient.Do
// succeeds, then the body write is delayed past the context deadline, so
// the failure surfaces from reading resp.Body rather than from Do itself.
func TestEmbed_ContextTimeoutDuringBodyRead_ReturnsDeadlineExceededNotRequestFailed(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"embeddings":[[0]]}`))
	}))
	t.Cleanup(server.Close)
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	vectors, err := e.Embed(ctx, []string{"hello"})
	elapsed := time.Since(start)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, errRequestFailed)
	assert.Nil(t, vectors)
	assert.Lessf(t, elapsed, 250*time.Millisecond, "Embed should return promptly on context deadline during body read")
}

func TestEmbed_EmptyInput_ReturnsEmptySliceWithoutRequest(t *testing.T) {
	t.Parallel()
	server, calls := serveEmbed(t, func(req embedRequest) (int, string) {
		return http.StatusOK, `{"embeddings":[]}`
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{})
	require.NoError(t, err)
	assert.Empty(t, vectors)
	assert.Equal(t, int32(0), atomic.LoadInt32(calls), "an empty input slice must not reach the server")
}

func TestDimension_MatchesConfiguredModel(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"nomic-embed-text":  768,
		"mxbai-embed-large": 1024,
		"bge-m3":            1024,
		"all-minilm":        384,
	}
	for model, want := range cases {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			e, err := New("http://localhost:11434", model, http.DefaultClient, testLogger())
			require.NoError(t, err)
			assert.Equal(t, want, e.Dimension())
		})
	}
}

func TestModelID_ChangesWhenModelChanges(t *testing.T) {
	t.Parallel()
	a, err := New("http://localhost:11434", "nomic-embed-text", http.DefaultClient, testLogger())
	require.NoError(t, err)
	b, err := New("http://localhost:11434", "mxbai-embed-large", http.DefaultClient, testLogger())
	require.NoError(t, err)
	assert.NotEqual(t, a.ModelID(), b.ModelID())
}

func TestModelID_StableForSameModel(t *testing.T) {
	t.Parallel()
	a, err := New("http://localhost:11434", "nomic-embed-text", http.DefaultClient, testLogger())
	require.NoError(t, err)
	b, err := New("http://localhost:11434", "nomic-embed-text", http.DefaultClient, testLogger())
	require.NoError(t, err)
	assert.Equal(t, a.ModelID(), b.ModelID())
}
