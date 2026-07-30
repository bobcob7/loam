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
	seedSHA := mustSetLocalRef(t, mirrorDir, "refs/heads/loam-reserved/wb-mine", "refs/heads/main")
	require.NoError(t, srv.CreateCollidingBranch(t.Context(), "acme/widgets", "wb-mine", ""))
	require.NoError(t, srv.AdvanceBranch(t.Context(), "acme/widgets", "wb-mine", fakeforge.AdvanceOptions{}))
	require.NoError(t, srv.ForcePushBranch(t.Context(), "acme/widgets", "wb-mine", fakeforge.ForcePushOptions{}))
	_, err = fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	postSHA, err := mirrorRevParse(t, mirrorDir, "refs/heads/loam-reserved/wb-mine")
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
	seedSHA := mustSetLocalRef(t, mirrorDir, "refs/heads/loam-reserved/wb-mine", "refs/heads/main")
	require.NoError(t, srv.DeleteBranch(t.Context(), "acme/widgets", "wb-mine"))
	_, err = fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	postSHA, err := mirrorRevParse(t, mirrorDir, "refs/heads/loam-reserved/wb-mine")
	require.NoError(t, err, "the work-branch ref must still exist in the mirror after upstream deletes its same-named branch")
	assert.Equal(t, seedSHA, postSHA, "an upstream deletion of a same-named branch must never prune the excluded work-branch ref")
}

// TestMirrorFetcherNarrowsPositiveRefspecToBranchesAndTags is loam-5f3's
// whole point: seed an upstream carrying refs/pull/* and refs/replace/*
// (namespaces `git clone --mirror`'s own "+refs/*:refs/*" would carry into
// the mirror) alongside an ordinary tag, fetch, and assert the tag and
// the target branch arrived while both non-branch/tag namespaces did not.
// A test that only checked the branch still arrives would have passed
// just as well before this bead's change -- the branch was never in
// question; the absence of refs/pull and refs/replace is.
func TestMirrorFetcherNarrowsPositiveRefspecToBranchesAndTags(t *testing.T) {
	t.Parallel()
	srv := newFakeForgeServer(t)
	const token = "narrow-refspec-test-token"
	srv.AddToken(token)
	fetcher, mirrorDir, _ := newRealMirrorFetcher(t, srv, token, nil)
	const replaceRef = "refs/replace/0000000000000000000000000000000000000001"
	require.NoError(t, srv.CreateRef(t.Context(), "acme/widgets", "refs/tags/v1", ""))
	require.NoError(t, srv.CreateRef(t.Context(), "acme/widgets", "refs/pull/1/head", ""))
	require.NoError(t, srv.CreateRef(t.Context(), "acme/widgets", replaceRef, ""))

	_, err := fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)

	_, err = mirrorRevParse(t, mirrorDir, "refs/heads/main")
	require.NoError(t, err, "the target branch must still be fetched")
	_, err = mirrorRevParse(t, mirrorDir, "refs/tags/v1")
	require.NoError(t, err, "tags must still be fetched")
	_, err = mirrorRevParse(t, mirrorDir, "refs/pull/1/head")
	assert.Error(t, err, "refs/pull/* must be absent from the mirror -- it is outside the narrowed positive refspec")
	_, err = mirrorRevParse(t, mirrorDir, replaceRef)
	assert.Error(t, err, "refs/replace/* must be absent from the mirror -- it is outside the narrowed positive refspec")
}

// TestMirrorFetcherProtectsAnUNREGISTEREDReservedRefFromPrune is loam-cmq's
// whole point, and it is the one test in this file the ENUMERATED
// exclusions cannot pass.
//
// The scenario is the TOCTOU window itself, reproduced exactly: a
// work-branch ref exists in the mirror, has NEVER existed upstream, and is
// NOT in the fetcher's exclusion list -- which is precisely the state of
// every ref created after ResolveRepo returned and before the fetch's argv
// finished executing. Note workBranchNames is nil below: the fetcher is
// told about no work branches at all, so nothing enumerated protects this
// ref. Only refnames.ReservedExclusionRefspec does.
//
// Without that glob the ref is DELETED -- verified against real git 2.50.1,
// which reports "- <sha> 000...0 refs/heads/<name>" for it -- and the
// deletion is unrecoverable, since work_branches has no SHA column and a
// bare mirror has no reflog. The control ref seeded alongside it (an
// ordinary local ref OUTSIDE the reserved namespace) is what proves this
// test would actually notice: it is pruned in the same fetch, so a fetch
// that quietly stopped pruning anything at all could not make this test
// pass by accident.
//
// A test asserting on buildFetchRefspecs' returned STRING would prove
// nothing here: the claim is about what git does with that string, not
// about its spelling.
func TestMirrorFetcherProtectsAnUNREGISTEREDReservedRefFromPrune(t *testing.T) {
	t.Parallel()
	srv := newFakeForgeServer(t)
	const token = "reserved-glob-prune-token"
	srv.AddToken(token)
	fetcher, mirrorDir, _ := newRealMirrorFetcher(t, srv, token, nil)
	_, err := fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)
	reservedSHA := mustSetLocalRef(t, mirrorDir, "refs/heads/loam-reserved/wb-brandnew", "refs/heads/main")
	controlSHA := mustSetLocalRef(t, mirrorDir, "refs/heads/wb-brandnew", "refs/heads/main")
	require.Equal(t, reservedSHA, controlSHA, "precondition: both seeded refs start at main's tip")

	_, err = fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.NoError(t, err)

	postSHA, err := mirrorRevParse(t, mirrorDir, "refs/heads/loam-reserved/wb-brandnew")
	require.NoError(t, err, "a ref in the reserved namespace must survive a pruning fetch even though it is in no exclusion list and never existed upstream")
	assert.Equal(t, reservedSHA, postSHA)
	_, err = mirrorRevParse(t, mirrorDir, "refs/heads/wb-brandnew")
	assert.Error(t, err, "control: the identical ref OUTSIDE the reserved namespace is pruned, so this fetch really does prune")
}

// TestMirrorFetcherFetch_UpstreamURLHasUserinfo_SurfacesActionableRepoNamedError
// is loam-ra1k's part (b) proven end-to-end through a REAL
// gittransport.Transport, standing in for repos.upstream_url still
// carrying a credential from before loam-ys1's transport-level rejection
// existed: repos.ResolveRepo (staticRepoResolver here) hands MirrorFetcher
// exactly that stored URL on every scheduled sync tick, precisely as
// internal/mirrorsync/scheduler.go's runSteps does in production, with no
// probe/enroll-time validation in the way to have caught it earlier.
// Transport's own validateUpstreamURL rejects the fetch before any git
// subprocess or network call, so no fakeforge server or credential is
// needed here at all -- the point is what Fetch's returned error contains:
// the repo name (mirrorsync's own "fetching repo %s"/"mirror-fetching repo
// %s" wraps, the same text an admin sees via GetRepo/ListRepos'
// sync.error, per loam-ra1k's audit of internal/mirrorsync/state's
// Reporter.ReportError, which persists err.Error() verbatim) and the
// shared, actionable gittransport.ErrUpstreamURLHasUserinfo sentinel --
// never the embedded credential itself.
func TestMirrorFetcherFetch_UpstreamURLHasUserinfo_SurfacesActionableRepoNamedError(t *testing.T) {
	t.Parallel()
	const poisoned = "https://user:leaked-token@forge.example.invalid/acme/widgets.git"
	transport := gittransport.New(&staticCredentialSource{token: "unused"}, fakeforge.NewClient("", ""), testLogger())
	resolver := &staticRepoResolver{host: "forge.example.invalid", upstreamURL: poisoned}
	fetcher := NewMirrorFetcher(t.TempDir(), transport, resolver)
	_, err := fetcher.Fetch(t.Context(), RepoID("acme/widgets"))
	require.Error(t, err)
	assert.ErrorIs(t, err, gittransport.ErrUpstreamURLHasUserinfo)
	assert.Contains(t, err.Error(), "acme/widgets", "the error must name the repo so an admin scanning sync_error across every enrolled repo knows which one to fix")
	assert.NotContains(t, err.Error(), "leaked-token", "a pre-existing row's embedded credential must never leak into the sync error an admin sees")
}
