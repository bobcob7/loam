package ollama

import (
	"context"
	"encoding/json"
	"errors"
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

// TestNew_ContextWindow_FollowsConfiguredModel pins that ContextWindow is
// not a single constant: two different configured models report two
// different budgets, both drawn from knownModelContextWindows, so a
// caller (the chunker, loam-zoa) that switches models automatically gets
// that model's own budget rather than one baked in for nomic-embed-text.
func TestNew_ContextWindow_FollowsConfiguredModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  int
	}{
		{"nomic-embed-text", 2048},
		{"nomic-embed-text:latest", 2048},
		{"mxbai-embed-large", 512},
		{"bge-m3", 8192},
		{"all-minilm", 512},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			e, err := New("http://localhost:11434", tc.model, http.DefaultClient, testLogger())
			require.NoError(t, err)
			assert.Equal(t, tc.want, e.ContextWindow())
		})
	}
}

// TestKnownModelTables_HaveIdenticalKeySets guards against the two
// hand-maintained model-fact tables (knownModelDimensions,
// knownModelContextWindows) drifting apart: a model present in one but not
// the other would make New fail unpredictably depending on which lookup
// ran first, rather than consistently for every known model.
func TestKnownModelTables_HaveIdenticalKeySets(t *testing.T) {
	t.Parallel()
	for model := range knownModelDimensions {
		_, ok := knownModelContextWindows[model]
		assert.Truef(t, ok, "%q has a known dimension but no known context window", model)
	}
	for model := range knownModelContextWindows {
		_, ok := knownModelDimensions[model]
		assert.Truef(t, ok, "%q has a known context window but no known dimension", model)
	}
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
		return http.StatusBadRequest, `{"error":"model not found"}`
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{"hello"})
	require.ErrorIs(t, err, errServerError)
	assert.NotErrorIs(t, err, errRequestFailed)
	assert.False(t, IsRetryable(err), "a 4xx must not be retryable")
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
	assert.True(t, IsRetryable(err), "a transport failure must be retryable")
	assert.Nil(t, vectors)
}

// TestEmbed_StatusClassification is the exhaustive taxonomy test: every
// status Ollama might plausibly return must land in exactly one bucket
// (transient-and-retryable vs. permanent-and-not) and IsRetryable — the one
// exported predicate loam-c94.13 drives retry-vs-hard-fail from — must agree.
// This is also the mutation-test target: flipping any one of these
// classifications in classifyStatusError should fail exactly the
// corresponding case below.
func TestEmbed_StatusClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		wantTransient bool
	}{
		{"500 internal server error is transient", http.StatusInternalServerError, `{"error":"model is loading"}`, true, true},
		{"503 service unavailable is transient", http.StatusServiceUnavailable, `{"error":"server busy"}`, true, true},
		{"429 too many requests is transient", http.StatusTooManyRequests, `{"error":"rate limited"}`, true, true},
		{"400 bad request is permanent", http.StatusBadRequest, `{"error":"invalid request shape"}`, false, false},
		{"404 not found is permanent", http.StatusNotFound, "404 page not found", false, false},
		{"422 unprocessable entity is permanent", http.StatusUnprocessableEntity, `{"error":"unsupported model"}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
				return tc.status, tc.body
			})
			e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
			require.NoError(t, err)
			vectors, err := e.Embed(t.Context(), []string{"hello"})
			require.Error(t, err)
			assert.Nil(t, vectors)
			assert.Equal(t, tc.wantRetryable, IsRetryable(err), "IsRetryable")
			assert.Equal(t, tc.wantTransient, errors.Is(err, errTransientServerError), "errors.Is(err, errTransientServerError)")
			assert.Equal(t, !tc.wantTransient, errors.Is(err, errServerError), "errors.Is(err, errServerError)")
		})
	}
}

// TestEmbed_404Routing_NamesEndpointAndOllamaVersion covers the routing 404:
// the bare Go default body, with no JSON and no model reference, that means
// this server predates /api/embed (pre-v0.1.35) or the URL is wrong.
func TestEmbed_404Routing_NamesEndpointAndOllamaVersion(t *testing.T) {
	t.Parallel()
	server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
		return http.StatusNotFound, "404 page not found"
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{"hello"})
	require.ErrorIs(t, err, errServerError)
	assert.False(t, IsRetryable(err))
	assert.Contains(t, err.Error(), "/api/embed", "a bare 404 body gives no hint the server is too old; the message must name the endpoint")
	assert.Contains(t, err.Error(), "v0.1.35", "the message must name the version boundary so an operator knows to upgrade")
	assert.NotContains(t, err.Error(), "pull it first", "a routing 404 must not tell the operator to pull a model")
	assert.Nil(t, vectors)
}

// TestEmbed_404ModelNotFound_NamesPullInstructionNotOllamaVersion covers the
// far more common 404 in practice: a typo'd or unpulled model name, which
// Ollama reports as JSON mentioning "try pulling it first". Conflating this
// with the routing 404 would tell an operator to upgrade Ollama when they
// actually just need to pull the model.
func TestEmbed_404ModelNotFound_NamesPullInstructionNotOllamaVersion(t *testing.T) {
	t.Parallel()
	server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
		return http.StatusNotFound, `{"error":"model \"nomic-embed-text\" not found, try pulling it first"}`
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{"hello"})
	require.ErrorIs(t, err, errServerError)
	assert.False(t, IsRetryable(err))
	assert.Contains(t, err.Error(), "pull it first", "the likeliest 404 an operator hits must say to pull the model")
	assert.NotContains(t, err.Error(), "v0.1.35", "a model-not-found 404 must not send the operator chasing an Ollama upgrade")
	assert.Nil(t, vectors)
}

func TestEmbed_ContextLengthExceeded_IsDistinctFromOtherPermanentErrors(t *testing.T) {
	t.Parallel()
	server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
		// Verified live against Ollama v0.32.4 with truncate:false.
		return http.StatusBadRequest, `{"error":"the input length exceeds the context length"}`
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := e.Embed(t.Context(), []string{"a very long chunk of text"})
	require.ErrorIs(t, err, errServerError, "still a permanent 4xx classification")
	require.ErrorIs(t, err, errContextLengthExceeded, "must be distinguishable from other 4xx causes")
	assert.False(t, IsRetryable(err), "retrying the same oversized input would not help")
	assert.True(t, IsContextLengthExceeded(err), "the exported predicate must agree with the internal sentinel")
	assert.Nil(t, vectors)
}

// TestEmbed_OtherBadRequest_IsNotMisclassifiedAsContextLengthExceeded pins
// the tightened match (isContextLengthExceededBody requires both "exceeds"
// and "context length"): a body that merely mentions "context length"
// without "exceeds" — a real-shaped rejection, not a contrived one — must
// not be misclassified as the input being too big.
func TestEmbed_OtherBadRequest_IsNotMisclassifiedAsContextLengthExceeded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"unrelated 400", `{"error":"invalid model name"}`},
		{"mentions context length without exceeding it", `{"error":"model does not support setting context length via options"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
				return http.StatusBadRequest, tc.body
			})
			e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
			require.NoError(t, err)
			_, err = e.Embed(t.Context(), []string{"hello"})
			require.ErrorIs(t, err, errServerError)
			assert.NotErrorIs(t, err, errContextLengthExceeded)
			assert.False(t, IsContextLengthExceeded(err))
		})
	}
}

func TestEmbed_MalformedResponse_IsNotRetryable(t *testing.T) {
	t.Parallel()
	server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
		return http.StatusOK, "{not valid json"
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	_, err = e.Embed(t.Context(), []string{"hello"})
	require.ErrorIs(t, err, errMalformedResponse)
	assert.False(t, IsRetryable(err), "a malformed body is a version/protocol mismatch, not transient")
}

func TestEmbed_DimensionMismatch_IsNotRetryable(t *testing.T) {
	t.Parallel()
	server, _ := serveEmbed(t, func(req embedRequest) (int, string) {
		resp := embedResponse{Embeddings: [][]float32{make([]float32, 384)}}
		out, marshalErr := json.Marshal(resp)
		require.NoError(t, marshalErr)
		return http.StatusOK, string(out)
	})
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	_, err = e.Embed(t.Context(), []string{"hello"})
	require.ErrorIs(t, err, errDimensionMismatch)
	assert.False(t, IsRetryable(err), "a dimension mismatch is a model misconfiguration, not transient")
}

// TestIsRetryable_ContextErrors_ReturnFalse pins the ctx contract documented
// on IsRetryable: Embed returns context.Canceled / context.DeadlineExceeded
// unwrapped by any sentinel, so IsRetryable reports false for them — correct
// for "the caller gave up," not "the embedder permanently failed." A caller
// wired to `if !IsRetryable(err) { markFailed() }` must check for a ctx
// error before consulting IsRetryable, or a graceful shutdown mid-ingest
// would be recorded as a permanent embedder failure.
func TestIsRetryable_ContextErrors_ReturnFalse(t *testing.T) {
	t.Parallel()
	assert.False(t, IsRetryable(context.Canceled), "ctx errors are unclassified; callers must check ctx before consulting IsRetryable")
	assert.False(t, IsRetryable(context.DeadlineExceeded), "ctx errors are unclassified; callers must check ctx before consulting IsRetryable")
}

// TestEmbed_SendsTruncateFalse locks in the loam-eg9 decision: Embed must
// send truncate:false on every request, not rely on the field's zero value
// happening to match, so an oversized chunk fails loudly via Ollama's error
// path (docs/ingestion-spec.md, "Consistency & Failure") instead of being
// silently truncated and embedded from partial text.
func TestEmbed_SendsTruncateFalse(t *testing.T) {
	t.Parallel()
	var rawBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		rawBody = string(b)
		vec := make([]float32, 768)
		out, err := json.Marshal(embedResponse{Embeddings: [][]float32{vec}})
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}))
	t.Cleanup(server.Close)
	e, err := New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	_, err = e.Embed(t.Context(), []string{"hello"})
	require.NoError(t, err)
	assert.Contains(t, rawBody, `"truncate":false`)
}

// TestEmbed_SendsNumCtxMatchingContextWindow locks in that Embed declares
// the context window it expects Ollama to serve (embedRequest.Options.
// NumCtx) rather than leaving it to Ollama's own default — the whole
// point of also exposing ContextWindow() being that the two never diverge.
// A different model must send that model's own number, not
// nomic-embed-text's, so this also pins that the value follows the
// configured model rather than being a hardcoded constant.
func TestEmbed_SendsNumCtxMatchingContextWindow(t *testing.T) {
	t.Parallel()
	var rawBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		rawBody = string(b)
		vec := make([]float32, 1024)
		out, err := json.Marshal(embedResponse{Embeddings: [][]float32{vec}})
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}))
	t.Cleanup(server.Close)
	e, err := New(server.URL, "bge-m3", server.Client(), testLogger())
	require.NoError(t, err)
	require.Equal(t, 8192, e.ContextWindow())
	_, err = e.Embed(t.Context(), []string{"hello"})
	require.NoError(t, err)
	assert.Contains(t, rawBody, `"num_ctx":8192`)
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
