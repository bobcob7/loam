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
