// Package testembed provides a deterministic bag-of-words Embedder double
// (docs/testing-spec.md, "The Three Test Doubles" -> Deterministic embedder).
// It is a drop-in stand-in for the real Ollama-backed Embedder
// (docs/ingestion-spec.md, "Chunk -> Embed -> Vectors"): same interface, no
// ingest-side branching for tests.
//
// The projection tokenizes text (case-fold, split on non-alphanumeric),
// hashes each token into a fixed-width bag-of-words vector, and L2-normalizes
// it, so cosine similarity tracks keyword overlap rather than any semantic
// signal. Given identical input text it always produces an identical vector
// — no randomness, no external calls — so golden tests and acceptance runs
// are reproducible across CI machines. Semantic quality, synonyms, and
// embedding-model behavior are explicitly out of scope (docs/testing-spec.md,
// "Out of Scope"): this double only exercises plumbing and ranking mechanics.
//
// Dimension is fixed at 768 — the same width as the production Ollama model
// (nomic-embed-text, docs/ingestion-spec.md), so the test Postgres schema's
// chunks.embedding vector(N) column (docs/persistence-spec.md) is
// byte-identical to production. This value must stay fixed for the life of
// the suite: changing it is a model swap in every sense that matters to the
// ingest pipeline, and forces the same full-rebuild rule as swapping the
// real embedding model (docs/ingestion-spec.md).
package testembed

import (
	"context"
	"hash/fnv"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Dimension is the fixed vector width this Embedder produces.
const Dimension = 768

// modelIDPrefix identifies this double's projection. ModelID appends
// Dimension to it, so a Dimension change is detectable as a model change by
// the ingest pipeline, the same as it would be for the real embedding model.
const modelIDPrefix = "testembed-bow-v1-d"

var tokenPattern = regexp.MustCompile(`[a-z0-9]+`)

// Embedder is a deterministic bag-of-words Embedder double for tests.
type Embedder struct{}

// New returns a ready-to-use deterministic Embedder double.
func New() *Embedder {
	return &Embedder{}
}

// Embed returns one L2-normalized bag-of-words vector per input text, in the
// same order as texts. It never calls out to anything external; the only
// error it can return is ctx's cancellation/deadline error.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vectors[i] = embedOne(text)
	}
	return vectors, nil
}

// Dimension reports the fixed vector width produced by Embed.
func (e *Embedder) Dimension() int {
	return Dimension
}

// ModelID identifies this double's projection version, including Dimension.
func (e *Embedder) ModelID() string {
	return modelIDPrefix + strconv.Itoa(Dimension)
}

func embedOne(text string) []float32 {
	vec := make([]float32, Dimension)
	for _, token := range tokenize(text) {
		vec[tokenIndex(token)]++
	}
	l2Normalize(vec)
	return vec
}

func tokenize(text string) []string {
	return tokenPattern.FindAllString(strings.ToLower(text), -1)
}

func tokenIndex(token string) int {
	h := fnv.New32a()
	h.Write([]byte(token))
	return int(h.Sum32() % uint32(Dimension))
}

func l2Normalize(vec []float32) {
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	if sumSq == 0 {
		return
	}
	norm := float32(math.Sqrt(sumSq))
	for i := range vec {
		vec[i] /= norm
	}
}
