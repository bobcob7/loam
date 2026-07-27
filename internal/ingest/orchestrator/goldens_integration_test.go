//go:build integration

// Golden ingest tests over internal/testfixture's fixture-polyglot
// (bead loam-li0.8, docs/testing-spec.md Layer 2 "Ingest pipeline").
//
// The point of a golden here is that a change in Tree-sitter extraction,
// in chunking, or in edge resolution shows up as a reviewable DIFF against
// a committed file rather than being silently absorbed by an assertion
// loose enough to keep passing. Regenerate deliberately:
//
//	LOAM_UPDATE_GOLDENS=1 TESTCONTAINERS_RYUK_DISABLED=true \
//	  go test -tags=integration -p 1 -count=1 -run Golden ./internal/ingest/orchestrator/...
//
// and review the resulting diff before committing it. Without that
// variable a stale golden FAILS -- there is no auto-accept path, and a
// MISSING golden fails too rather than being quietly created.
package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/testembed"
)

// updateGoldensEnv is the ONE way to re-baseline a golden. It is read, not
// a flag, so it survives `go test ./...` invocations that cannot thread
// custom flags through, and so it is impossible to set by accident from a
// normal test run.
const updateGoldensEnv = "LOAM_UPDATE_GOLDENS"

const seedGoldenPath = "testdata/golden/fixture-polyglot-seed.json"

// goldenDocument is what a golden file holds: the commit the derived state
// was built from, plus that state. SeedCommit is in the file on purpose --
// a golden is only meaningful as "this output, for THAT input", and
// pinning the sha means a change to the fixture's own content fails here
// loudly instead of silently re-baselining the rows underneath it.
type goldenDocument struct {
	SeedCommit string `json:"seed_commit"`
	derivedState
}

// TestGolden_FixturePolyglotSeedCommit pins the entire derived output of a
// full ingest of fixture-polyglot at its seed commit: every symbol, every
// reference, every resolved edge, and every chunk's boundaries and text.
//
// Chunk EMBEDDINGS are not in the file (768 floats per chunk would bury
// the content a reviewer is meant to read) but they are not unchecked
// either: the subtest below re-derives every stored vector from
// internal/testembed and asserts it matches the stored one exactly, which
// is a stronger statement than a pinned digest would be.
func TestGolden_FixturePolyglotSeedCommit(t *testing.T) {
	t.Parallel()
	f := newPolyglotFixture(t)
	stats, plan := runIngest(t, f, ingest.KindIncremental)
	require.Equal(t, ingest.KindFull, plan.Kind, "a never-ingested branch is a first ingest, which diffplan escalates to a full rebuild")
	require.Positive(t, stats.FilesParsed)
	require.Positive(t, stats.ChunksEmbedded)

	got := goldenDocument{SeedCommit: headSHA(t, f), derivedState: readDerivedState(t, f.repoID)}
	require.False(t, got.empty(), "an empty derived state would make this golden vacuously satisfiable")
	compareGolden(t, seedGoldenPath, got)

	t.Run("every stored embedding is testembed's projection of that chunk's own content", func(t *testing.T) {
		t.Parallel()
		embedder := testembed.New()
		for _, c := range got.Chunks {
			want, err := embedder.Embed(t.Context(), []string{c.Content})
			require.NoError(t, err)
			require.Len(t, want, 1)
			assert.Equal(t, want[0], parsePgVector(t, c.Embedding),
				"chunk %s:%d-%d stored a vector that is not this content's embedding", c.File, c.StartLine, c.EndLine)
		}
	})
}

// TestGolden_AmbiguousSameNamedSymbolStaysTwoDistinctRows is
// fixture-polyglot's reason for existing: pkg/validate/validate.go (Go)
// and src/validate.ts (TypeScript) both export a function named Validate.
// Extraction must record two separate symbols -- one per file -- and never
// merge them into a single row keyed on the name.
func TestGolden_AmbiguousSameNamedSymbolStaysTwoDistinctRows(t *testing.T) {
	t.Parallel()
	f := newPolyglotFixture(t)
	runIngest(t, f, ingest.KindIncremental)
	state := readDerivedState(t, f.repoID)

	var validates []symbolRow
	for _, s := range state.Symbols {
		if s.Name == "Validate" && s.Kind == "function" {
			validates = append(validates, s)
		}
	}
	require.Len(t, validates, 2, "the Go and TypeScript Validate must be two rows, not one merged row: got %+v", validates)
	files := distinctFiles(validates)
	assert.Equal(t, []string{"pkg/validate/validate.go", "src/validate.ts"}, files,
		"the two Validate rows must be the Go one and the TypeScript one, each attributed to its own file")
}

// TestGolden_MutualRecursionResolvesEdgesInBothDirections covers
// scripts/parity.py's is_even/is_odd pair: each calls the other, so edge
// resolution must produce an edge in BOTH directions. That the ingest
// returns at all is the cycle-safety half -- resolution walks
// symbol_references against symbols and must not loop on the cycle it is
// building.
func TestGolden_MutualRecursionResolvesEdgesInBothDirections(t *testing.T) {
	t.Parallel()
	f := newPolyglotFixture(t)
	runIngest(t, f, ingest.KindIncremental)
	state := readDerivedState(t, f.repoID)

	assert.True(t, hasEdge(state.Edges, "is_even", "is_odd"), "is_even calls is_odd, so an edge must resolve in that direction")
	assert.True(t, hasEdge(state.Edges, "is_odd", "is_even"), "is_odd calls is_even, so the reverse edge must resolve too")
}

// TestGolden_GrammarlessFilesAreChunkedButProduceNoSymbols is
// docs/ingestion-spec.md's "files with no grammar are skipped for the
// graph" paired with "but they are still chunked for RAG". Markdown has no
// registered Tree-sitter grammar in internal/parser (only Go, Python, and
// the TypeScript family), so fixture-polyglot's three .md files are
// exactly this case.
func TestGolden_GrammarlessFilesAreChunkedButProduceNoSymbols(t *testing.T) {
	t.Parallel()
	f := newPolyglotFixture(t)
	runIngest(t, f, ingest.KindIncremental)
	state := readDerivedState(t, f.repoID)

	grammarless := []string{"README.md", "CHANGELOG.md", "docs/OVERVIEW.md"}
	symbolFilesSet := map[string]struct{}{}
	for _, s := range state.Symbols {
		symbolFilesSet[s.File] = struct{}{}
	}
	referenceFilesSet := map[string]struct{}{}
	for _, r := range state.References {
		referenceFilesSet[r.File] = struct{}{}
	}
	chunkFilesSet := map[string]struct{}{}
	for _, c := range state.Chunks {
		chunkFilesSet[c.File] = struct{}{}
	}
	for _, path := range grammarless {
		assert.NotContains(t, symbolFilesSet, path, "%s has no grammar, so it must produce zero symbols", path)
		assert.NotContains(t, referenceFilesSet, path, "%s has no grammar, so it must produce zero references", path)
		assert.Contains(t, chunkFilesSet, path, "%s must still be chunked for RAG even with no grammar", path)
	}
}

// TestGolden_DeletedAndRenamedFilesDropTheirDerivedRows is the
// incremental drop half stated over fixture-polyglot rather than over the
// ad-hoc repo integration_test.go uses: after one incremental ingest that
// deletes one file and renames another, the deleted path and the rename's
// OLD path must have no symbols, no references and no chunks, and the
// rename's NEW path must carry the rows the old one had.
func TestGolden_DeletedAndRenamedFilesDropTheirDerivedRows(t *testing.T) {
	t.Parallel()
	f := newPolyglotFixture(t)
	runIngest(t, f, ingest.KindIncremental)
	before := readDerivedState(t, f.repoID)
	require.Contains(t, distinctFiles(before.Symbols), "src/index.ts")
	require.Contains(t, distinctFiles(before.Symbols), "scripts/parity.py")

	applyStep(t, f, "delete src/index.ts and rename scripts/parity.py", []mutation{
		deleteFile("src/index.ts"),
		renameFile("scripts/parity.py", "scripts/parity_check.py"),
	})
	_, plan := runIngest(t, f, ingest.KindIncremental)
	require.Equal(t, ingest.KindIncremental, plan.Kind, "this must exercise the INCREMENTAL drop path, not a full rebuild")
	require.Subset(t, plan.DropFiles, []string{"src/index.ts", "scripts/parity.py"},
		"a delete contributes its path and a rename contributes its OLD path to DropFiles")

	after := readDerivedState(t, f.repoID)
	files := distinctFiles(after.Symbols)
	assert.NotContains(t, files, "src/index.ts", "a deleted file's symbols must be gone")
	assert.NotContains(t, files, "scripts/parity.py", "a renamed-away path's symbols must be gone")
	assert.Contains(t, files, "scripts/parity_check.py", "the rename's new path must carry the symbols")
	for _, r := range after.References {
		assert.NotEqual(t, "src/index.ts", r.File)
		assert.NotEqual(t, "scripts/parity.py", r.File)
	}
	for _, c := range after.Chunks {
		assert.NotEqual(t, "src/index.ts", c.File)
		assert.NotEqual(t, "scripts/parity.py", c.File)
	}
	assert.True(t, hasEdge(after.Edges, "is_even", "is_odd"),
		"the renamed file's mutual-recursion edges must be rebuilt at the new path, not lost with the old one")
	for _, e := range after.Edges {
		assert.NotEqual(t, "src/index.ts", e.FromFile, "no edge may still point out of a deleted file")
		assert.NotEqual(t, "src/index.ts", e.ToFile, "no edge may still point into a deleted file's symbol")
	}
}

// --- golden plumbing ---

func hasEdge(edges []edgeRow, from, to string) bool {
	for _, e := range edges {
		if e.FromName == from && e.ToName == to {
			return true
		}
	}
	return false
}

// compareGolden asserts got matches the committed golden at path, or --
// only when updateGoldensEnv is set -- rewrites it. A missing golden is a
// FAILURE in the normal path: silently creating one would mean the first
// run of a broken extractor becomes the baseline everyone else is
// measured against.
func compareGolden(t *testing.T, path string, got goldenDocument) {
	t.Helper()
	encoded := encodeGolden(t, got)
	if os.Getenv(updateGoldensEnv) != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, encoded, 0o644))
		t.Logf("%s=1: rewrote %s (%d bytes) -- review the diff before committing it", updateGoldensEnv, path, len(encoded))
		return
	}
	want, err := os.ReadFile(path)
	require.NoErrorf(t, err, "golden %s is missing; regenerate deliberately with %s=1 and review the diff", path, updateGoldensEnv)
	assert.Equalf(t, string(want), string(encoded),
		"the derived output for fixture-polyglot no longer matches %s.\n"+
			"If this change is intended -- an extraction query, a chunking strategy, or edge resolution changed on purpose --\n"+
			"regenerate with %s=1 and commit the diff. If it is not intended, this is the regression the golden exists to catch.",
		path, updateGoldensEnv)
}

// encodeGolden renders got as indented JSON with HTML escaping OFF, so the
// file holds readable source text ("<", ">", "&" as themselves) rather
// than < escapes.
func encodeGolden(t *testing.T, got goldenDocument) []byte {
	t.Helper()
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(got))
	return []byte(buf.String())
}

// parsePgVector turns pgvector's "[0.1,-0.2,...]" text rendering back into
// the []float32 it was stored from. pgvector prints float4 with shortest
// round-trip precision, so this is lossless and exact equality against a
// freshly computed vector is legitimate for a deterministic embedder.
func parsePgVector(t *testing.T, s string) []float32 {
	t.Helper()
	require.True(t, strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"), "not a pgvector rendering: %q", s)
	body := s[1 : len(s)-1]
	if body == "" {
		return nil
	}
	parts := strings.Split(body, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		require.NoError(t, err)
		out[i] = float32(v)
	}
	return out
}
