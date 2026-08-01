package vectors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/ingest/chunker"
	"github.com/bobcob7/loam/internal/ingest/embed/ollama"
)

// fakeOllamaServer starts an httptest.Server that answers /api/embed the way
// a real Ollama server does when truncate:false and one of the batch's
// inputs is longer than byteLimit bytes: HTTP 400 with body
// {"error":"the input length exceeds the context length"} -- verified live
// against Ollama v0.32.4 per internal/ingest/embed/ollama's own doc comment.
// byteLimit stands in for the model's real per-input token budget: this
// test cares about the REACTION to that rejection, not about reproducing
// Ollama's actual tokenizer, so a byte threshold is enough to trigger the
// same error path a real dense-JSON/base64 chunk would.
func fakeOllamaServer(t *testing.T, byteLimit int, dimension int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		for _, in := range req.Input {
			if len(in) > byteLimit {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"the input length exceeds the context length"}`))
				return
			}
		}
		embeddings := make([][]float32, len(req.Input))
		for i := range embeddings {
			embeddings[i] = make([]float32, dimension)
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings}))
	}))
}

// TestIngestFileChunks_ChunkExceedsEmbedderContextWindow_SplitsAndRetries is
// loam-c94.16's proof: a chunk that fits internal/ingest/chunk's byte-budget
// estimate (bytesPerTokenBudget) can still be too token-dense for the real
// model to accept -- the bead's own live measurement found 4096B of dense
// JSONL rejected while the estimate says it should fit. Before this fix,
// that ONE oversized chunk failed the whole batch's Embed call, and
// pool.go's classifier (ollama.IsPermanent, which subsumes
// ollama.IsContextLengthExceeded) treated it as permanent -- so the entire
// repo's ingest job never completed, ever, no matter how many times it
// retried unchanged.
//
// This test wires a REAL ollama.Embedder against a fake Ollama HTTP server
// (fakeOllamaServer) that rejects oversized input exactly the way live
// Ollama does, so the error IngestFileChunks sees is a genuine
// ollama.IsContextLengthExceeded -- not a hand-rolled stand-in -- and
// asserts the ingest completes: the oversized unit is split into pieces
// that each fit, every piece is persisted with its OWN vector, and the
// pieces reassemble the original content exactly (lossless).
func TestIngestFileChunks_ChunkExceedsEmbedderContextWindow_SplitsAndRetries(t *testing.T) {
	t.Parallel()
	const byteLimit = 40
	const dimension = 768 // nomic-embed-text's real dimension
	server := fakeOllamaServer(t, byteLimit, dimension)
	defer server.Close()
	e, err := ollama.New(server.URL, "nomic-embed-text", server.Client(), testLogger())
	require.NoError(t, err)
	st, calls := newFakeStore()
	const smallContent = "small ok chunk"
	oversized := strings.Join([]string{
		"line-of-content", "line-of-content", "line-of-content",
		"line-of-content", "line-of-content", "line-of-content",
	}, "\n")
	require.Greater(t, len(oversized), byteLimit, "the test fixture itself must exceed byteLimit, or this test proves nothing")
	files := []chunker.FileChunks{{
		Path: testFileA,
		Units: []chunk.Unit{
			{StartLine: 1, EndLine: 1, Content: smallContent},
			{StartLine: 2, EndLine: 7, Content: oversized},
		},
	}}

	stats, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, files)
	require.NoError(t, err, "one token-dense chunk must not fail the whole ingest")

	require.Len(t, *calls, 1, "the file must still get exactly one ReplaceFileChunks call")
	inputs := (*calls)[0].inputs
	require.Greater(t, len(inputs), 2, "the oversized chunk must have been split into more than one persisted row")
	assert.Equal(t, smallContent, inputs[0].Content, "the chunk that was never oversized must be untouched")
	assert.Len(t, inputs[0].Embedding, dimension)
	var rebuilt strings.Builder
	for _, in := range inputs[1:] {
		assert.LessOrEqualf(t, len(in.Content), byteLimit, "piece %q must fit what the embedder actually accepted, or the split didn't do its job", in.Content)
		assert.Len(t, in.Embedding, dimension, "every split piece must carry its own real embedding, not a placeholder or a copy of another piece's")
		rebuilt.WriteString(in.Content)
	}
	assert.Equal(t, oversized, rebuilt.String(), "splitting must be lossless: concatenating every piece must reproduce the original chunk's content exactly")
	assert.Greater(t, stats.EmbedCalls, 1, "recovering from the batch rejection must cost extra embed calls, visible to an operator via Stats")
}
