package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/testembed"
)

// fixtureSeedDir is internal/testfixture's embedded seed tree, read here
// straight off disk rather than through testfixture.New. Materializing a
// fixture would need a git binary and would make this unit test depend on
// one for no gain: the property under test is about the BYTES of the
// fixture's files, which are the same bytes either way.
const fixtureSeedDir = "../../internal/testfixture/testdata/fixture-polyglot"

// readFixtureCorpus returns the content of every file in fixture-polyglot
// -- the complete set of text the demo's search query is ranked against,
// minus the two files the demo itself commits.
func readFixtureCorpus(t *testing.T) map[string]string {
	t.Helper()
	corpus := make(map[string]string)
	err := filepath.WalkDir(fixtureSeedDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(fixtureSeedDir, path)
		if relErr != nil {
			return relErr
		}
		corpus[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, corpus, "fixture-polyglot seed tree is empty or missing at %s", fixtureSeedDir)
	return corpus
}

// TestSearchQuery_TokensAppearOnlyInTheAuthFile is the first half of the
// proof behind demo:m3's "the auth chunk ranks first" assertion.
//
// internal/testembed scores by exact-token overlap, so a document
// containing none of the query's tokens has a cosine of exactly zero
// against it -- not "a low score", zero. If every token of searchQuery
// appears in the auth file and in nothing else in the corpus, then every
// other chunk scores zero and the auth chunk, which scores above zero,
// ranks first by construction. That is a property of the corpus, checked
// here, rather than an observation about one run of the demo.
//
// A chunk is always a substring of its file, so checking files rather than
// chunks is sound in the strict direction: a token absent from the file is
// absent from every chunk of it.
func TestSearchQuery_TokensAppearOnlyInTheAuthFile(t *testing.T) {
	t.Parallel()
	corpus := readFixtureCorpus(t)
	corpus[legacyFilePath] = legacyFileContent
	queryTokens := tokensOf(searchQuery)
	require.NotEmpty(t, queryTokens)
	authTokens := tokensOf(authFileContent)
	for _, token := range queryTokens {
		assert.Contains(t, authTokens, token, "searchQuery token %q does not appear in authFileContent, so the auth chunk would score zero for it", token)
		for path, content := range corpus {
			assert.NotContains(t, tokensOf(content), token,
				"searchQuery token %q also appears in %s, which would let that file's chunks compete for first place", token, path)
		}
	}
}

// TestSearchQuery_TokensDoNotCollideWithTheCorpus is the second half of
// the proof.
//
// testembed hashes tokens into testembed.Dimension buckets, so two
// distinct tokens can share a bucket (unavoidable by pigeonhole -- see
// internal/testembed/collision.go). A collision between a query token and
// a token in some other file would give that file a nonzero score for a
// word it does not contain, which is precisely the failure mode the
// no-shared-tokens check above cannot see. CollidingTokens is called with
// exactly the co-ranked vocabulary its doc comment prescribes: the query
// plus the documents it is ranked against, never the whole tree.
func TestSearchQuery_TokensDoNotCollideWithTheCorpus(t *testing.T) {
	t.Parallel()
	corpus := readFixtureCorpus(t)
	corpus[legacyFilePath] = legacyFileContent
	queryTokens := tokensOf(searchQuery)
	texts := []string{searchQuery}
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
			"a searchQuery token shares a bucket with corpus vocabulary %v, which would give an unrelated chunk a nonzero score", group)
	}
}

// TestDemoSymbols_AreUniqueToTheirOwnFile pins the other half of the
// demo's proof-by-presence: LegacySignIn and Login each have to exist in
// exactly one commit's worth of content, or their presence/absence in the
// graph would say nothing about which commit was ingested.
func TestDemoSymbols_AreUniqueToTheirOwnFile(t *testing.T) {
	t.Parallel()
	corpus := readFixtureCorpus(t)
	assert.Contains(t, legacyFileContent, legacySymbol)
	assert.Contains(t, authFileContent, authSymbol)
	assert.NotContains(t, authFileContent, legacySymbol)
	assert.NotContains(t, legacyFileContent, authSymbol)
	for path, content := range corpus {
		assert.NotContains(t, content, legacySymbol, "%s already defines %s", path, legacySymbol)
		assert.NotContains(t, content, authSymbol, "%s already defines %s", path, authSymbol)
	}
}

// TestDemoFiles_AreValidGoAndChunkable guards the assumption the search
// assertion rests on: the auth file's doc comment is only embedded because
// the chunker attaches leading comments to the declaration below them, so
// the comment must actually be contiguous with func Login.
func TestDemoFiles_AreValidGoAndChunkable(t *testing.T) {
	t.Parallel()
	lines := strings.Split(authFileContent, "\n")
	declIndex := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "func "+authSymbol+"(") })
	require.NotEqual(t, -1, declIndex, "authFileContent must declare func %s at top level", authSymbol)
	require.Greater(t, declIndex, 0)
	for _, token := range tokensOf(searchQuery) {
		found := false
		for i := declIndex - 1; i >= 0 && strings.HasPrefix(lines[i], "//"); i-- {
			if slices.Contains(tokensOf(lines[i]), token) {
				found = true
				break
			}
		}
		assert.True(t, found, "searchQuery token %q is not in the contiguous comment block immediately above func %s, so it would not be part of that symbol's chunk", token, authSymbol)
	}
}

// tokensOf reproduces internal/testembed's tokenizer: case-fold, then
// split on anything that is not [a-z0-9].
func tokensOf(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}
