//go:build integration

// See internal/db/migrations/integration_test.go's header for the
// podman/ryuk workaround note; it applies equally here. Run explicitly
// with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/ingest/graph/... -v
package graph

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/codegraph"
	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/parser"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/testfixture"
)

// sharedPool mirrors internal/codegraph/integration_test.go's own
// sharedPool: one migrated pgvector Postgres container for the whole test
// binary, per that file's documented fix for concurrent container starts
// blowing the test timeout. Every test below scopes its rows to its own
// freshly generated repoID.
var sharedPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting shared pgvector container:", err)
		os.Exit(1)
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolving shared container DSN:", err)
		os.Exit(1)
	}
	if err := migrations.Migrate(ctx, dsn, logger); err != nil {
		fmt.Fprintln(os.Stderr, "migrating shared container:", err)
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "opening shared pool:", err)
		os.Exit(1)
	}
	sharedPool = pool
	code := m.Run()
	pool.Close()
	if err := container.Terminate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "terminating shared pgvector container:", err)
	}
	os.Exit(code)
}

// newIntegrationExtractor builds a real, cgo-backed Extractor -- no mocks
// anywhere in this file, since the whole point of an integration test is
// proving the real Tree-sitter grammars and the real database agree with
// each other through this package's actual call path.
func newIntegrationExtractor(t *testing.T) *Extractor {
	t.Helper()
	p := parser.NewParser(testLogger())
	t.Cleanup(p.Close)
	e, err := New(p, testLogger())
	require.NoError(t, err)
	t.Cleanup(e.Close)
	return e
}

// newIntegrationRepo seeds a repos row this test alone owns and returns a
// codegraph.Store wired over the shared pool -- codegraph.Store implements
// this package's own store interface structurally, exactly as production
// wiring will (this bead's whole point is writing through the real store,
// not a stand-in).
func newIntegrationRepo(t *testing.T) (*codegraph.Store, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	repoID := uuid.Must(uuid.NewV7())
	_, err := sharedPool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, 'https://example.com/repo.git', 'example.com', 'main')`,
		repoID, "group/graph-"+repoID.String(),
	)
	require.NoError(t, err)
	return codegraph.New(gen.New(sharedPool), testLogger()), repoID
}

func readFile(t *testing.T, dir, rel string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, rel))
	require.NoError(t, err)
	return content
}

// symbolNames/referenceNames query the real tables directly (bypassing this
// package and codegraph.Store entirely) so the golden assertions below
// verify what is actually persisted, not merely what IngestFiles returned
// in memory.
func symbolNames(t *testing.T, repoID uuid.UUID, file string) []string {
	t.Helper()
	rows, err := sharedPool.Query(t.Context(), `SELECT name FROM symbols WHERE repo_id = $1 AND file = $2 ORDER BY name`, repoID, file)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	return names
}

func referenceNames(t *testing.T, repoID uuid.UUID, file string) []string {
	t.Helper()
	rows, err := sharedPool.Query(t.Context(), `SELECT name FROM symbol_references WHERE repo_id = $1 AND file = $2 ORDER BY name`, repoID, file)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	return names
}

// TestIngestFiles_FixturePolyglot_GoldenSymbolsAndReferences is the golden
// test the brief asks for: it runs IngestFiles over every file
// fixture-polyglot's own doc comment (internal/testfixture/fixture.go)
// describes as meaningful to the symbol graph, against a real Postgres
// database, and asserts the exact persisted rows -- proving this package's
// extraction and persistence agree with the real grammars and the real
// schema end to end, not just in an in-memory FileResult.
func TestIngestFiles_FixturePolyglot_GoldenSymbolsAndReferences(t *testing.T) {
	t.Parallel()
	repo := testfixture.NewT(t.Context(), t)
	e := newIntegrationExtractor(t)
	store, repoID := newIntegrationRepo(t)

	files := []FileInput{
		{Path: "pkg/validate/validate.go", Content: readFile(t, repo.Dir(), "pkg/validate/validate.go")},
		{Path: "pkg/report/report.go", Content: readFile(t, repo.Dir(), "pkg/report/report.go")},
		{Path: "src/validate.ts", Content: readFile(t, repo.Dir(), "src/validate.ts")},
		{Path: "src/index.ts", Content: readFile(t, repo.Dir(), "src/index.ts")},
		{Path: "scripts/parity.py", Content: readFile(t, repo.Dir(), "scripts/parity.py")},
		{Path: "docs/OVERVIEW.md", Content: readFile(t, repo.Dir(), "docs/OVERVIEW.md")},
	}
	stats, err := e.IngestFiles(t.Context(), store, repoID, "main", files)
	require.NoError(t, err)
	assert.Equal(t, 5, stats.FilesExtracted, "every file except docs/OVERVIEW.md has a registered grammar")
	assert.Equal(t, 1, stats.FilesSkippedUnsupportedLanguage, "docs/OVERVIEW.md (.md) has no registered grammar")
	assert.Equal(t, 0, stats.FilesFailed)
	assert.Equal(t, 0, stats.FilesWithSyntaxErrors, "the seed fixture is clean, valid source")

	assert.Equal(t, []string{"validate", "Validate"}, symbolNames(t, repoID, "pkg/validate/validate.go"), "the module symbol 'validate' plus the Validate function")
	assert.Equal(t, []string{"TrimSpace"}, referenceNames(t, repoID, "pkg/validate/validate.go"), "validate.go's own reference is the stdlib call strings.TrimSpace, captured via the selector_expression call pattern")

	assert.Equal(t, []string{"report", "Summarize"}, symbolNames(t, repoID, "pkg/report/report.go"))
	assert.Equal(t, []string{"Validate"}, referenceNames(t, repoID, "pkg/report/report.go"), "report.go calls validate.Validate")

	assert.Equal(t, []string{"validate", "Validate"}, symbolNames(t, repoID, "src/validate.ts"), "the module symbol 'validate' plus the Validate function")
	assert.Equal(t, []string{"trim"}, referenceNames(t, repoID, "src/validate.ts"), "value.trim() is captured via the member_expression call pattern")

	assert.Equal(t, []string{"index", "summarize"}, symbolNames(t, repoID, "src/index.ts"))
	assert.Equal(t, []string{"Validate"}, referenceNames(t, repoID, "src/index.ts"), "index.ts calls Validate directly")

	assert.Equal(t, []string{"is_even", "is_odd", "parity"}, symbolNames(t, repoID, "scripts/parity.py"))
	assert.ElementsMatch(t, []string{"is_even", "is_odd"}, referenceNames(t, repoID, "scripts/parity.py"), "is_even and is_odd call each other")

	assert.Empty(t, symbolNames(t, repoID, "docs/OVERVIEW.md"), "an unsupported-language file must never reach the store")
	assert.Empty(t, referenceNames(t, repoID, "docs/OVERVIEW.md"))
}

// TestIngestFiles_Reparse_DropsStaleSymbolsAndReferences is MUTATION 3's
// kill switch, proved end to end against a real database through THIS
// package's actual call path (not codegraph's own already-covered unit
// test of ReplaceFileSymbols in isolation): reparsing a file whose content
// changed must leave no trace of the OLD symbols/references behind. If the
// delete half of delete-and-replace were ever dropped -- from
// codegraph.Store, or from this package accidentally routing around it --
// this test fails with the old "Validate"/"TrimSpace" rows still present
// alongside the new ones.
func TestIngestFiles_Reparse_DropsStaleSymbolsAndReferences(t *testing.T) {
	t.Parallel()
	repo := testfixture.NewT(t.Context(), t)
	e := newIntegrationExtractor(t)
	store, repoID := newIntegrationRepo(t)
	const path = "pkg/validate/validate.go"

	original := readFile(t, repo.Dir(), path)
	_, err := e.IngestFiles(t.Context(), store, repoID, "main", []FileInput{{Path: path, Content: original}})
	require.NoError(t, err)
	require.Equal(t, []string{"validate", "Validate"}, symbolNames(t, repoID, path))

	renamed := []byte("package validate\n\nfunc Check(s string) bool {\n\treturn len(s) > 0\n}\n")
	_, err = e.IngestFiles(t.Context(), store, repoID, "main", []FileInput{{Path: path, Content: renamed}})
	require.NoError(t, err)

	names := symbolNames(t, repoID, path)
	assert.NotContains(t, names, "Validate", "the OLD function symbol must be gone after reparse, not accumulated alongside the new one")
	assert.Contains(t, names, "Check", "the NEW function symbol must be present")
	assert.Equal(t, []string{"Check", "validate"}, names, "exactly the current parse's symbols must remain -- nothing stale, nothing missing")

	var count int
	require.NoError(t, sharedPool.QueryRow(t.Context(), `SELECT count(*) FROM symbols WHERE repo_id = $1 AND file = $2`, repoID, path).Scan(&count))
	assert.Equal(t, 2, count, "exactly module+Check, not 3+ rows from an accumulated stale insert")
}

// edgeRow is one graph_edges row joined back to both endpoints' (file, name)
// identity -- the shape TestIngestFiles_FixturePolyglot_GraphEdgesResolved
// asserts against, since a bare symbol-id pair is meaningless to a reader of
// test output.
type edgeRow struct {
	fromFile, fromName string
	toFile, toName     string
}

// graphEdges queries every graph_edges row for repoID, joined back to the
// endpoints' (file, name) identity, ordered for deterministic comparison.
func graphEdges(t *testing.T, repoID uuid.UUID) []edgeRow {
	t.Helper()
	rows, err := sharedPool.Query(t.Context(), `
		SELECT sf.file, sf.name, st.file, st.name
		FROM graph_edges ge
		JOIN symbols sf ON sf.id = ge.from_symbol_id
		JOIN symbols st ON st.id = ge.to_symbol_id
		WHERE ge.repo_id = $1
		ORDER BY sf.file, sf.name, st.file, st.name`, repoID)
	require.NoError(t, err)
	defer rows.Close()
	var edges []edgeRow
	for rows.Next() {
		var e edgeRow
		require.NoError(t, rows.Scan(&e.fromFile, &e.fromName, &e.toFile, &e.toName))
		edges = append(edges, e)
	}
	require.NoError(t, rows.Err())
	return edges
}

// TestIngestFiles_FixturePolyglot_GraphEdgesResolvedEndToEnd is loam-c94.6's
// central proof: running the real IngestFiles pipeline (real Tree-sitter
// extraction, real codegraph.Store, real Postgres) over fixture-polyglot's
// full file set must leave graph_edges holding exactly the edges the
// fixture's own doc comments describe, resolved by RecomputeGraphEdges after
// every file's symbols/references land -- not merely stats reported in
// memory (loam-c94.5's golden test already covers that half).
//
// fixture-polyglot's "Validate" is deliberately ambiguous by design
// (pkg/validate/validate.go's and src/validate.ts's own doc comments): a Go
// export and a TypeScript export share the name. Edge resolution must NOT
// merge them -- report.go (Go) reaches only the Go Validate and index.ts
// (TS) only the TypeScript one, which is precisely what internal/testfixture's
// package doc says the pair exists to prove. Until loam-w5g this test
// asserted the opposite, expecting each caller to fan out to BOTH
// definitions and citing docs/cli-spec.md's "ambiguous target is data, not
// an error"; that rule governs ambiguity WITHIN a language (two same-named
// Go symbols are both real candidates), not across a language boundary,
// where a Go function cannot call a TypeScript one at all. This test and
// the orchestrator golden were the two places that had the leak pinned as
// expected output.
// is_even/is_odd's mutual recursion (scripts/parity.py) proves a same-file
// cycle resolves in both directions. validate.go's TrimSpace reference and
// validate.ts's trim reference are stdlib/built-in calls with no matching
// symbols row anywhere in the repo -- docs/cli-spec.md's "MVP does not
// resolve cross-repo/third-party edges" -- and must produce NO edge at all,
// proved by their total absence from the result set, not a NULL-ended row.
func TestIngestFiles_FixturePolyglot_GraphEdgesResolvedEndToEnd(t *testing.T) {
	t.Parallel()
	repo := testfixture.NewT(t.Context(), t)
	e := newIntegrationExtractor(t)
	store, repoID := newIntegrationRepo(t)

	files := []FileInput{
		{Path: "pkg/validate/validate.go", Content: readFile(t, repo.Dir(), "pkg/validate/validate.go")},
		{Path: "pkg/report/report.go", Content: readFile(t, repo.Dir(), "pkg/report/report.go")},
		{Path: "src/validate.ts", Content: readFile(t, repo.Dir(), "src/validate.ts")},
		{Path: "src/index.ts", Content: readFile(t, repo.Dir(), "src/index.ts")},
		{Path: "scripts/parity.py", Content: readFile(t, repo.Dir(), "scripts/parity.py")},
		{Path: "docs/OVERVIEW.md", Content: readFile(t, repo.Dir(), "docs/OVERVIEW.md")},
	}
	stats, err := e.IngestFiles(t.Context(), store, repoID, "main", files)
	require.NoError(t, err)

	want := []edgeRow{
		{fromFile: "pkg/report/report.go", fromName: "Summarize", toFile: "pkg/validate/validate.go", toName: "Validate"},
		{fromFile: "scripts/parity.py", fromName: "is_even", toFile: "scripts/parity.py", toName: "is_odd"},
		{fromFile: "scripts/parity.py", fromName: "is_odd", toFile: "scripts/parity.py", toName: "is_even"},
		{fromFile: "src/index.ts", fromName: "summarize", toFile: "src/validate.ts", toName: "Validate"},
	}
	assert.Equal(t, int64(len(want)), stats.EdgesRecomputed, "Stats.EdgesRecomputed must report the exact edge count RecomputeGraphEdges inserted")
	got := graphEdges(t, repoID)
	assert.Equal(t, want, got, "graph_edges must hold exactly these 4 rows: each Validate caller resolving to its OWN language's definition, is_even/is_odd's mutual recursion in both directions, and nothing for TrimSpace/trim")

	for _, edge := range got {
		assert.NotEqual(t, "TrimSpace", edge.toName, "TrimSpace is a stdlib call with no matching symbols row -- it must never appear as an edge target")
		assert.NotEqual(t, "trim", edge.toName, "trim is a built-in method call with no matching symbols row -- it must never appear as an edge target")
		crossed := strings.HasSuffix(edge.fromFile, ".go") && strings.HasSuffix(edge.toFile, ".ts") ||
			strings.HasSuffix(edge.fromFile, ".ts") && strings.HasSuffix(edge.toFile, ".go")
		assert.False(t, crossed, "no edge may cross a language boundary (loam-w5g): %s %s -> %s %s", edge.fromFile, edge.fromName, edge.toFile, edge.toName)
	}

	// Re-running IngestFiles over the identical file set (as a second ingest
	// of an unchanged tree would) must leave graph_edges at exactly the same
	// 6 rows, not 12 -- RecomputeGraphEdges' delete-then-rebuild contract
	// applied through this package's actual call path, not just
	// internal/codegraph's own already-covered unit test of it in isolation.
	statsAgain, err := e.IngestFiles(t.Context(), store, repoID, "main", files)
	require.NoError(t, err)
	assert.Equal(t, int64(len(want)), statsAgain.EdgesRecomputed)
	assert.Equal(t, want, graphEdges(t, repoID), "recomputing edges for an unchanged tree must not accumulate duplicate rows")
}

// TestIngestFiles_UnsupportedLanguageFile_NeverWritesRows corroborates the
// in-memory Stats assertion in the golden test above against the real
// table directly: a .md file must leave zero symbols/symbol_references
// rows, not merely report a skip count that could theoretically diverge
// from what was actually persisted.
func TestIngestFiles_UnsupportedLanguageFile_NeverWritesRows(t *testing.T) {
	t.Parallel()
	e := newIntegrationExtractor(t)
	store, repoID := newIntegrationRepo(t)
	_, err := e.IngestFiles(t.Context(), store, repoID, "main", []FileInput{
		{Path: "README.md", Content: []byte("# hello\n")},
	})
	require.NoError(t, err)
	var symbolCount, refCount int
	require.NoError(t, sharedPool.QueryRow(t.Context(), `SELECT count(*) FROM symbols WHERE repo_id = $1`, repoID).Scan(&symbolCount))
	require.NoError(t, sharedPool.QueryRow(t.Context(), `SELECT count(*) FROM symbol_references WHERE repo_id = $1`, repoID).Scan(&refCount))
	assert.Zero(t, symbolCount)
	assert.Zero(t, refCount)
}
