package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/testembed"
)

// m5PostMergeCorpus is the exact set of file contents demo:m5's search
// query is ranked against: fixture-polyglot's own tree, with the two files
// the demo rewrites REPLACED by the versions that end up on the indexed
// branch after the pull request merges, plus the work branch's new file.
//
// Building it this way rather than reading the fixture tree alone is the
// whole point. The demo does not search the fixture; it searches what the
// fixture becomes after an upstream advance and a merged proposal, and a
// query token smuggled in by one of the demo's OWN rewritten Markdown
// files would break the ranking exactly as surely as one already in the
// fixture.
func m5PostMergeCorpus(t *testing.T) map[string]string {
	t.Helper()
	corpus := readFixtureCorpus(t)
	require.Contains(t, corpus, readmePath, "the fixture must already carry %s for the demo's conflict to be a conflict", readmePath)
	require.Contains(t, corpus, changelogPath, "the fixture must already carry %s for the demo's conflict to be a conflict", changelogPath)
	corpus[readmePath] = caughtUpReadme
	corpus[changelogPath] = caughtUpChangelog
	return corpus
}

// TestM5SearchQuery_TokensAppearOnlyInTheSessionFile is the first half of
// the proof behind demo:m5's "the session chunk ranks first" assertion.
//
// internal/testembed scores by exact-token overlap, so a document
// containing none of the query's tokens scores exactly zero -- not "low",
// zero. If every m5SearchQuery token appears in sessionFileContent and in
// nothing else on the post-merge indexed branch, every other chunk scores
// zero and the session chunk ranks first by construction. That is a
// property of the corpus, checked here, not an observation about one run.
func TestM5SearchQuery_TokensAppearOnlyInTheSessionFile(t *testing.T) {
	t.Parallel()
	corpus := m5PostMergeCorpus(t)
	queryTokens := tokensOf(m5SearchQuery)
	require.NotEmpty(t, queryTokens)
	sessionTokens := tokensOf(sessionFileContent)
	for _, token := range queryTokens {
		assert.Contains(t, sessionTokens, token,
			"m5SearchQuery token %q does not appear in sessionFileContent, so the session chunk would score zero for it", token)
		for path, content := range corpus {
			assert.NotContains(t, tokensOf(content), token,
				"m5SearchQuery token %q also appears in %s, which would let that file's chunks compete for first place", token, path)
		}
	}
}

// TestM5SearchQuery_TokensDoNotCollideWithTheCorpus is the second half.
//
// testembed hashes tokens into testembed.Dimension buckets, so two
// distinct tokens can share one (unavoidable by pigeonhole --
// internal/testembed/collision.go). A collision between a query token and
// a token in another file would give that file a nonzero score for a word
// it does not contain, which is exactly what the no-shared-tokens check
// above cannot see. CollidingTokens is called with the co-ranked
// vocabulary its doc comment prescribes -- the query plus the documents it
// is ranked against -- never the whole tree.
func TestM5SearchQuery_TokensDoNotCollideWithTheCorpus(t *testing.T) {
	t.Parallel()
	corpus := m5PostMergeCorpus(t)
	corpus[sessionFilePath] = sessionFileContent
	queryTokens := tokensOf(m5SearchQuery)
	texts := []string{m5SearchQuery}
	for _, content := range corpus {
		texts = append(texts, content)
	}
	for _, group := range testembed.CollidingTokens(texts...) {
		hasQueryToken := false
		for _, token := range group {
			if slices.Contains(queryTokens, token) {
				hasQueryToken = true
				break
			}
		}
		assert.False(t, hasQueryToken,
			"an m5SearchQuery token shares a bucket with corpus vocabulary %v, which would give an unrelated chunk a nonzero score", group)
	}
}

// TestM5SessionSymbol_IsUniqueToTheProposal pins the graph half of the
// demo's proof. StartSession resolving at the merge commit only means "the
// merge reached the index" if nothing else in the post-merge tree defines
// it.
func TestM5SessionSymbol_IsUniqueToTheProposal(t *testing.T) {
	t.Parallel()
	assert.Contains(t, sessionFileContent, sessionSymbol)
	for path, content := range m5PostMergeCorpus(t) {
		assert.NotContains(t, content, sessionSymbol, "%s already mentions %s", path, sessionSymbol)
	}
}

// TestM5SessionFile_IsChunkable guards the assumption the search assertion
// rests on: internal/ingest/chunker chunks Go by top-level declaration,
// extended backward through the contiguous leading comment block, so every
// query token has to live in the comment lines immediately above func
// StartSession -- not merely somewhere in the file.
func TestM5SessionFile_IsChunkable(t *testing.T) {
	t.Parallel()
	lines := strings.Split(sessionFileContent, "\n")
	declIndex := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "func "+sessionSymbol+"(") })
	require.NotEqual(t, -1, declIndex, "sessionFileContent must declare func %s at top level", sessionSymbol)
	require.Greater(t, declIndex, 0)
	for _, token := range tokensOf(m5SearchQuery) {
		found := false
		for i := declIndex - 1; i >= 0 && strings.HasPrefix(lines[i], "//"); i-- {
			if slices.Contains(tokensOf(lines[i]), token) {
				found = true
				break
			}
		}
		assert.True(t, found,
			"m5SearchQuery token %q is not in the contiguous comment block immediately above func %s, so it would not be part of that symbol's chunk", token, sessionSymbol)
	}
}

// TestM5ConflictFixtures_ActuallyConflict guards the demo's other
// structural assumption. Both conflicts are three-way merges of an
// overlapping edit, so the proposal's version and upstream's version of
// each file must differ from each other AND from the fixture's original --
// if any pair were equal, git would auto-merge, nothing would be flagged,
// and the entire catch-up half of the demo would pass vacuously.
func TestM5ConflictFixtures_ActuallyConflict(t *testing.T) {
	t.Parallel()
	original := readFixtureCorpus(t)
	for _, tc := range []struct {
		path             string
		branch, upstream string
		caughtUp         string
		name             string
	}{
		{readmePath, proposalReadme, upstreamReadme, caughtUpReadme, "reviewed branch"},
		{changelogPath, draftChangelog, upstreamChangelog, caughtUpChangelog, "draft branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := original[tc.path]
			require.NotEmpty(t, base, "%s must exist in the fixture", tc.path)
			assert.NotEqual(t, base, tc.branch, "the work branch's %s is unchanged from the fixture, so it edits nothing", tc.path)
			assert.NotEqual(t, base, tc.upstream, "upstream's %s is unchanged from the fixture, so the target never advanced", tc.path)
			assert.NotEqual(t, tc.branch, tc.upstream, "the work branch and upstream wrote the same %s, so git would auto-merge and no conflict could arise", tc.path)
			assert.NotEqual(t, tc.upstream, tc.caughtUp, "the catch-up resolution of %s discards the branch's own edit", tc.path)
			assert.NotEqual(t, tc.branch, tc.caughtUp, "the catch-up resolution of %s discards upstream's edit, so it is not a resolution", tc.path)
		})
	}
}

// TestFixtureFile_PrintsEveryBlobVerbatim pins `demoenv fixture-file`'s
// contract: byte-for-byte output with no trailing newline added, because
// the Taskfile redirects it straight into files that are then committed
// and diffed.
func TestFixtureFile_PrintsEveryBlobVerbatim(t *testing.T) {
	t.Parallel()
	for name, want := range m5Fixtures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			require.NoError(t, runFixtureFile([]string{"-name", name}, &out))
			assert.Equal(t, want, out.String())
		})
	}
}

// TestFixtureFile_RejectsAnUnknownName pins the loud-failure half: an
// unknown name must be an error, never empty output, because an empty file
// committed onto a work branch sails through the push and fails several
// steps later naming the wrong culprit.
func TestFixtureFile_RejectsAnUnknownName(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	err := runFixtureFile([]string{"-name", "no-such-blob"}, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown fixture")
	assert.Empty(t, out.String())
}
