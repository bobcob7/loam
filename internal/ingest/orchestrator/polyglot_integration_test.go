//go:build integration

// Shared machinery for this package's two fixture-polyglot suites
// (goldens_integration_test.go and incremental_integration_test.go), both
// bead loam-li0.8:
//
//   - materializing internal/testfixture's fixture-polyglot into the
//     LOAM_DATA_DIR-shaped bare mirror integration_test.go's fixture
//     already builds, so both suites drive the FULL production
//     collaborator graph through newOrchestratorFor;
//   - a scripted worktree mutation vocabulary (add / delete / modify /
//     rename / text-becomes-binary) applied as real commits;
//   - readDerivedState, the identity-insensitive snapshot of every derived
//     table, which is the comparison both the goldens and the
//     incremental-equals-full property are expressed in.
//
// Run with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration -p 1 -count=1 ./internal/ingest/orchestrator/...
package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/diffplan"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/testfixture"
)

// --- fixture-polyglot on top of integration_test.go's fixture ---

// newPolyglotFixture is newFixture plus internal/testfixture's
// fixture-polyglot seeded into the working clone and pushed to the bare
// mirror, so the very first ingest sees the documented polyglot symbol
// graph rather than an ad-hoc two-file repo.
//
// testfixture.New pins the seed commit's author, committer, and both dates,
// so the commit this produces has the SAME sha in every materialization on
// every machine -- which is what makes "goldens for fixture-polyglot at a
// known commit" a meaningful claim rather than a slogan. seedCommit records
// that sha into the golden so a fixture content change fails the golden
// instead of silently re-baselining the derived rows underneath it.
func newPolyglotFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	_, err := testfixture.New(t.Context(), f.work)
	require.NoError(t, err, "materializing fixture-polyglot into the working clone")
	f.git(t, f.work, "push", "--force", f.mirror, "main:main")
	return f
}

// headSHA is the working clone's current main tip.
func headSHA(t *testing.T, f *fixture) string {
	t.Helper()
	return trimTrailingNewline(f.git(t, f.work, "rev-parse", "main"))
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// --- scripted mutations ---

// mutation is one scripted change to the working tree, applied and then
// committed and force-pushed by applyStep. The vocabulary deliberately
// covers exactly the five diff shapes an incremental plan has to get right:
// a file added, deleted, modified, renamed, and one that WAS text and
// BECOMES binary (loam-8uo, closed by loam-c94.12, which fixed it by
// having chunker.ChunkFiles emit a zero-unit entry for a binary file
// instead of dropping it from the batch).
//
// makeBinary's targets in these suites are all GRAMMARLESS files (.md) on
// purpose. graph.ExtractFile picks a language by EXTENSION, not by
// sniffing content, so a .go file whose bytes turn binary still parses
// into an ERROR-laden tree and still has its graph rows replaced -- the
// binary transition never affected the graph track at all, only chunks
// (loam-c94.12's own report; the residual case of the parser returning no
// tree whatsoever is loam-1z0, deliberately out of scope). Pointing
// makeBinary at a .go file would therefore be asserting a symmetry across
// the two tracks that does not exist.
type mutation struct {
	name  string
	apply func(t *testing.T, f *fixture)
}

func addFile(path, content string) mutation {
	return mutation{name: "add " + path, apply: func(t *testing.T, f *fixture) {
		t.Helper()
		writeWorktreeFile(t, f, path, []byte(content))
	}}
}

func modifyFile(path, content string) mutation {
	return mutation{name: "modify " + path, apply: func(t *testing.T, f *fixture) {
		t.Helper()
		require.FileExists(t, filepath.Join(f.work, path), "modifyFile must target a file that already exists, or it is really an add")
		writeWorktreeFile(t, f, path, []byte(content))
	}}
}

func deleteFile(path string) mutation {
	return mutation{name: "delete " + path, apply: func(t *testing.T, f *fixture) {
		t.Helper()
		require.NoError(t, os.Remove(filepath.Join(f.work, path)))
	}}
}

// renameFile moves oldPath to newPath with byte-identical content, which is
// what makes `git diff --name-status` report an R record (rename detection
// is on by default) rather than an unrelated delete+add pair.
func renameFile(oldPath, newPath string) mutation {
	return mutation{name: "rename " + oldPath + " -> " + newPath, apply: func(t *testing.T, f *fixture) {
		t.Helper()
		f.git(t, f.work, "mv", oldPath, newPath)
	}}
}

// makeBinary overwrites an existing text file with NUL-containing bytes.
// The path still EXISTS at the new ref, so it lands in the plan's reparse
// set and never in DropFiles -- only the chunker emitting a zero-unit entry
// for it can drop its stale chunks.
func makeBinary(path string) mutation {
	return mutation{name: "make binary " + path, apply: func(t *testing.T, f *fixture) {
		t.Helper()
		require.FileExists(t, filepath.Join(f.work, path))
		writeWorktreeFile(t, f, path, []byte{0x00, 0x01, 0x02, 'n', 'o', 'w', 0x00, 0xff, 0xfe})
	}}
}

func writeWorktreeFile(t *testing.T, f *fixture, path string, content []byte) {
	t.Helper()
	full := filepath.Join(f.work, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, content, 0o644))
}

// applyStep applies every mutation in one step and commits the result as a
// single new tip, force-pushed into the bare mirror -- the same shape
// internal/mirrorsync's forced fetch produces.
func applyStep(t *testing.T, f *fixture, message string, mutations []mutation) {
	t.Helper()
	for _, m := range mutations {
		m.apply(t, f)
	}
	f.git(t, f.work, "add", "-A")
	f.git(t, f.work, "commit", "-m", message)
	f.git(t, f.work, "push", "--force", f.mirror, "main:main")
}

// --- planner and dropper seams, for non-vacuity and for mutation testing ---

// recordingPlanner wraps the real diffplan.Planner and remembers every Plan
// it produced, so a test can assert the SECOND ingest was genuinely
// incremental. Without this the incremental-equals-full property could pass
// because both sides silently ran a full rebuild -- the exact vacuity mode
// loam-4q2 warns about. Not goroutine-safe, and does not need to be: each
// instance backs one synchronous Run driven by the test itself.
type recordingPlanner struct {
	inner planner
	plans []diffplan.Plan
}

func (p *recordingPlanner) Plan(ctx context.Context, mirrorDir string, req diffplan.Request) (diffplan.Plan, error) {
	plan, err := p.inner.Plan(ctx, mirrorDir, req)
	p.plans = append(p.plans, plan)
	return plan, err
}

func (p *recordingPlanner) last(t *testing.T) diffplan.Plan {
	t.Helper()
	require.NotEmpty(t, p.plans, "the orchestrator never called the planner")
	return p.plans[len(p.plans)-1]
}

// runIngest drives one ingest synchronously on the real transactor and
// returns the plan the planner finalized, so callers can assert the KIND
// they think they exercised is the kind that actually ran.
//
// Driving the orchestrator directly, rather than racing internal/ingest's
// worker pool, is deliberate (loam-4q2): every observation these suites
// make is of a settled state after a Run that has already returned, so
// there is no value that cycles underneath the assertion and no window to
// sample unluckily.
func runIngest(t *testing.T, f *fixture, kind ingest.Kind) (ingest.Stats, diffplan.Plan) {
	t.Helper()
	return runIngestWith(t, f, kind, nil)
}

// runIngestWith is runIngest with a seam for the mutation tests: tweak sees
// the fully-wired production orchestrator and may swap one collaborator for
// a deliberately broken one before Run is called.
func runIngestWith(t *testing.T, f *fixture, kind ingest.Kind, tweak func(*Orchestrator)) (ingest.Stats, diffplan.Plan) {
	t.Helper()
	orch := newOrchestratorFor(t, f, realTransactor())
	rec := &recordingPlanner{inner: orch.planner}
	orch.planner = rec
	if tweak != nil {
		tweak(orch)
	}
	stats, err := orch.Run(t.Context(), f.job(kind))
	require.NoError(t, err)
	return stats, rec.last(t)
}

// pathDropSkippingDropper is the MUTANT used to prove the
// incremental-equals-full property is falsifiable: it forwards the
// full-rebuild drop untouched but silently does nothing for the
// incremental per-path drop, which is exactly the bug the property exists
// to catch (a deleted or renamed-away file keeping its symbols, references
// and chunks forever).
type pathDropSkippingDropper struct{ inner dropper }

func (d pathDropSkippingDropper) DropRepoBranch(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string) error {
	return d.inner.DropRepoBranch(ctx, tx, repoID, targetBranch)
}

func (d pathDropSkippingDropper) DropPaths(context.Context, pgx.Tx, uuid.UUID, string, []string) error {
	return nil
}

// --- the identity-insensitive derived-state snapshot ---
//
// WHAT IS COMPARED, AND WHY THE REST IS NOT.
//
// Compared: every column of symbols, symbol_references, graph_edges and
// chunks that is DERIVED FROM THE SOURCE TREE -- file, line, name, kind,
// chunk boundaries, chunk content, and the chunk's embedding.
//
// Deliberately NOT compared, each for a specific reason rather than for
// convenience:
//
//   - symbols.id, symbol_references.id, graph_edges.id, chunks.id --
//     every one is a fresh uuid.NewV7() minted per INSERT by
//     codegraph.ReplaceFileSymbols / ReplaceFileReferences /
//     RecomputeGraphEdges and chunkstore.ReplaceFileChunks. Re-ingesting
//     byte-identical content changes all of them, so comparing ids would
//     make the property fail for every input including the ones where
//     incremental and full agree perfectly. graph_edges is compared by
//     RESOLVING its two symbol-id foreign keys back to the referenced
//     symbols' (file, line, name, kind) for the same reason.
//   - chunks.created_at -- DEFAULT now() (0002_code_intel.up.sql), so it
//     is a wall-clock timestamp of the ingest, not a property of the
//     content.
//   - repo_id and target_branch -- the two sides of the property are two
//     DIFFERENT repos by construction (that is what makes the full
//     rebuild a genuinely independent build rather than a re-run over the
//     same rows), so these differ on purpose.
//   - symbol_history -- nothing writes it yet (loam-c94.7 is open; see
//     orchestrator.go's writeSwap comment), so there is no derived state
//     there to compare. When that lands it belongs in this snapshot.
//
// Embeddings are compared EXACTLY, as the pgvector text rendering of the
// stored vector. That is only legitimate because these suites run against
// internal/testembed, whose projection is a pure function of the chunk
// text. A live Ollama nomic-embed-text is deterministic in practice but
// not contractually, so a variant of this comparison running against a
// real embedder would have to compare vectors within a tolerance instead.
// There is deliberately no such path here: every ingest in this package's
// integration suite goes through testembed (newOrchestratorFor).

// nullableLine is symbols.line, which is NULL for the file-level "module"
// symbol every parsed file gets (docs/persistence-spec.md: "line (null for
// file-level)").
//
// It is a VALUE type rather than the *int32 pgx would scan into directly,
// and that is load-bearing: derivedState's rows are compared with ==, and a
// pointer field compares ADDRESSES. Two independently-read snapshots never
// share an address, so a *int32 here would make every comparison report a
// difference; worse, the obvious "fix" of ignoring the field would make
// every line change invisible. Neither failure mode is acceptable in the
// one comparison this bead exists to make trustworthy, so the nullability
// is modeled as a comparable value instead. The two
// TestDerivedStateComparison_DetectsACorruptedSide cases that mutate a
// symbol's line -- to another number, and to NULL -- exist to keep it that
// way.
type nullableLine struct {
	Valid bool
	Value int32
}

func lineOf(p *int32) nullableLine {
	if p == nil {
		return nullableLine{}
	}
	return nullableLine{Valid: true, Value: *p}
}

// MarshalJSON renders an absent line as JSON null and a present one as a
// bare number, so a golden reads exactly as the column does.
func (n nullableLine) MarshalJSON() ([]byte, error) {
	if !n.Valid {
		return []byte("null"), nil
	}
	return strconv.AppendInt(nil, int64(n.Value), 10), nil
}

type symbolRow struct {
	File string       `json:"file"`
	Line nullableLine `json:"line"`
	Name string       `json:"name"`
	Kind string       `json:"kind"`
}

type referenceRow struct {
	File string `json:"file"`
	Line int32  `json:"line"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// edgeRow is one graph_edges row with both symbol-id foreign keys resolved
// to the identity-insensitive description of the symbol they point at.
type edgeRow struct {
	FromFile string       `json:"from_file"`
	FromLine nullableLine `json:"from_line"`
	FromName string       `json:"from_name"`
	FromKind string       `json:"from_kind"`
	ToFile   string       `json:"to_file"`
	ToLine   nullableLine `json:"to_line"`
	ToName   string       `json:"to_name"`
	ToKind   string       `json:"to_kind"`
	Kind     string       `json:"kind"`
}

type chunkRow struct {
	File      string `json:"file"`
	StartLine int32  `json:"start_line"`
	EndLine   int32  `json:"end_line"`
	Content   string `json:"content"`
	// Embedding is the pgvector text rendering of the stored vector. It is
	// json:"-" so the goldens stay readable -- 768 floats per chunk would
	// bury the content a human is meant to review -- but it IS part of the
	// struct, so the incremental-equals-full comparison covers it, and the
	// goldens suite separately re-derives every stored vector from
	// testembed and asserts equality.
	Embedding string `json:"-"`
}

// derivedState is one repo+branch's entire derived index, in a form where
// two independently-built indexes over the same tree must be equal.
type derivedState struct {
	Symbols    []symbolRow    `json:"symbols"`
	References []referenceRow `json:"symbol_references"`
	Edges      []edgeRow      `json:"graph_edges"`
	Chunks     []chunkRow     `json:"chunks"`
}

// empty reports whether nothing at all was indexed -- the vacuity guard
// every caller of readDerivedState checks before comparing two of them.
func (s derivedState) empty() bool {
	return len(s.Symbols) == 0 && len(s.References) == 0 && len(s.Edges) == 0 && len(s.Chunks) == 0
}

// differences names every table in which a and b disagree. It exists
// alongside the per-table assert.Equal the property test uses because the
// meta-tests (which prove this comparison can FAIL) need the comparison as
// a value rather than as an assertion.
func (s derivedState) differences(other derivedState) []string {
	var diffs []string
	if !equalSlices(s.Symbols, other.Symbols) {
		diffs = append(diffs, "symbols")
	}
	if !equalSlices(s.References, other.References) {
		diffs = append(diffs, "symbol_references")
	}
	if !equalSlices(s.Edges, other.Edges) {
		diffs = append(diffs, "graph_edges")
	}
	if !equalSlices(s.Chunks, other.Chunks) {
		diffs = append(diffs, "chunks")
	}
	return diffs
}

func equalSlices[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readDerivedState snapshots every derived table for repoID on branch main,
// on readerPool (never the writer), in a total order that is a function of
// the compared columns alone -- so two equal indexes always produce equal
// slices regardless of insertion order, and unequal ones always produce
// unequal slices rather than merely differently-ordered ones.
func readDerivedState(t *testing.T, repoID uuid.UUID) derivedState {
	t.Helper()
	return derivedState{
		Symbols:    readSymbolRows(t, repoID),
		References: readReferenceRows(t, repoID),
		Edges:      readEdgeRows(t, repoID),
		Chunks:     readChunkRows(t, repoID),
	}
}

func readSymbolRows(t *testing.T, repoID uuid.UUID) []symbolRow {
	t.Helper()
	rows, err := readerPool.Query(t.Context(),
		`SELECT file, line, name, kind FROM symbols
		 WHERE repo_id = $1 AND target_branch = 'main'
		 ORDER BY file COLLATE "C", name COLLATE "C", kind COLLATE "C", line NULLS FIRST`, repoID)
	require.NoError(t, err)
	defer rows.Close()
	out := []symbolRow{}
	for rows.Next() {
		var (
			r    symbolRow
			line *int32
		)
		require.NoError(t, rows.Scan(&r.File, &line, &r.Name, &r.Kind))
		r.Line = lineOf(line)
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func readReferenceRows(t *testing.T, repoID uuid.UUID) []referenceRow {
	t.Helper()
	rows, err := readerPool.Query(t.Context(),
		`SELECT file, line, name, kind FROM symbol_references
		 WHERE repo_id = $1 AND target_branch = 'main'
		 ORDER BY file COLLATE "C", line, name COLLATE "C", kind COLLATE "C"`, repoID)
	require.NoError(t, err)
	defer rows.Close()
	out := []referenceRow{}
	for rows.Next() {
		var r referenceRow
		require.NoError(t, rows.Scan(&r.File, &r.Line, &r.Name, &r.Kind))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func readEdgeRows(t *testing.T, repoID uuid.UUID) []edgeRow {
	t.Helper()
	rows, err := readerPool.Query(t.Context(),
		`SELECT f.file, f.line, f.name, f.kind, tsym.file, tsym.line, tsym.name, tsym.kind, e.kind
		 FROM graph_edges e
		 JOIN symbols f ON f.id = e.from_symbol_id
		 JOIN symbols tsym ON tsym.id = e.to_symbol_id
		 WHERE e.repo_id = $1 AND e.target_branch = 'main'
		 ORDER BY f.file COLLATE "C", f.name COLLATE "C", f.line NULLS FIRST,
		          tsym.file COLLATE "C", tsym.name COLLATE "C", tsym.line NULLS FIRST, e.kind COLLATE "C"`, repoID)
	require.NoError(t, err)
	defer rows.Close()
	out := []edgeRow{}
	for rows.Next() {
		var (
			r                edgeRow
			fromLine, toLine *int32
		)
		require.NoError(t, rows.Scan(&r.FromFile, &fromLine, &r.FromName, &r.FromKind, &r.ToFile, &toLine, &r.ToName, &r.ToKind, &r.Kind))
		r.FromLine, r.ToLine = lineOf(fromLine), lineOf(toLine)
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func readChunkRows(t *testing.T, repoID uuid.UUID) []chunkRow {
	t.Helper()
	rows, err := readerPool.Query(t.Context(),
		`SELECT file, start_line, end_line, content, embedding::text FROM chunks
		 WHERE repo_id = $1 AND target_branch = 'main'
		 ORDER BY file COLLATE "C", start_line, end_line, content COLLATE "C"`, repoID)
	require.NoError(t, err)
	defer rows.Close()
	out := []chunkRow{}
	for rows.Next() {
		var r chunkRow
		require.NoError(t, rows.Scan(&r.File, &r.StartLine, &r.EndLine, &r.Content, &r.Embedding))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// distinctFiles is the set of files any symbol row names, used by the
// grammarless-file assertions.
func distinctFiles(rows []symbolRow) []string {
	seen := map[string]struct{}{}
	for _, r := range rows {
		seen[r.File] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
