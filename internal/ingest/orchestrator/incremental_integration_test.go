//go:build integration

// The incremental-equals-full property (bead loam-li0.8, docs/testing-spec.md
// Layer 2; docs/ingestion-spec.md "Incremental Build").
//
// The claim: for any scripted sequence of changes, ingesting the sequence
// INCREMENTALLY step by step must leave the derived index equivalent to
// ingesting the sequence's END STATE as a single FULL rebuild into a fresh
// repo. "Equivalent" is polyglot_integration_test.go's derivedState -- every
// derived column of symbols, symbol_references, graph_edges and chunks, with
// generated ids and created_at deliberately excluded (see readDerivedState's
// comment for the field-by-field reasoning).
//
// A property test that passes because both sides are empty, or because the
// comparison quietly ignores the field that differs, is worse than no test.
// Three things guard against that, and each is itself a test in this file
// rather than a claim in a comment:
//
//   - Non-vacuity is asserted inline: both sides must be non-empty, and the
//     end state must actually DIFFER from the pre-mutation state, or the
//     sequence proved nothing.
//   - The incremental side's plan Kind is recorded and asserted, so the
//     property can never pass because diffplan silently escalated both
//     sides to a full rebuild.
//   - TestIncrementalEqualsFull_IsFalsifiableWhenTheIncrementalPathSkipsADrop
//     and TestDerivedStateComparison_DetectsACorruptedSide prove the
//     comparison FAILS when it should.
package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/ingest"
)

// --- the scripted change vocabulary, in fixture-polyglot's own terms ---

const extraGoSource = `package extra

import "fixture/pkg/validate"

// Check defers to the Go Validate, adding a cross-package reference from a
// file that did not exist at the base commit.
func Check(s string) bool {
	return validate.Validate(s)
}
`

const validateGoWithSecondFunction = `// Package validate provides input validation for the fixture repo.
package validate

import "strings"

// Validate reports whether s is a non-empty, trimmed string.
func Validate(s string) bool {
	return strings.TrimSpace(s) != ""
}

// ValidateAll reports whether every element of items passes Validate. It is
// added by a scripted modification, so an incremental ingest must re-parse
// this file and produce both a new symbol and a new intra-file reference.
func ValidateAll(items []string) bool {
	for _, item := range items {
		if !Validate(item) {
			return false
		}
	}
	return true
}
`

const overviewWithExtraSection = `# Fixture Polyglot Overview

Rewritten by a scripted modification so section chunking has to re-derive
this file's units.

## Validation

Two languages each export a symbol named ` + "`Validate`" + `.

## Reporting

Cross-file reference resolution within a single language.
`

// --- the property ---

// TestIncrementalEqualsFull is the single most important assertion in this
// bead. Each case scripts a sequence of steps; every step is one commit
// force-pushed to the mirror followed by one INCREMENTAL ingest. Once the
// sequence is done, the same end state is pushed into a second, brand-new
// repo and ingested as a FULL rebuild. The two derived indexes must match.
//
// A divergence here is a real bug in the incremental path, not a reason to
// weaken the comparison.
func TestIncrementalEqualsFull(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		steps [][]mutation
	}{
		{
			name:  "a file is added",
			steps: [][]mutation{{addFile("pkg/extra/extra.go", extraGoSource)}},
		},
		{
			name:  "a file is deleted",
			steps: [][]mutation{{deleteFile("src/index.ts")}},
		},
		{
			name:  "a file is modified",
			steps: [][]mutation{{modifyFile("pkg/validate/validate.go", validateGoWithSecondFunction)}},
		},
		{
			name:  "a file is renamed",
			steps: [][]mutation{{renameFile("scripts/parity.py", "scripts/parity_check.py")}},
		},
		{
			// loam-8uo: the path still EXISTS at the new ref, so it is in
			// the reparse set and never in DropFiles. Only the chunker
			// emitting a zero-unit entry for it drops its stale chunks --
			// which is precisely what this case checks the incremental path
			// does, by comparing against a full rebuild that never had
			// those chunks in the first place.
			name:  "a text file becomes binary",
			steps: [][]mutation{{makeBinary("docs/OVERVIEW.md")}},
		},
		{
			name: "every interesting diff shape in one commit",
			steps: [][]mutation{{
				addFile("pkg/extra/extra.go", extraGoSource),
				deleteFile("src/index.ts"),
				modifyFile("pkg/validate/validate.go", validateGoWithSecondFunction),
				renameFile("scripts/parity.py", "scripts/parity_check.py"),
				makeBinary("README.md"),
			}},
		},
		{
			// Multi-step matters on its own: an incremental ingest builds
			// on the index the PREVIOUS incremental ingest left behind, so
			// a per-step error accumulates in a way a single-step case
			// cannot expose. The last step deletes the file the first step
			// added, which the full rebuild never sees at all.
			name: "a sequence of four incremental steps",
			steps: [][]mutation{
				{addFile("pkg/extra/extra.go", extraGoSource)},
				{modifyFile("pkg/validate/validate.go", validateGoWithSecondFunction)},
				{renameFile("scripts/parity.py", "scripts/parity_check.py"), modifyFile("docs/OVERVIEW.md", overviewWithExtraSection)},
				{deleteFile("pkg/extra/extra.go"), makeBinary("CHANGELOG.md")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inc := newPolyglotFixture(t)
			_, basePlan := runIngest(t, inc, ingest.KindIncremental)
			require.Equal(t, ingest.KindFull, basePlan.Kind, "the first ingest of a never-ingested branch is a full rebuild by definition")
			base := readDerivedState(t, inc.repoID)
			require.False(t, base.empty(), "the base ingest indexed nothing, so nothing downstream of it can mean anything")

			for i, step := range tc.steps {
				applyStep(t, inc, stepMessage(step), step)
				_, plan := runIngest(t, inc, ingest.KindIncremental)
				require.Equalf(t, ingest.KindIncremental, plan.Kind,
					"step %d must exercise the INCREMENTAL path; diffplan escalated it to a full rebuild (%s), which would make this property compare a full rebuild against a full rebuild",
					i, plan.Reason)
				require.NotEmptyf(t, append(append([]string{}, plan.DropFiles...), plan.ReparseFiles...),
					"step %d planned no work at all, so its mutations changed nothing", i)
			}
			got := readDerivedState(t, inc.repoID)
			require.NotEmpty(t, base.differences(got),
				"the scripted sequence left the derived index byte-identical to the base ingest, so it exercised nothing")

			// The full rebuild: the SAME end state pushed into a brand-new
			// repo with an empty index, ingested as one full rebuild. Two
			// distinct repo ids, two distinct mirrors -- an independent
			// build, not a re-run over the same rows.
			full := newFixture(t)
			inc.git(t, inc.work, "push", "--force", full.mirror, "main:main")
			_, fullPlan := runIngest(t, full, ingest.KindFull)
			require.Equal(t, ingest.KindFull, fullPlan.Kind)
			require.Nil(t, fullPlan.DropFiles, "a full plan's drop is repo-scoped, never file-scoped")
			want := readDerivedState(t, full.repoID)
			require.False(t, want.empty(), "the full rebuild indexed nothing, which would make every comparison below vacuously true")

			assert.Equal(t, want.Symbols, got.Symbols, "incremental and full disagree on symbols")
			assert.Equal(t, want.References, got.References, "incremental and full disagree on symbol_references")
			assert.Equal(t, want.Edges, got.Edges, "incremental and full disagree on graph_edges")
			assert.Equal(t, len(want.Chunks), len(got.Chunks), "incremental and full disagree on how many chunks the end state has")
			assertChunksEqual(t, want.Chunks, got.Chunks)
		})
	}
}

// assertChunksEqual compares chunk sets field by field so a failure names
// the file and line range that diverged instead of dumping two slices of
// full file text. Embeddings are compared exactly -- legitimate only
// because both sides went through internal/testembed (see
// readDerivedState's comment).
func assertChunksEqual(t *testing.T, want, got []chunkRow) {
	t.Helper()
	n := min(len(want), len(got))
	for i := 0; i < n; i++ {
		w, g := want[i], got[i]
		if w == g {
			continue
		}
		assert.Equal(t, w.File, g.File, "chunk %d: file differs", i)
		assert.Equal(t, [2]int32{w.StartLine, w.EndLine}, [2]int32{g.StartLine, g.EndLine}, "chunk %d (%s): line span differs", i, w.File)
		assert.Equal(t, w.Content, g.Content, "chunk %d (%s:%d-%d): content differs", i, w.File, w.StartLine, w.EndLine)
		assert.Equal(t, w.Embedding, g.Embedding, "chunk %d (%s:%d-%d): embedding differs", i, w.File, w.StartLine, w.EndLine)
	}
	for i := n; i < len(want); i++ {
		assert.Failf(t, "the incremental index is missing a chunk", "%s:%d-%d exists only in the full rebuild", want[i].File, want[i].StartLine, want[i].EndLine)
	}
	for i := n; i < len(got); i++ {
		assert.Failf(t, "the incremental index has a stale chunk", "%s:%d-%d exists only in the incremental index", got[i].File, got[i].StartLine, got[i].EndLine)
	}
}

func stepMessage(step []mutation) string {
	msg := "step:"
	for _, m := range step {
		msg += " " + m.name + ";"
	}
	return msg
}

// --- proofs that the property and the comparison can actually fail ---

// TestIncrementalEqualsFull_IsFalsifiableWhenTheIncrementalPathSkipsADrop
// is the mutation test for the property itself, kept as a permanent test
// rather than run once by hand.
//
// The incremental ingest is run with a dropper that forwards the
// full-rebuild drop but silently does NOTHING for the per-path drop -- the
// exact bug the property exists to catch. The comparison must then report a
// divergence. If this test ever starts passing with an EMPTY difference
// list, the property above has become unfalsifiable and is worthless.
func TestIncrementalEqualsFull_IsFalsifiableWhenTheIncrementalPathSkipsADrop(t *testing.T) {
	t.Parallel()
	inc := newPolyglotFixture(t)
	runIngest(t, inc, ingest.KindIncremental)

	applyStep(t, inc, "delete src/index.ts and rename scripts/parity.py", []mutation{
		deleteFile("src/index.ts"),
		renameFile("scripts/parity.py", "scripts/parity_check.py"),
	})
	_, plan := runIngestWith(t, inc, ingest.KindIncremental, func(o *Orchestrator) {
		o.dropper = pathDropSkippingDropper{inner: o.dropper}
	})
	require.Equal(t, ingest.KindIncremental, plan.Kind)
	require.NotEmpty(t, plan.DropFiles, "the mutant only bites if the plan actually named paths to drop")

	full := newFixture(t)
	inc.git(t, inc.work, "push", "--force", full.mirror, "main:main")
	runIngest(t, full, ingest.KindFull)

	got := readDerivedState(t, inc.repoID)
	want := readDerivedState(t, full.repoID)
	diffs := want.differences(got)
	require.NotEmpty(t, diffs,
		"a skipped incremental drop went UNDETECTED: the incremental-equals-full property cannot fail, so it proves nothing")
	assert.Contains(t, diffs, "symbols", "the dropped-but-kept file's symbols must show up as a divergence")
	assert.Contains(t, diffs, "symbol_references", "the dropped-but-kept file's references must show up as a divergence")
	assert.Contains(t, diffs, "chunks", "the dropped-but-kept file's chunks must show up as a divergence")
	assert.Contains(t, diffs, "graph_edges", "edges resolved into the stale symbols must show up as a divergence")
}

// TestDerivedStateComparison_DetectsACorruptedSide proves, field by field,
// that the comparison is not vacuous. Two repos are ingested from the
// identical tree -- which must compare EQUAL, itself a determinism check on
// the whole pipeline and on the C-collation ordering the snapshot relies on
// -- and then exactly one row on one side is deleted or edited per subtest.
// Every corruption must be caught and attributed to the right table.
//
// The line-column cases are the reason this test enumerates fields rather
// than just deleting rows: a row DELETE is caught by length alone, so it
// cannot tell a real field comparison apart from one that silently ignores
// the column. The mutated-line and nulled-line cases can only pass if the
// comparison genuinely reads symbols.line.
func TestDerivedStateComparison_DetectsACorruptedSide(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		table     string
		corrupt   string
		alsoNames []string
	}{
		{
			name:    "a deleted symbols row",
			table:   "symbols",
			corrupt: `DELETE FROM symbols WHERE id = (SELECT id FROM symbols WHERE repo_id = $1 AND kind = 'function' ORDER BY file, name LIMIT 1)`,
			// graph_edges references symbols ON DELETE CASCADE, so
			// removing a symbol necessarily removes its edges too.
			alsoNames: []string{"graph_edges"},
		},
		{
			name:    "a deleted symbol_references row",
			table:   "symbol_references",
			corrupt: `DELETE FROM symbol_references WHERE id = (SELECT id FROM symbol_references WHERE repo_id = $1 ORDER BY file, line LIMIT 1)`,
		},
		{
			name:    "a deleted graph_edges row",
			table:   "graph_edges",
			corrupt: `DELETE FROM graph_edges WHERE id = (SELECT id FROM graph_edges WHERE repo_id = $1 ORDER BY id LIMIT 1)`,
		},
		{
			name:    "a deleted chunks row",
			table:   "chunks",
			corrupt: `DELETE FROM chunks WHERE id = (SELECT id FROM chunks WHERE repo_id = $1 ORDER BY file, start_line LIMIT 1)`,
		},
		{
			name:      "a symbol whose line moved",
			table:     "symbols",
			corrupt:   `UPDATE symbols SET line = line + 1000 WHERE id = (SELECT id FROM symbols WHERE repo_id = $1 AND line IS NOT NULL ORDER BY file, name LIMIT 1)`,
			alsoNames: []string{"graph_edges"},
		},
		{
			name:      "a symbol whose line became NULL",
			table:     "symbols",
			corrupt:   `UPDATE symbols SET line = NULL WHERE id = (SELECT id FROM symbols WHERE repo_id = $1 AND line IS NOT NULL ORDER BY file, name LIMIT 1)`,
			alsoNames: []string{"graph_edges"},
		},
		{
			name:      "a symbol renamed in place",
			table:     "symbols",
			corrupt:   `UPDATE symbols SET name = name || '_tampered' WHERE id = (SELECT id FROM symbols WHERE repo_id = $1 AND kind = 'function' ORDER BY file, name LIMIT 1)`,
			alsoNames: []string{"graph_edges"},
		},
		{
			name:    "a reference whose line moved",
			table:   "symbol_references",
			corrupt: `UPDATE symbol_references SET line = line + 1000 WHERE id = (SELECT id FROM symbol_references WHERE repo_id = $1 ORDER BY file, line LIMIT 1)`,
		},
		{
			name:    "a chunk whose line span moved",
			table:   "chunks",
			corrupt: `UPDATE chunks SET end_line = end_line + 5 WHERE id = (SELECT id FROM chunks WHERE repo_id = $1 ORDER BY file, start_line LIMIT 1)`,
		},
		{
			name:    "a chunk whose content was edited",
			table:   "chunks",
			corrupt: `UPDATE chunks SET content = content || ' tampered' WHERE id = (SELECT id FROM chunks WHERE repo_id = $1 ORDER BY file, start_line LIMIT 1)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+" is caught", func(t *testing.T) {
			t.Parallel()
			a := newPolyglotFixture(t)
			runIngest(t, a, ingest.KindFull)
			b := newFixture(t)
			a.git(t, a.work, "push", "--force", b.mirror, "main:main")
			runIngest(t, b, ingest.KindFull)

			before := readDerivedState(t, a.repoID)
			require.False(t, before.empty())
			require.Empty(t, before.differences(readDerivedState(t, b.repoID)),
				"two full rebuilds of the identical tree must produce identical derived state before anything is corrupted")

			tag, err := writerPool.Exec(t.Context(), tc.corrupt, b.repoID)
			require.NoError(t, err)
			require.EqualValues(t, 1, tag.RowsAffected(), "the corruption must actually have touched a row, or this subtest proves nothing")

			diffs := before.differences(readDerivedState(t, b.repoID))
			require.NotEmpty(t, diffs, "%s went undetected: the comparison does not actually cover that column", tc.name)
			assert.Contains(t, diffs, tc.table)
			for _, also := range tc.alsoNames {
				assert.Contains(t, diffs, also)
			}
		})
	}
}

// TestDerivedStateComparison_DetectsAMutatedChunkEmbedding is the same
// non-vacuity proof for the one field the goldens deliberately do NOT
// carry. A chunk row whose content is untouched but whose vector is
// replaced must still be caught, or "modulo ids and timestamps" would
// silently have become "modulo the embedding too".
func TestDerivedStateComparison_DetectsAMutatedChunkEmbedding(t *testing.T) {
	t.Parallel()
	a := newPolyglotFixture(t)
	runIngest(t, a, ingest.KindFull)
	before := readDerivedState(t, a.repoID)
	require.Greater(t, len(before.Chunks), 1)
	require.NotEqual(t, before.Chunks[0].Embedding, before.Chunks[len(before.Chunks)-1].Embedding,
		"the swap below only corrupts anything if the donor vector actually differs from the victim's")

	tag, err := writerPool.Exec(t.Context(),
		`UPDATE chunks SET embedding = (SELECT embedding FROM chunks WHERE repo_id = $1 ORDER BY file DESC, start_line DESC LIMIT 1)
		 WHERE id = (SELECT id FROM chunks WHERE repo_id = $1 ORDER BY file, start_line LIMIT 1)`, a.repoID)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())

	after := readDerivedState(t, a.repoID)
	require.Equal(t, len(before.Chunks), len(after.Chunks), "only the vector was meant to change")
	diffs := before.differences(after)
	require.Contains(t, diffs, "chunks",
		"a chunk keeping its content but losing its own embedding went undetected: the comparison does not actually cover embeddings")
	assert.Equal(t, []string{"chunks"}, diffs, "nothing but the chunk table should have changed")
}
