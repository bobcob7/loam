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

// TestAllSentinelsDiscoversEveryExportedErrVar is the stronger half of a
// two-test pairing with TestFakeforgeSentinelsMatchOnlyTheirOwnForgeClass
// (internal/fakeforge/errors_test.go). This test parses this package's own
// non-test source (stdlib go/parser, no new dependency) for every exported
// top-level `Err*` var declaration and checks AllSentinels() against it two
// ways:
//
//  1. Count equality — catches a sentinel declared here and never added to
//     AllSentinels() (loam-ddv review, "declare a sixth Err* var, do not add
//     it to AllSentinels()": every existing test at the time passed green)
//     AND the reverse, a sentinel quietly dropped from AllSentinels() while
//     its declaration stays. Either direction changes the declared-vs-
//     returned count, so this one check is two-sided: it catches additions
//     left unwired and plain removals on its own, without needing a paired
//     guard elsewhere.
//  2. Uniqueness over AllSentinels()'s own return value — catches a
//     sentinel silently DUPLICATED in place of a dropped one, which the
//     count check alone cannot see because the count doesn't change. The
//     reviewer validated this against a live mutation: AllSentinels()
//     returning {ErrInvalidToken, ErrInvalidToken, ErrRepoNotFound,
//     ErrNoWriteAccess, ErrDuplicatePR} — still 5 entries, still every
//     fakeforge `want` column's target — silently drops ErrInsufficientScope,
//     which is loam-ddv's original bug reproduced by a single-line
//     copy-paste slip in one return statement, the single most likely way
//     that line is ever edited wrong.
//
// It is deliberately not an identity check: go/ast gives names, not the
// runtime error values those names are bound to, so it cannot (and does not
// try to) confirm AllSentinels() contains the RIGHT five, only that it
// contains as many DISTINCT entries as this package declares. Catching a
// wrong-identity swap — AllSentinels() unchanged, but a fakeforge sentinel
// wraps the wrong member of it — is what
// TestFakeforgeSentinelsMatchOnlyTheirOwnForgeClass's `want` column does;
// that test's own require.Len canary is the narrower of the two guards,
// only catching a change to AllSentinels()'s length. Between the count
// check, the uniqueness check, and that `want` column, the pair is tight
// against additions, removals, and duplicate-swaps.
//
// This also depends on every forge-level sentinel following the `Err*`
// naming convention: a var named, say, RateLimitedError and left out of
// AllSentinels() is invisible to this test (verified empirically) —
// acceptable because `Err*` is the universal Go convention and all five
// current sentinels follow it, but worth stating plainly rather than
// leaving a reader to assume the discovery is name-agnostic.
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
	require.NotEmpty(t, declared, "found no exported Err* vars in internal/forge — the AST discovery walked zero declarations, which is a sign it is looking in the wrong place, not that forge has no sentinels")
	require.Len(t, AllSentinels(), len(declared),
		"internal/forge declares these exported Err* vars: %v — AllSentinels() must return exactly one entry per sentinel, or fakeforge's regression guard cannot see it either", declared)
	unique := make(map[error]struct{}, len(declared))
	for _, sentinel := range AllSentinels() {
		unique[sentinel] = struct{}{}
	}
	require.Len(t, unique, len(declared),
		"AllSentinels() has the right length but contains a duplicate, which silently drops a distinct sentinel while the count check above stays green")
}
