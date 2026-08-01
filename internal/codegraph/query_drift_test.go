package codegraph

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dependentsQueryPath is the production Dependents query.
// TestDependentsCTE_GuardRemovedHangs and
// TestDependentsCTE_DiamondFixture_BoundedIntermediateRows (both in
// integration_test.go, behind the integration build tag) each hand-copy
// its recursive term into a Go const, because they need to run a variant
// of it -- guard removed, or the recursive term's raw row count -- that
// the sqlc-generated Dependents method does not expose. That duplication
// is legitimate (loam-8hol), but it means the .sql file can change while
// the copies do not, and both tests would keep passing while proving
// something about a query the code no longer runs (loam-9xx hit exactly
// this and had to update both copies by hand). This guard is what would
// catch that: it needs neither Postgres nor the integration tag, so it
// runs in the ordinary `go test` sweep instead of only in the
// containerized integration suite -- see this file's test doc comment.
const dependentsQueryPath = "../db/queries/code_graph.sql"

// dependentsTestFilePath holds the two inlined copies this guard checks.
const dependentsTestFilePath = "integration_test.go"

// recursiveTermStart marks the start of the fragment shared by the
// production query and both inlined copies: the WITH RECURSIVE clause's
// base case and recursive term. It deliberately stops short of the CYCLE
// clause -- TestDependentsCTE_GuardRemovedHangs' entire point is running
// this fragment WITHOUT the CYCLE clause, so the CYCLE clause is not part
// of what all three copies hold in common, and comparing it would make
// this guard fail against a correct, deliberate mutation.
const recursiveTermStart = "WITH RECURSIVE dependents(symbol_id, depth) AS ("

// recursiveTermEnd matches the closing paren of the WITH clause, which in
// every copy sits alone (or with a trailing CYCLE clause) at the start of
// its own line.
var recursiveTermEnd = regexp.MustCompile(`(?m)^\)`)

// extractRecursiveTermBody returns the text between recursiveTermStart
// and the next line starting with ")", or "" if either the start marker
// or the closing line is not found. Returning "" rather than panicking or
// silently comparing against nothing lets callers fail loudly when the
// extraction finds nothing -- a guard that quietly stops finding its
// target must fail, not vacuously pass (loam-ddv).
func extractRecursiveTermBody(source string) string {
	start := strings.Index(source, recursiveTermStart)
	if start < 0 {
		return ""
	}
	body := source[start+len(recursiveTermStart):]
	end := recursiveTermEnd.FindStringIndex(body)
	if end == nil {
		return ""
	}
	return body[:end[0]]
}

// normalizeSQL collapses all whitespace -- indentation, newlines, runs of
// spaces -- down to single spaces between tokens, so a copy reformatted
// to a different indent width (Go raw string vs. .sql file) still
// compares equal to the production query. It only touches whitespace:
// token content, order, and count are untouched, so an added, removed, or
// reordered clause -- the actual drift this guard exists to catch --
// still fails the comparison rather than being normalized away.
func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestDependentsRecursiveTermMatchesInlinedCopies fails if
// internal/db/queries/code_graph.sql's Dependents recursive term diverges
// from either hand-inlined copy in integration_test.go. It is intentionally
// NOT behind the integration build tag and touches no database: it only
// reads two files on disk, so it executes in the ordinary unit run (`go
// test ./...`) rather than only in the opt-in, containerized integration
// sweep -- which is what makes it a guard against drift going unnoticed
// for a whole release cycle instead of only at the next manual review.
func TestDependentsRecursiveTermMatchesInlinedCopies(t *testing.T) {
	t.Parallel()
	sqlBytes, err := os.ReadFile(dependentsQueryPath)
	require.NoError(t, err, "the production Dependents query must be readable for this guard to mean anything")
	testBytes, err := os.ReadFile(dependentsTestFilePath)
	require.NoError(t, err, "the test file holding the inlined copies must be readable for this guard to mean anything")

	want := extractRecursiveTermBody(string(sqlBytes))
	require.NotEmpty(t, want, "found no Dependents recursive term in %s -- the guard has drifted off its target, not passed", dependentsQueryPath)
	wantNormalized := normalizeSQL(want)

	starts := regexp.MustCompile(regexp.QuoteMeta(recursiveTermStart)).FindAllStringIndex(string(testBytes), -1)
	require.Len(t, starts, 2, "expected exactly two inlined copies of the Dependents recursive term in %s (TestDependentsCTE_GuardRemovedHangs and TestDependentsCTE_DiamondFixture_BoundedIntermediateRows) -- found %d; a copy was added, removed, or renamed out from under this guard", dependentsTestFilePath, len(starts))

	for i, s := range starts {
		got := extractRecursiveTermBody(string(testBytes)[s[0]:])
		require.NotEmpty(t, got, "found the start of inlined copy #%d in %s but not its closing line -- the guard has drifted off its target, not passed", i+1, dependentsTestFilePath)
		assert.Equal(t, wantNormalized, normalizeSQL(got), "inlined copy #%d of the Dependents recursive term in %s has drifted from %s -- update the inlined copy to match (or, if the divergence is an intentional variant, confirm it still proves something about the query that actually runs)", i+1, dependentsTestFilePath, dependentsQueryPath)
	}
}
