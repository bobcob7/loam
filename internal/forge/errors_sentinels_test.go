package forge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAllSentinelsDiscoversEveryExportedErrVar closes the direction
// TestFakeforgeSentinelsMatchOnlyTheirOwnForgeClass's require.Len canary
// cannot: that guard (internal/fakeforge/errors_test.go) catches a sentinel
// silently REMOVED from AllSentinels(), because its `want` column names
// each of the five by identity — but both AllSentinels() and its own
// hand-counted length are maintained by the same hand, so a sentinel
// declared here and never ADDED to AllSentinels() passes every existing
// test green (loam-ddv review, mutation "declare a sixth Err* var, do not
// add it to AllSentinels()"). That is exactly the drift class loam-ddv was
// widened to eliminate — errMissingScope's stale wrap to ErrInvalidToken
// survived unnoticed for the same structural reason.
//
// This test parses this package's own non-test source (stdlib go/parser,
// no new dependency) for every exported top-level `Err*` var declaration
// and requires AllSentinels() to have exactly that many entries. It is
// deliberately a count check, not an identity check: go/ast gives names,
// not the runtime error values those names are bound to, so it cannot
// (and does not try to) confirm AllSentinels() contains the RIGHT five —
// only that it contains as many as this package declares. Catching a
// wrong-identity swap is what TestFakeforgeSentinelsMatchOnlyTheirOwnForgeClass's
// `want` column does; catching a same-count swap (one sentinel dropped,
// a different one added, net count unchanged) is the one gap neither test
// closes alone — the two are a pairing, not each individually complete.
func TestAllSentinelsDiscoversEveryExportedErrVar(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)
	var declared []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range value.Names {
						if ast.IsExported(name.Name) && strings.HasPrefix(name.Name, "Err") {
							declared = append(declared, name.Name)
						}
					}
				}
			}
		}
	}
	sort.Strings(declared)
	require.Len(t, AllSentinels(), len(declared),
		"internal/forge declares these exported Err* vars: %v — AllSentinels() must return exactly one entry per sentinel, or fakeforge's regression guard cannot see it either", declared)
}
