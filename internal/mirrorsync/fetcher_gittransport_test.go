package mirrorsync

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/gittransport"
)

// This file proves MirrorFetcher's three defining properties --
// upstream-wins forced fetch, prune, and work-branch-ref exclusion --
// against a real git subprocess and a real (in-process) fakeforge git
// smart-HTTP server, the same style internal/gittransport's own tests use
// (docs/testing-spec.md): no container, no mocked git.

// requireGit skips the test when the git binary is not on PATH, matching
// internal/gittransport's own convention for tests that shell out to a
// real git subprocess.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found on PATH; skipping mirror-fetch integration test")
	}
}

// newFakeForgeServer builds a fakeforge.Server wrapped in an
// httptest.Server, the real git smart-HTTP counterparty this test drives
// MirrorFetcher against.
func newFakeForgeServer(t *testing.T) *fakeforge.Server {
	t.Helper()
	srv, err := fakeforge.New(testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.SetBaseURL(ts.URL)
	return srv
}

// staticCredentialSource is a minimal credentialSource returning the same
// token for every host, satisfying gittransport's own credentialSource
// interface structurally.
type staticCredentialSource struct {
	token string
}

func (s *staticCredentialSource) GetByHost(_ context.Context, _ string) (credentialstore.Credential, error) {
	return credentialstore.Credential{Token: s.token}, nil
}

// staticRepoResolver is a repoResolver returning fixed coordinates and
// work-branch names, for driving MirrorFetcher against a real Transport
// without a real reposstore/workbranchstore.
type staticRepoResolver struct {
	host, upstreamURL string
	workBranchNames   []string
}

func (r *staticRepoResolver) ResolveRepo(context.Context, RepoID) (string, string, []string, error) {
	return r.host, r.upstreamURL, r.workBranchNames, nil
}

// hostOfURL returns rawURL's host:port authority, the value production
// call sites pass as Transport's host parameter (the same key
// credentialstore.Store.GetByHost is keyed on).
func hostOfURL(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Host
}

// mirrorRevParse runs `git rev-parse --verify <ref>` directly against the
// mirror (never through MirrorFetcher/Transport), as an independent
// verifier of what actually landed.
func mirrorRevParse(t *testing.T, mirrorDir, ref string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "--git-dir="+mirrorDir, "rev-parse", "--verify", ref)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// newRealMirrorFetcher builds a MirrorFetcher over a real
// gittransport.Transport hitting srv's "acme/widgets" repo, with an empty
// bare mirror initialized at exactly the path MirrorFetcher.mirrorDir
// derives from repo -- the same on-disk layout production uses
// (docs/server-spec.md: "bare mirrors under
// <dir>/mirrors/<group>/<repo_name>.git").
func newRealMirrorFetcher(t *testing.T, srv *fakeforge.Server, token string, workBranchNames []string) (fetcher *MirrorFetcher, mirrorDir, upstreamURL string) {
	t.Helper()
	requireGit(t)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL = srv.GitURL("acme/widgets")
	host := hostOfURL(t, upstreamURL)
	transport := gittransport.New(&staticCredentialSource{token: token}, fakeforge.NewClient("", ""), testLogger())
	dataDir := t.TempDir()
	resolver := &staticRepoResolver{host: host, upstreamURL: upstreamURL, workBranchNames: workBranchNames}
	fetcher = NewMirrorFetcher(dataDir, transport, resolver)
	mirrorDir = fetcher.mirrorDir(RepoID("acme/widgets"))
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	initCmd := exec.CommandContext(t.Context(), "git", "init", "--bare", "-q", mirrorDir)
	out, err := initCmd.CombinedOutput()
	require.NoErrorf(t, err, "git init --bare: %s", out)
	return fetcher, mirrorDir, upstreamURL
}

// mustSetLocalRef points ref at whatever fromRef currently resolves to in
// the mirror, standing in for Loam's own "work start" creating a
// work-branch ref server-side (docs/git-spec.md), and returns the SHA it
// was set to.
func mustSetLocalRef(t *testing.T, mirrorDir, ref, fromRef string) string {
	t.Helper()
	sha, err := mirrorRevParse(t, mirrorDir, fromRef)
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), "git", "--git-dir="+mirrorDir, "update-ref", ref, sha)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "update-ref: %s", out)
	return sha
}

func TestMirrorFetcherFollowsUpstreamForcePush(t *testing.T) {
	t.Parallel()
	srv := newFakeForgeServer(t)
	const token = "force-push-test-token"
	srv.AddToken(token)
	fetcher, mirrorDir, _ := newRealMirrorFetcher(t, srv, token, nil)
	_, err := fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	preSHA, err := mirrorRevParse(t, mirrorDir, "refs/heads/main")
	require.NoError(t, err)
	// ForcePushBranch's synthesized-divergent-commit path resolves the
	// branch's parent commit as its rewrite base, so the branch needs a
	// second commit first -- a fresh single-commit branch has no parent to
	// resolve.
	require.NoError(t, srv.AdvanceBranch(t.Context(), "acme/widgets", "main", fakeforge.AdvanceOptions{}))
	require.NoError(t, srv.ForcePushBranch(t.Context(), "acme/widgets", "main", fakeforge.ForcePushOptions{}))
	_, err = fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err, "a forced fetch must accept a genuine upstream force-push, never reject it")
	postSHA, err := mirrorRevParse(t, mirrorDir, "refs/heads/main")
	require.NoError(t, err)
	assert.NotEqual(t, preSHA, postSHA, "the mirror's main must follow the rewritten upstream history")
}

func TestMirrorFetcherPrunesUpstreamDeletedBranch(t *testing.T) {
	t.Parallel()
	srv := newFakeForgeServer(t)
	const token = "prune-test-token"
	srv.AddToken(token)
	fetcher, mirrorDir, _ := newRealMirrorFetcher(t, srv, token, nil)
	require.NoError(t, srv.CreateCollidingBranch(t.Context(), "acme/widgets", "wb-doomed", ""))
	_, err := fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	_, err = mirrorRevParse(t, mirrorDir, "refs/heads/wb-doomed")
	require.NoError(t, err, "precondition: the branch must exist in the mirror before deletion upstream")
	require.NoError(t, srv.DeleteBranch(t.Context(), "acme/widgets", "wb-doomed"))
	_, err = fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	_, err = mirrorRevParse(t, mirrorDir, "refs/heads/wb-doomed")
	assert.Error(t, err, "a branch deleted upstream must be pruned from the mirror")
}

// TestMirrorFetcherExcludesWorkBranchRefFromForcePush is the property most
// likely to be silently wrong: seed a work-branch ref in the mirror, then
// force-push a colliding upstream branch, and assert the work-branch ref
// is untouched. A test that only checks upstream branches arrive would
// pass even if the fetch quietly clobbered every work branch on every
// tick.
func TestMirrorFetcherExcludesWorkBranchRefFromForcePush(t *testing.T) {
	t.Parallel()
	srv := newFakeForgeServer(t)
	const token = "wb-exclude-force-token"
	srv.AddToken(token)
	fetcher, mirrorDir, _ := newRealMirrorFetcher(t, srv, token, []string{"wb-mine"})
	_, err := fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	seedSHA := mustSetLocalRef(t, mirrorDir, "refs/heads/wb-mine", "refs/heads/main")
	require.NoError(t, srv.CreateCollidingBranch(t.Context(), "acme/widgets", "wb-mine", ""))
	require.NoError(t, srv.AdvanceBranch(t.Context(), "acme/widgets", "wb-mine", fakeforge.AdvanceOptions{}))
	require.NoError(t, srv.ForcePushBranch(t.Context(), "acme/widgets", "wb-mine", fakeforge.ForcePushOptions{}))
	_, err = fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	postSHA, err := mirrorRevParse(t, mirrorDir, "refs/heads/wb-mine")
	require.NoError(t, err, "the work-branch ref must still exist in the mirror")
	assert.Equal(t, seedSHA, postSHA, "an upstream force-push of a same-named branch must never touch the excluded work-branch ref")
}

// TestMirrorFetcherExcludesWorkBranchRefFromPrune mirrors the force-push
// test for the prune path: a colliding upstream branch is deleted, and the
// mirror's own work-branch ref of the same name must survive.
//
// Note the SHA-equality assertion below is not the discriminating check it
// looks like: CreateCollidingBranch's empty fromRef resolves upstream
// HEAD, so seedSHA and the (now-deleted) upstream wb-mine's SHA are
// identical by construction, and a broken exclusion here would prune the
// ref away rather than leave it pointing at a different SHA. This test's
// real assertion is the require.NoError on the rev-parse a few lines down
// (the ref must still exist at all) -- assert.Equal is included only for
// symmetry with the force-push test above, where the SHAs genuinely
// differ.
func TestMirrorFetcherExcludesWorkBranchRefFromPrune(t *testing.T) {
	t.Parallel()
	srv := newFakeForgeServer(t)
	const token = "wb-exclude-prune-token"
	srv.AddToken(token)
	fetcher, mirrorDir, _ := newRealMirrorFetcher(t, srv, token, []string{"wb-mine"})
	require.NoError(t, srv.CreateCollidingBranch(t.Context(), "acme/widgets", "wb-mine", ""))
	_, err := fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	seedSHA := mustSetLocalRef(t, mirrorDir, "refs/heads/wb-mine", "refs/heads/main")
	require.NoError(t, srv.DeleteBranch(t.Context(), "acme/widgets", "wb-mine"))
	_, err = fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	postSHA, err := mirrorRevParse(t, mirrorDir, "refs/heads/wb-mine")
	require.NoError(t, err, "the work-branch ref must still exist in the mirror after upstream deletes its same-named branch")
	assert.Equal(t, seedSHA, postSHA, "an upstream deletion of a same-named branch must never prune the excluded work-branch ref")
}
