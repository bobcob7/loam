package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEveryHTTPClientInThisPackageIsInstrumented is a source-level guard, and
// it exists because of the specific way loam-9v9s' first attempt was
// incomplete rather than out of a general taste for AST tests.
//
// The instrumentation seam is "wrap every *http.Client", but the clients are
// not constructed in the packages that own the APIs they call -- internal/forge
// and internal/ingest/embed/ollama both take an injected client. They are all
// constructed HERE, in the composition root. An author who finds the call
// sites by reading internal/forge therefore finds every client that package's
// tests exercise and still misses the ones main.go and sync.go build: the
// first pass wrapped five and missed buildSyncScheduler's, which drives
// GetPRState/ClosePR on every sync tick for every open work branch and is the
// highest-frequency forge traffic in the process.
//
// No behavioural test would have caught that. buildSyncScheduler has no
// assertion about its transport, and adding one to each register* function
// would prove only what each already knows to check. The invariant is a
// property of the FILE SET -- "no bare client is constructed anywhere in this
// package" -- so that is what this asserts. A seventh client added in a
// future bead fails here on the line that introduces it.
//
// The rule is deliberately syntactic and slightly over-strict: a client must
// be wrapped AT ITS CONSTRUCTION, not somewhere downstream. That rules out a
// correct-but-invisible variant where the wrap happens three calls away, on
// the grounds that the next person to add a client will copy whatever the
// nearest existing one does.
func TestEveryHTTPClientInThisPackageIsInstrumented(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)
	require.Contains(t, pkgs, "main")
	// Positions of every expression handed to a *.InstrumentHTTPClient call
	// as its first argument.
	wrapped := map[token.Pos]bool{}
	var clients []token.Pos
	for _, file := range pkgs["main"].Files {
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && len(call.Args) > 0 {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "InstrumentHTTPClient" {
					wrapped[call.Args[0].Pos()] = true
				}
			}
			if unary, ok := n.(*ast.UnaryExpr); ok && unary.Op == token.AND {
				if lit, ok := unary.X.(*ast.CompositeLit); ok && isHTTPClientType(lit.Type) {
					clients = append(clients, unary.Pos())
				}
			}
			return true
		})
	}
	require.NotEmpty(t, clients, "found no &http.Client{} in this package at all; if the wiring moved, this test is now vacuous and must be updated rather than deleted")
	for _, pos := range clients {
		at := fset.Position(pos)
		assert.True(t, wrapped[pos],
			"%s:%d constructs a bare &http.Client{}. Every outbound client in this package must be wrapped at construction "+
				"— forge.InstrumentHTTPClient for forge traffic, ollama.InstrumentHTTPClient for the embedder (loam-9v9s).",
			filepath.Base(at.Filename), at.Line)
	}
}

// isHTTPClientType reports whether a composite literal's type is http.Client.
func isHTTPClientType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Client" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}
