package chunker

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/parser"
	"github.com/bobcob7/loam/internal/testfixture"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// readFixtureFile materializes a fresh copy of fixture-polyglot (internal/
// testfixture, loam-li0.4) and reads rel from it, exactly mirroring
// internal/ingest/graph/extract_test.go's own helper of the same name, so
// this package's golden tests exercise the same real source loam-c94.5's
// golden tests already pin, rather than a hand-copied snippet that could
// silently drift from it.
func readFixtureFile(t *testing.T, rel string) []byte {
	t.Helper()
	repo := testfixture.NewT(t.Context(), t)
	content, err := os.ReadFile(filepath.Join(repo.Dir(), rel))
	require.NoError(t, err)
	return content
}

// newRealChunker builds a Chunker over a real *parser.Parser, for tests
// that want genuine Tree-sitter output.
func newRealChunker(t *testing.T) *Chunker {
	t.Helper()
	p := parser.NewParser(testLogger())
	t.Cleanup(p.Close)
	return NewChunker(p, testLogger())
}

// fixedBudgeter returns a BudgeterMock reporting a constant context window.
func fixedBudgeter(window int) *BudgeterMock {
	return &BudgeterMock{ContextWindowFunc: func() int { return window }}
}

// assertNoBlankUnits fails t if any unit's content is empty or
// whitespace-only -- the property every strategy in this package promises
// (unitForLines is the single enforcement point; see its doc comment) and
// that EnforceBudget's own fast path (content already under budget) does
// not re-check on units it did not itself split, so this package must get
// it right before ever calling EnforceBudget.
func assertNoBlankUnits(t *testing.T, units []chunk.Unit) {
	t.Helper()
	for i, u := range units {
		require.NotEmptyf(t, strings.TrimSpace(u.Content), "unit %d (lines %d-%d) must not be blank", i, u.StartLine, u.EndLine)
	}
}
