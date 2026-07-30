package parser

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// graphEdgeQueryPath is the SQL that resolves code-graph edges. Its
// language-bucket CASE expressions (loam-w5g) classify a file by extension
// so an edge never crosses a language boundary, which necessarily restates
// the extension set extensionLanguages owns.
const graphEdgeQueryPath = "../db/queries/code_graph.sql"

// bucketExtensions pulls every extension out of the CASE arms in
// ResolveGraphEdgeCandidates. The arms are Postgres regexes of the form
// '\.(ts|mts|cts|tsx)$' or '\.go$', so this discovers the extension set
// from the SQL itself rather than from a second hand-maintained list --
// per loam-ddv's guard-design lesson, a guard that restates the thing it
// guards is maintained by the same hand and catches nothing.
var bucketExtensionRe = regexp.MustCompile(`\\\.\(?([a-z|]+)\)?\$`)

// TestGraphEdgeLanguageBucketsCoverEveryGrammar fails when a grammar is
// added to extensionLanguages without being classified in the graph-edge
// query. That combination is silent, not loud: an unclassified extension
// makes the CASE yield NULL, NULL = NULL is not true, and EVERY edge in
// the new language is dropped from the code graph with no error anywhere.
// The only signal would be a user noticing their graph is empty.
func TestGraphEdgeLanguageBucketsCoverEveryGrammar(t *testing.T) {
	t.Parallel()
	sql, err := os.ReadFile(graphEdgeQueryPath)
	require.NoError(t, err, "the graph-edge query must be readable for this guard to mean anything")
	matches := bucketExtensionRe.FindAllStringSubmatch(string(sql), -1)
	require.NotEmpty(t, matches, "found no language-bucket CASE arms in %s -- the guard has drifted off its target, not passed", graphEdgeQueryPath)
	classified := map[string]struct{}{}
	for _, m := range matches {
		for _, ext := range strings.Split(m[1], "|") {
			classified["."+ext] = struct{}{}
		}
	}
	var missing []string
	for ext := range extensionLanguages {
		if _, ok := classified[ext]; !ok {
			missing = append(missing, ext)
		}
	}
	sort.Strings(missing)
	assert.Empty(t, missing, "extensions have a grammar but no language bucket in %s, so every code-graph edge in those files is silently dropped", graphEdgeQueryPath)
}

// TestGraphEdgeLanguageBucketsClassifyNothingExtra is the other direction:
// a bucket arm naming an extension no grammar produces is dead weight that
// suggests the two lists have drifted, and would quietly widen matching if
// a grammar for it is ever added under a different bucket.
func TestGraphEdgeLanguageBucketsClassifyNothingExtra(t *testing.T) {
	t.Parallel()
	sql, err := os.ReadFile(graphEdgeQueryPath)
	require.NoError(t, err)
	matches := bucketExtensionRe.FindAllStringSubmatch(string(sql), -1)
	require.NotEmpty(t, matches)
	var extra []string
	for _, m := range matches {
		for _, ext := range strings.Split(m[1], "|") {
			if _, ok := extensionLanguages["."+ext]; !ok {
				extra = append(extra, "."+ext)
			}
		}
	}
	sort.Strings(extra)
	assert.Empty(t, extra, "%s classifies extensions that no grammar parses", graphEdgeQueryPath)
}
