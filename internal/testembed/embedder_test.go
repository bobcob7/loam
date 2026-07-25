package testembed

import (
	"math"
	"testing"

	"github.com/bobcob7/loam/internal/ingest/embed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ embed.Embedder = (*Embedder)(nil)

func cosine(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func TestEmbed_CosineIncreasesWithSharedTokenCount(t *testing.T) {
	t.Parallel()
	e := New()
	query := "auth token validate session"
	docs := []string{
		"database migration schema export",
		"auth database migration schema",
		"auth token database migration",
		"auth token validate database",
	}
	sharedCounts := []int{0, 1, 2, 3}
	vectors, err := e.Embed(t.Context(), append([]string{query}, docs...))
	require.NoError(t, err)
	queryVec := vectors[0]
	docVecs := vectors[1:]
	similarities := make([]float64, len(docVecs))
	for i, docVec := range docVecs {
		similarities[i] = cosine(queryVec, docVec)
	}
	for i := 1; i < len(similarities); i++ {
		assert.Greaterf(t, similarities[i], similarities[i-1], "doc with %d shared tokens should rank above doc with %d shared tokens", sharedCounts[i], sharedCounts[i-1])
	}
}

func TestEmbed_Deterministic(t *testing.T) {
	t.Parallel()
	e := New()
	text := "the auth chunk ranks first for an auth query"
	first, err := e.Embed(t.Context(), []string{text})
	require.NoError(t, err)
	second, err := e.Embed(t.Context(), []string{text})
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestEmbed_DeterministicAcrossInstances(t *testing.T) {
	t.Parallel()
	text := "reproducible across CI machines"
	first, err := New().Embed(t.Context(), []string{text})
	require.NoError(t, err)
	second, err := New().Embed(t.Context(), []string{text})
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestEmbed_L2Normalized(t *testing.T) {
	t.Parallel()
	e := New()
	vectors, err := e.Embed(t.Context(), []string{"some non-empty chunk of code and prose"})
	require.NoError(t, err)
	var sumSq float64
	for _, v := range vectors[0] {
		sumSq += float64(v) * float64(v)
	}
	assert.InDelta(t, 1.0, math.Sqrt(sumSq), 1e-6)
}

func TestEmbed_EmptyTextYieldsZeroVector(t *testing.T) {
	t.Parallel()
	e := New()
	vectors, err := e.Embed(t.Context(), []string{""})
	require.NoError(t, err)
	require.Len(t, vectors[0], Dimension)
	for _, v := range vectors[0] {
		assert.Zero(t, v)
	}
}

func TestEmbed_ReturnsOneVectorPerInputInOrder(t *testing.T) {
	t.Parallel()
	e := New()
	vectors, err := e.Embed(t.Context(), []string{"alpha", "beta", "gamma"})
	require.NoError(t, err)
	require.Len(t, vectors, 3)
	assert.NotEqual(t, vectors[0], vectors[1])
	assert.NotEqual(t, vectors[1], vectors[2])
}

func TestDimension_IsStableAndMatchesVectorWidth(t *testing.T) {
	t.Parallel()
	e := New()
	vectors, err := e.Embed(t.Context(), []string{"stable dimension check"})
	require.NoError(t, err)
	assert.Equal(t, Dimension, e.Dimension())
	assert.Len(t, vectors[0], e.Dimension())
}

func TestModelID_IsStable(t *testing.T) {
	t.Parallel()
	e := New()
	first := e.ModelID()
	second := New().ModelID()
	assert.Equal(t, first, second)
	assert.NotEmpty(t, first)
}
