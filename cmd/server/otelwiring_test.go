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

// implicitDefaultClientFuncs are the net/http package-level helpers that use
// http.DefaultClient without ever naming it. They are untraceable by
// construction, so they are banned outright rather than checked for wrapping.
var implicitDefaultClientFuncs = map[string]bool{
	"Get": true, "Post": true, "Head": true, "PostForm": true,
}

// TestEveryHTTPClientInThisPackageIsInstrumented is a source-level guard, and
// it exists because of the specific way loam-9v9s' first attempt was
// incomplete rather than out of a general taste for AST tests.
//
// The instrumentation seam is "wrap every *http.Client", but the clients are
// not constructed in the packages that own the APIs they call --
// internal/forge and internal/ingest/embed/ollama both take an injected
// client. They are all constructed HERE, in the composition root. An author
// who finds the call sites by reading internal/forge therefore finds every
// client that package's tests exercise and still misses the ones main.go and
// sync.go build: the first pass wrapped five and missed buildSyncScheduler's,
// which drives GetPRState/ClosePR on every sync tick for every open work
// branch and is the highest-frequency forge traffic in the process.
//
// No behavioural test would have caught that. buildSyncScheduler has no
// assertion about its transport, and adding one to each register* function
// would prove only what each already knows to check. The invariant is a
// property of the FILE SET -- "no bare client is constructed anywhere in this
// package" -- so that is what this asserts. A seventh client added in a
// future bead fails here on the line that introduces it.
//
// # WHAT IT MATCHES, AND WHY IT IS FOUR FORMS AND NOT ONE
//
// The first version of this test matched only &http.Client{}, and its doc
// comment claimed the full invariant anyway. It did not hold:
// new(http.Client) shipped an uninstrumented forge client with the suite
// green, and so did http.DefaultClient and a var declaration. The vacuity
// guard below does not help there, because the other five sites still match.
// A guard whose comment promises more than its matcher delivers is WORSE
// than no guard, because it stops the next person from looking. Each of the
// four forms below has been mutated individually and confirmed to fail.
//
// Two of them cannot be wrapped in place and are therefore banned outright
// rather than checked: a `var c http.Client` value has no pointer to wrap at
// its declaration, and http.Get/Post/Head/PostForm reach
// http.DefaultClient without naming it.
//
// # ON STRICTNESS
//
// The rule is syntactic: a client must be wrapped AT ITS CONSTRUCTION, not
// somewhere downstream. That is deliberate -- the next person to add a client
// copies whatever the nearest existing one does, so a correct-but-distant
// wrap teaches the wrong pattern.
//
// It is NOT as restrictive as it first looks, and the obvious objection was
// tested rather than argued: the DRY refactor someone would actually want --
// collapsing the repeated forge.InstrumentHTTPClient(&http.Client{},
// cfg.TracerProvider) sites into a local helper -- PASSES, because the helper
// still wraps at construction and every call site then goes through it. What
// fails is only the thing that should: a client built here and wrapped
// somewhere else, or not at all.
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
	// Composite literals that appear as &http.Client{...}, so the bare-value
	// check below does not double-report them.
	addressed := map[token.Pos]bool{}
	var clients []token.Pos
	type ban struct {
		pos    token.Pos
		reason string
	}
	var banned []ban
	for _, file := range pkgs["main"].Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if sel.Sel.Name == "InstrumentHTTPClient" && len(x.Args) > 0 {
						wrapped[x.Args[0].Pos()] = true
					}
					if isHTTPPkg(sel.X) && implicitDefaultClientFuncs[sel.Sel.Name] {
						banned = append(banned, ban{x.Pos(), "http." + sel.Sel.Name + " uses http.DefaultClient implicitly and cannot be instrumented; build a client and wrap it"})
					}
				}
				// new(http.Client)
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isHTTPClientType(x.Args[0]) {
					clients = append(clients, x.Pos())
				}
			case *ast.UnaryExpr:
				if x.Op == token.AND {
					if lit, ok := x.X.(*ast.CompositeLit); ok && isHTTPClientType(lit.Type) {
						addressed[lit.Pos()] = true
						clients = append(clients, x.Pos())
					}
				}
			case *ast.SelectorExpr:
				if isHTTPPkg(x.X) && x.Sel.Name == "DefaultClient" {
					clients = append(clients, x.Pos())
				}
			case *ast.CompositeLit:
				if isHTTPClientType(x.Type) && !addressed[x.Pos()] {
					banned = append(banned, ban{x.Pos(), "an http.Client VALUE has no pointer to wrap at its construction; build &http.Client{} and wrap it"})
				}
			case *ast.ValueSpec:
				if x.Type != nil && isHTTPClientType(x.Type) {
					banned = append(banned, ban{x.Pos(), "a `var ... http.Client` declaration cannot be wrapped at construction; build &http.Client{} and wrap it"})
				}
			}
			return true
		})
	}
	require.NotEmpty(t, clients, "found no http client construction in this package at all; if the wiring moved, this test is now vacuous and must be updated rather than deleted")
	for _, pos := range clients {
		at := fset.Position(pos)
		assert.True(t, wrapped[pos],
			"%s:%d constructs an uninstrumented http client. Every outbound client in this package must be wrapped at construction "+
				"— forge.InstrumentHTTPClient for forge traffic, ollama.InstrumentHTTPClient for the embedder (loam-9v9s).",
			filepath.Base(at.Filename), at.Line)
	}
	for _, b := range banned {
		at := fset.Position(b.pos)
		assert.Fail(t, "uninstrumentable http client",
			"%s:%d: %s (loam-9v9s).", filepath.Base(at.Filename), at.Line, b.reason)
	}
}

// isHTTPClientType reports whether an expression names the type http.Client.
func isHTTPClientType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Client" {
		return false
	}
	return isHTTPPkg(sel.X)
}

// isHTTPPkg reports whether expr is the identifier `http`.
func isHTTPPkg(expr ast.Expr) bool {
	pkg, ok := expr.(*ast.Ident)
	return ok && pkg.Name == "http"
}
