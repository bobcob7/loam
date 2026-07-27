package fakeembed

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/ingest/embed/ollama"
	"github.com/bobcob7/loam/internal/testembed"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newServed starts this package's Server behind a real httptest listener
// and returns the PRODUCTION ollama.Embedder pointed at it. Every test
// below drives that production client rather than this package's handler
// directly: the contract this package has to keep is not "it emits some
// JSON" but "the shipped client accepts it", and only the real client can
// assert that.
func newServed(t *testing.T, model string) (*ollama.Embedder, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(New(model, testLogger()))
	t.Cleanup(ts.Close)
	clientModel := model
	if clientModel == "" {
		clientModel = DefaultModel
	}
	embedder, err := ollama.New(ts.URL, clientModel, ts.Client(), testLogger())
	require.NoError(t, err)
	return embedder, ts
}

func TestEmbed_ProductionClientRoundTrips(t *testing.T) {
	t.Parallel()
	embedder, _ := newServed(t, "")
	vectors, err := embedder.Embed(t.Context(), []string{"alpha beta", "gamma"})
	require.NoError(t, err)
	require.Len(t, vectors, 2)
	assert.Len(t, vectors[0], testembed.Dimension)
	assert.Len(t, vectors[1], testembed.Dimension)
}

// TestEmbed_MatchesTestembedExactly pins the whole point of routing
// testembed through HTTP: the vector a caller gets over the wire is the
// same vector testembed computes in process, so a ranking property proven
// against the in-process double still holds against this server.
func TestEmbed_MatchesTestembedExactly(t *testing.T) {
	t.Parallel()
	embedder, _ := newServed(t, "")
	const text = "how is authentication handled"
	got, err := embedder.Embed(t.Context(), []string{text})
	require.NoError(t, err)
	want, err := testembed.New().Embed(t.Context(), []string{text})
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestEmbed_Deterministic(t *testing.T) {
	t.Parallel()
	embedder, _ := newServed(t, "")
	first, err := embedder.Embed(t.Context(), []string{"same text"})
	require.NoError(t, err)
	second, err := embedder.Embed(t.Context(), []string{"same text"})
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

// TestEmbed_PreservesInputOrder guards the property the batched /api/embed
// endpoint exists for: one vector per input, in request order.
func TestEmbed_PreservesInputOrder(t *testing.T) {
	t.Parallel()
	embedder, _ := newServed(t, "")
	texts := []string{"first", "second", "third"}
	batched, err := embedder.Embed(t.Context(), texts)
	require.NoError(t, err)
	require.Len(t, batched, len(texts))
	for i, text := range texts {
		single, err := embedder.Embed(t.Context(), []string{text})
		require.NoError(t, err)
		assert.Equal(t, single[0], batched[i], "vector %d does not match a single-input embed of %q", i, text)
	}
}

// TestEmbed_UnknownModel_IsClassifiedAsPullTheModel proves the 404 body
// this server writes reaches the operator as the actionable half of
// ollama.classifyNotFound, not as "upgrade Ollama".
func TestEmbed_UnknownModel_IsClassifiedAsPullTheModel(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(New("nomic-embed-text", testLogger()))
	t.Cleanup(ts.Close)
	embedder, err := ollama.New(ts.URL, "bge-m3", ts.Client(), testLogger())
	require.NoError(t, err)
	_, err = embedder.Embed(t.Context(), []string{"anything"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull it first")
	assert.False(t, ollama.IsRetryable(err), "an unpulled model is a permanent misconfiguration, not a transient failure")
}

// TestEmbed_TagSuffixIsServed mirrors ollama.modelFamily: a client
// configured with an explicit tag must still be served by a server holding
// the same base model.
func TestEmbed_TagSuffixIsServed(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(New("nomic-embed-text", testLogger()))
	t.Cleanup(ts.Close)
	embedder, err := ollama.New(ts.URL, "nomic-embed-text:latest", ts.Client(), testLogger())
	require.NoError(t, err)
	vectors, err := embedder.Embed(t.Context(), []string{"tagged"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
}

// TestEmbed_OverBudgetInput_IsRejectedNotTruncated is the truncate:false
// contract end to end: the production client always sends truncate:false,
// so an over-budget input must come back as
// ollama.IsContextLengthExceeded rather than as a silently partial vector.
func TestEmbed_OverBudgetInput_IsRejectedNotTruncated(t *testing.T) {
	t.Parallel()
	embedder, _ := newServed(t, "")
	oversized := strings.Repeat("x", chunk.TokenBudgetChars(testembed.ContextWindow)+1)
	_, err := embedder.Embed(t.Context(), []string{oversized})
	require.Error(t, err)
	assert.True(t, ollama.IsContextLengthExceeded(err), "want a context-length rejection, got %v", err)
	assert.False(t, ollama.IsRetryable(err))
}

// TestEmbed_AtBudget_IsAccepted pins the boundary from the other side, so
// the guard above cannot drift into rejecting chunks the chunker itself
// considers legal.
func TestEmbed_AtBudget_IsAccepted(t *testing.T) {
	t.Parallel()
	embedder, _ := newServed(t, "")
	atBudget := strings.Repeat("x", chunk.TokenBudgetChars(testembed.ContextWindow))
	vectors, err := embedder.Embed(t.Context(), []string{atBudget})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
}

// TestEmbed_TruncateTrue_ReturnsTruncatedVector documents the behaviour
// the production client deliberately avoids by sending truncate:false --
// a 200 carrying a well-formed but silently partial vector. It is here so
// the difference between the two modes is observable against this fake
// exactly as it is against a real server.
func TestEmbed_TruncateTrue_ReturnsTruncatedVector(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(New("", testLogger()))
	t.Cleanup(ts.Close)
	budget := chunk.TokenBudgetChars(testembed.ContextWindow)
	head := strings.Repeat("a ", budget/2)
	body := `{"model":"nomic-embed-text","input":[` + quote(head+"tail") + `],"truncate":true,"options":{"num_ctx":2048}}`
	resp, err := ts.Client().Post(ts.URL+"/api/embed", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestEmbed_MalformedBody_IsBadRequest(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(New("", testLogger()))
	t.Cleanup(ts.Close)
	resp, err := ts.Client().Post(ts.URL+"/api/embed", "application/json", strings.NewReader("{not json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestEmbed_EmptyInput_NeverReachesTheServer(t *testing.T) {
	t.Parallel()
	embedder, _ := newServed(t, "")
	vectors, err := embedder.Embed(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, vectors)
}

// TestLiveness_MatchesOllama pins the readiness signal a caller polls, so
// no demo or harness has to invent an endpoint this package made up.
func TestLiveness_MatchesOllama(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(New("", testLogger()))
	t.Cleanup(ts.Close)
	resp, err := ts.Client().Get(ts.URL + "/")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, liveBody, string(body))
}

func TestNew_DefaultsToTheConfiguredProductionModel(t *testing.T) {
	t.Parallel()
	assert.Equal(t, DefaultModel, New("", testLogger()).Model())
	assert.Equal(t, "bge-m3", New("bge-m3", testLogger()).Model())
}
