package mirrorsync

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFetchRefspecsNoWorkBranchesReturnsOnlyPositiveRefspecs(t *testing.T) {
	t.Parallel()
	refspecs := buildFetchRefspecs(nil)
	assert.Equal(t, []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*", "^refs/heads/loam-reserved/*"}, refspecs)
}

func TestBuildFetchRefspecsAddsOneNegativeExclusionPerWorkBranch(t *testing.T) {
	t.Parallel()
	refspecs := buildFetchRefspecs([]string{"wb-1", "wb-2"})
	assert.Equal(t, []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*", "^refs/heads/loam-reserved/*", "^refs/heads/loam-reserved/wb-1", "^refs/heads/loam-reserved/wb-2"}, refspecs)
}

// TestBuildFetchRefspecsExcludesEveryNonHeadsNonTagsNamespace is the
// narrowing loam-5f3 makes real: the wildcard "+refs/*:refs/*" this
// function used to return would fetch refs/pull/*, refs/notes/*, and
// refs/replace/* too. Asserting the exact returned slice (as the two
// tests above do) already proves those strings are gone from the
// positive side; this test names the specific upstream namespaces the
// narrowing was for, so a regression back to a broader glob (e.g.
// "+refs/*:refs/*" or "+refs/**:refs/**") is caught by name rather than
// only by incidental slice-equality.
func TestBuildFetchRefspecsExcludesEveryNonHeadsNonTagsNamespace(t *testing.T) {
	t.Parallel()
	refspecs := buildFetchRefspecs(nil)
	for _, positive := range refspecs[:2] {
		assert.True(t, strings.HasPrefix(positive, "+refs/heads/") || strings.HasPrefix(positive, "+refs/tags/"),
			"positive refspec %q must target only refs/heads/* or refs/tags/*", positive)
	}
	for _, excludedNamespace := range []string{"refs/pull/", "refs/notes/", "refs/replace/", "refs/merge-requests/"} {
		for _, spec := range refspecs {
			assert.NotContains(t, spec, excludedNamespace, "refspec %q must not reference %s", spec, excludedNamespace)
		}
	}
}

func TestParsePorcelainFetchParsesFastForward(t *testing.T) {
	t.Parallel()
	out := []byte("  8e9302dc2468c98d4c4ed30341b2eb3d90d0ac12 8c9a25ef69308c445dc914c7485e411a7312a167 refs/heads/main\n")
	refs, err := parsePorcelainFetch(out)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, RefUpdate{Ref: "refs/heads/main", OldSHA: "8e9302dc2468c98d4c4ed30341b2eb3d90d0ac12", NewSHA: "8c9a25ef69308c445dc914c7485e411a7312a167"}, refs[0])
}

func TestParsePorcelainFetchParsesForcedUpdate(t *testing.T) {
	t.Parallel()
	out := []byte("+ 8c9a25ef69308c445dc914c7485e411a7312a167 fbd521e0d1153d8ad2effa0474b56c99d4cbaba6 refs/heads/main\n")
	refs, err := parsePorcelainFetch(out)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "fbd521e0d1153d8ad2effa0474b56c99d4cbaba6", refs[0].NewSHA)
}

func TestParsePorcelainFetchParsesPruneAsEmptyNewSHA(t *testing.T) {
	t.Parallel()
	out := []byte("- 8f8b1a8f23ab2fcf5a97cf3356225dd5df86843c 0000000000000000000000000000000000000000 refs/heads/feature\n")
	refs, err := parsePorcelainFetch(out)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, RefUpdate{Ref: "refs/heads/feature", OldSHA: "8f8b1a8f23ab2fcf5a97cf3356225dd5df86843c", NewSHA: ""}, refs[0])
}

func TestParsePorcelainFetchParsesNewRefAsEmptyOldSHA(t *testing.T) {
	t.Parallel()
	out := []byte("* 0000000000000000000000000000000000000000 4f19bdffac2774e6d125bd189f030c166529b260 refs/heads/newbranch\n")
	refs, err := parsePorcelainFetch(out)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, RefUpdate{Ref: "refs/heads/newbranch", OldSHA: "", NewSHA: "4f19bdffac2774e6d125bd189f030c166529b260"}, refs[0])
}

func TestParsePorcelainFetchParsesMultipleLines(t *testing.T) {
	t.Parallel()
	out := []byte("- 8f8b1a8f23ab2fcf5a97cf3356225dd5df86843c 0000000000000000000000000000000000000000 refs/heads/feature\n" +
		"  8f8b1a8f23ab2fcf5a97cf3356225dd5df86843c 652c30b0450edd64e6d03e2c634acf0dddc787b6 refs/heads/main\n" +
		"* 0000000000000000000000000000000000000000 4f19bdffac2774e6d125bd189f030c166529b260 refs/heads/newbranch\n")
	refs, err := parsePorcelainFetch(out)
	require.NoError(t, err)
	assert.Len(t, refs, 3)
}

func TestParsePorcelainFetchEmptyOutputReturnsNoRefs(t *testing.T) {
	t.Parallel()
	refs, err := parsePorcelainFetch([]byte(""))
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestParsePorcelainFetchSkipsLineWithTooFewFields(t *testing.T) {
	t.Parallel()
	refs, err := parsePorcelainFetch([]byte("only-two-fields\n"))
	require.NoError(t, err)
	assert.Empty(t, refs)
}

// TestParsePorcelainFetchSkipsInterleavedStderrWarning is the must-fix
// regression: Transport.run returns cmd.CombinedOutput()
// (internal/gittransport/transport.go), so a benign git warning on stderr
// -- e.g. a redirected upstream URL -- lands in the same byte stream this
// function parses. A redirect warning has exactly 3 whitespace-separated
// fields ("warning:", "redirecting", "to" -- or similar), so the old
// field-count-only check would have silently fabricated a bogus RefUpdate
// from it; this asserts the warning line contributes nothing and the real
// porcelain lines around it are still parsed correctly.
func TestParsePorcelainFetchSkipsInterleavedStderrWarning(t *testing.T) {
	t.Parallel()
	out := []byte("warning: redirecting to https://github.com/octocat/Hello-World.git/\n" +
		"  8e9302dc2468c98d4c4ed30341b2eb3d90d0ac12 8c9a25ef69308c445dc914c7485e411a7312a167 refs/heads/main\n")
	refs, err := parsePorcelainFetch(out)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "refs/heads/main", refs[0].Ref)
}

// TestParsePorcelainFetchSkipsFromSummaryLine covers the other shape
// git's own "From <url>" fetch summary line takes on stderr: exactly 2
// fields, which the old implementation hard-errored on -- failing an
// entire sync cycle into sync_state=error over a benign line, not a real
// fetch failure.
func TestParsePorcelainFetchSkipsFromSummaryLine(t *testing.T) {
	t.Parallel()
	out := []byte("From https://forge.example.com/acme/widgets\n" +
		"  8e9302dc2468c98d4c4ed30341b2eb3d90d0ac12 8c9a25ef69308c445dc914c7485e411a7312a167 refs/heads/main\n")
	refs, err := parsePorcelainFetch(out)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "refs/heads/main", refs[0].Ref)
}

func TestParsePorcelainFetchSkipsLineWithNonRefDestination(t *testing.T) {
	t.Parallel()
	out := []byte("  8e9302dc2468c98d4c4ed30341b2eb3d90d0ac12 8c9a25ef69308c445dc914c7485e411a7312a167 not-a-ref\n")
	refs, err := parsePorcelainFetch(out)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestParsePorcelainFetchSkipsLineWithNonHexObjectID(t *testing.T) {
	t.Parallel()
	out := []byte("  not-hex-old not-hex-new refs/heads/main\n")
	refs, err := parsePorcelainFetch(out)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestMirrorFetcherFetchBuildsRefspecsAndParsesResult(t *testing.T) {
	t.Parallel()
	repos := &repoResolverMock{
		ResolveRepoFunc: func(_ context.Context, repo RepoID) (string, string, []string, error) {
			assert.Equal(t, RepoID("acme/widgets"), repo)
			return "forge.example.com", "https://forge.example.com/acme/widgets.git", []string{"wb-1"}, nil
		},
	}
	upstream := &upstreamRefFetcherMock{
		FetchFunc: func(_ context.Context, host, mirrorDir, upstreamURL string, refspecs []string) ([]byte, error) {
			assert.Equal(t, "forge.example.com", host)
			assert.Equal(t, "/data/mirrors/acme/widgets.git", mirrorDir)
			assert.Equal(t, "https://forge.example.com/acme/widgets.git", upstreamURL)
			assert.Equal(t, []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*", "^refs/heads/loam-reserved/*", "^refs/heads/loam-reserved/wb-1"}, refspecs)
			return []byte("  8e9302dc2468c98d4c4ed30341b2eb3d90d0ac12 8c9a25ef69308c445dc914c7485e411a7312a167 refs/heads/main\n"), nil
		},
	}
	fetcher := NewMirrorFetcher("/data", upstream, repos)
	result, err := fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	require.Len(t, result.Refs, 1)
	assert.Equal(t, RefUpdate{Ref: "refs/heads/main", OldSHA: "8e9302dc2468c98d4c4ed30341b2eb3d90d0ac12", NewSHA: "8c9a25ef69308c445dc914c7485e411a7312a167"}, result.Refs[0])
}

func TestMirrorFetcherFetchPropagatesResolveRepoError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom: repo lookup failed")
	repos := &repoResolverMock{
		ResolveRepoFunc: func(_ context.Context, _ RepoID) (string, string, []string, error) {
			return "", "", nil, wantErr
		},
	}
	upstream := &upstreamRefFetcherMock{
		FetchFunc: func(context.Context, string, string, string, []string) ([]byte, error) {
			t.Fatal("Fetch must not be called when repo resolution already failed")
			return nil, nil
		},
	}
	fetcher := NewMirrorFetcher("/data", upstream, repos)
	_, err := fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestMirrorFetcherFetchPropagatesUpstreamFetchError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom: git fetch failed")
	repos := &repoResolverMock{
		ResolveRepoFunc: func(context.Context, RepoID) (string, string, []string, error) {
			return "forge.example.com", "https://forge.example.com/acme/widgets.git", nil, nil
		},
	}
	upstream := &upstreamRefFetcherMock{
		FetchFunc: func(context.Context, string, string, string, []string) ([]byte, error) {
			return nil, wantErr
		},
	}
	fetcher := NewMirrorFetcher("/data", upstream, repos)
	_, err := fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}
