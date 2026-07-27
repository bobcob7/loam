package fakeforge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
)

func TestClientValidateToken(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("good-token")
	client := NewClient(ts.URL, "good-token")
	assert.NoError(t, client.ValidateToken(t.Context(), "example.invalid", "good-token"))
	assert.ErrorIs(t, client.ValidateToken(t.Context(), "example.invalid", "bad-token"), errUnauthorized)
}

// TestClientValidateTokenMissingPRScope covers ValidateToken's "the token
// authenticates but lacks the scopes needed to open PRs" case (loam-li0.9's
// design names this as a required ValidateToken scenario), distinct from an
// entirely unregistered token.
func TestClientValidateTokenMissingPRScope(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddTokenWithoutPRScope("push-only-token")
	client := NewClient(ts.URL, "push-only-token")
	err := client.ValidateToken(t.Context(), "example.invalid", "push-only-token")
	assert.ErrorIs(t, err, errMissingScope)
}

func TestClientCheckRepo(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	srv.AddToken("rw-token")
	srv.AddReadOnlyToken("ro-token")
	rw := NewClient(ts.URL, "rw-token")
	assert.NoError(t, rw.CheckRepo(ctx, srv.GitURL("acme/widgets")))
	ro := NewClient(ts.URL, "ro-token")
	assert.ErrorIs(t, ro.CheckRepo(ctx, srv.GitURL("acme/widgets")), errNoWriteAccess)
	assert.ErrorIs(t, rw.CheckRepo(ctx, srv.GitURL("acme/nope")), errRepoNotFound)
}

// TestClientCheckRepoUnregisteredTokenLooksLikeRepoNotFound documents that
// CheckRepo probes the git surface directly (docs/sync-spec.md → Upstream
// Transport: an authenticated ls-remote for read, a receive-pack probe for
// write) rather than a side-channel REST call. From outside, a read probe
// that 401s because the credential is garbage is indistinguishable from one
// that 404s because the repo doesn't exist, so both are classified as
// errRepoNotFound — matching the real provider's behavior verified against
// this fake by the loam-li0.9 contract suite.
func TestClientCheckRepoUnregisteredTokenLooksLikeRepoNotFound(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	unauthed := NewClient(ts.URL, "never-registered")
	assert.ErrorIs(t, unauthed.CheckRepo(ctx, srv.GitURL("acme/widgets")), errRepoNotFound)
}

func TestClientCreatePRGetStateClosePR(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	prURL, number, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "title", "desc")
	require.NoError(t, err)
	assert.NotEmpty(t, prURL)
	assert.Equal(t, 1, number)
	state, err := client.GetPRState(ctx, "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "open", state)
	require.NoError(t, client.ClosePR(ctx, "acme/widgets", number))
	state, err = client.GetPRState(ctx, "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "closed", state)
	_, err = client.GetPRState(ctx, "acme/widgets", 999)
	assert.ErrorIs(t, err, errPRNotFound)
}

// TestClientPRLookupAgainstNonexistentPRIsForgeErrRepoNotFound covers
// loam-hza's negative PR-lookup row: real Forgejo 9.0.3 maps EVERY 404 from
// the pulls endpoints to ErrRepoNotFound, including a nonexistent PR number
// against a repo that exists (verified empirically — GET and PATCH
// .../pulls/{number} for a missing number return the identical generic 404
// body a missing repo would), so GetPRState and ClosePR against a PR number
// that was never created must both match forge.ErrRepoNotFound on the fake
// too, not just the fake-internal errPRNotFound.
func TestClientPRLookupAgainstNonexistentPRIsForgeErrRepoNotFound(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, err := client.GetPRState(ctx, "acme/widgets", 999)
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrRepoNotFound)
	err = client.ClosePR(ctx, "acme/widgets", 999)
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrRepoNotFound)
}

func TestClientCreatePRRepoNotFound(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, _, err := client.CreatePR(t.Context(), "acme/nope", "wb-x", "main", "t", "d")
	assert.ErrorIs(t, err, errRepoNotFound)
}

// TestClientCreatePRRejectsNonexistentHeadBranch covers the first defect
// this bead fixes: real Forgejo rejects opening a PR whose head branch does
// not exist, but the fake previously checked only that the repo directory
// existed (control_test.go's ClosePR-over-HTTP case exercised this gap by
// accident, passing with a head branch that was never created).
//
// This must NOT match forge.ErrRepoNotFound (loam-9qu): verified
// empirically against Forgejo 9.0.3, a nonexistent HEAD branch on CreatePR
// 500s there with a leaked git error ("exit status 128: ... fatal: bad
// revision") instead of 404ing — an apparent upstream bug distinct from the
// target-branch case below, which DOES 404 correctly. Mimicking the 500
// would propagate that bug into the fake for no test value; the fake keeps
// rejecting with a plain 404 (satisfying loam-5rh) but does not claim the
// forge.ErrRepoNotFound class real Forgejo cannot actually deliver here.
// loam-li0.9's shared contract table must not add a head-branch-not-found
// row expecting identical error classes across the fake and real Forgejo.
func TestClientCreatePRRejectsNonexistentHeadBranch(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, _, err := client.CreatePR(ctx, "acme/widgets", "wb-never-created", "main", "t", "d")
	require.Error(t, err)
	assert.ErrorIs(t, err, errBranchNotFound)
	assert.NotErrorIs(t, err, forge.ErrRepoNotFound, "a nonexistent head branch has no forge-sentinel parity with real Forgejo 9.0.3 — see loam-9qu")
	assert.False(t, branchExists(t, srv, "acme/widgets", "wb-never-created"))
}

// TestClientCreatePRRejectsNonexistentTargetBranch is the target-branch
// half of the same validation: the head branch is real, but the target is
// not. Unlike the head-branch case above, this DOES match
// forge.ErrRepoNotFound: verified empirically against Forgejo 9.0.3, a
// nonexistent base/target branch on CreatePR correctly 404s with
// {"message":"BaseNotExist"}, which doPullRequest folds into
// ErrRepoNotFound the same as a missing repo or PR (loam-hza/loam-9qu).
func TestClientCreatePRRejectsNonexistentTargetBranch(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, _, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "release", "t", "d")
	require.Error(t, err)
	assert.ErrorIs(t, err, errTargetBranchNotFound)
	assert.ErrorIs(t, err, forge.ErrRepoNotFound)
}

// TestClientCreatePRDuplicateOpenReturnsConflict covers the second, most
// consequential defect: a repeat CreatePR for the same head/target pair
// while the first is still open must fail with a conflict instead of
// minting a fresh PR number, matching real Forgejo (which rejects a second
// PR for an already-open head/target pair) and giving loam-giq.7's
// idempotent CreatePR implementation something that can actually fail.
func TestClientCreatePRDuplicateOpenReturnsConflict(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, first, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t", "d")
	require.NoError(t, err)
	_, _, err = client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t2", "d2")
	require.Error(t, err)
	assert.ErrorIs(t, err, errPRExists)
	assert.ErrorIs(t, err, forge.ErrDuplicatePR, "verified against Forgejo 9.0.3: a repeat CreatePR for an open head/target pair returns 409, which doPullRequest maps to ErrDuplicatePR (loam-hza)")
	state, stateErr := client.GetPRState(ctx, "acme/widgets", first)
	require.NoError(t, stateErr)
	assert.Equal(t, "open", state, "the original PR must be untouched by the rejected duplicate")
}

// TestClientCreatePRAfterCloseAllowsNewPR proves the duplicate check is
// scoped to open PRs only: once the original concludes (here, via a close),
// a new CreatePR for the same head/target pair is a fresh, legitimate PR,
// the same way a real forge lets you re-open after a prior PR closed.
func TestClientCreatePRAfterCloseAllowsNewPR(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, first, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t", "d")
	require.NoError(t, err)
	require.NoError(t, client.ClosePR(ctx, "acme/widgets", first))
	_, second, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t", "d")
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
}

// TestCreatePRTwiceForSameHeadTargetFailsNonIdempotentCaller is this bead's
// mutation-test deliverable: it plays the part of a deliberately
// NON-IDEMPOTENT CreatePR caller — exactly the bug loam-giq.7's idempotent
// CreatePR implementation must not have — by invoking CreatePR twice for
// the same head/target pair and asserting the second call fails with a
// conflict rather than silently minting a second PR number. Before this
// bead's fix, both calls succeeded with two different PR numbers; a
// non-idempotent CreatePR implementation would have sailed through
// loam-giq.7's tests against the unfixed fake.
func TestCreatePRTwiceForSameHeadTargetFailsNonIdempotentCaller(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	nonIdempotentCreatePR := func() (string, int, error) {
		return client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t", "d")
	}
	_, firstNumber, err := nonIdempotentCreatePR()
	require.NoError(t, err)
	_, secondNumber, err := nonIdempotentCreatePR()
	require.Error(t, err, "a naive, non-idempotent CreatePR caller repeating the same request must be rejected")
	assert.ErrorIs(t, err, errPRExists)
	assert.Zero(t, secondNumber, "no second PR number should be issued for the rejected duplicate")
	assert.NotEqual(t, 0, firstNumber)
}

// TestClientFindOpenPRFindsExistingOpenPR covers FindOpenPR's core case: an
// open PR already exists for the exact head/target pair, and the returned
// URL/number match what CreatePR itself returned for that PR.
func TestClientFindOpenPRFindsExistingOpenPR(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	wantURL, wantNumber, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t", "d")
	require.NoError(t, err)
	gotURL, gotNumber, found, err := client.FindOpenPR(ctx, "acme/widgets", "wb-feature", "main")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, wantURL, gotURL)
	assert.Equal(t, wantNumber, gotNumber)
}

// TestClientFindOpenPRNotFoundWhenNoMatch covers the not-found case: a repo
// that exists but has no PR at all for the requested head/target pair.
// found=false with a nil error, not an error.
func TestClientFindOpenPRNotFoundWhenNoMatch(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, _, found, err := client.FindOpenPR(ctx, "acme/widgets", "wb-feature", "main")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestClientFindOpenPRIgnoresClosedPR covers a head/target pair whose only
// PR has since closed: matching prRegistry.findOpen's own semantics (a
// closed or merged PR does not count), so a caller must be free to open a
// fresh PR for the pair rather than find a stale one.
func TestClientFindOpenPRIgnoresClosedPR(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, number, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t", "d")
	require.NoError(t, err)
	require.NoError(t, client.ClosePR(ctx, "acme/widgets", number))
	_, _, found, err := client.FindOpenPR(ctx, "acme/widgets", "wb-feature", "main")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestClientFindOpenPRAdoptsAfterDuplicateConflict is the scenario loam-46g
// exists for (loam-giq.7's retry path): CreatePR fails with ErrDuplicatePR
// because a prior attempt already opened the PR, and FindOpenPR recovers
// that PR's real per-repo number without parsing the 409's message.
func TestClientFindOpenPRAdoptsAfterDuplicateConflict(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	wantURL, wantNumber, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t", "d")
	require.NoError(t, err)
	_, _, err = client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t2", "d2")
	require.Error(t, err)
	require.ErrorIs(t, err, forge.ErrDuplicatePR)
	gotURL, gotNumber, found, findErr := client.FindOpenPR(ctx, "acme/widgets", "wb-feature", "main")
	require.NoError(t, findErr)
	assert.True(t, found)
	assert.Equal(t, wantNumber, gotNumber, "adoption must recover the per-repo number, not the 409 message's internal id")
	assert.Equal(t, wantURL, gotURL)
}

// TestClientFindOpenPRRepoNotFoundIsForgeErrRepoNotFound covers a repo that
// was never seeded, matching the real Forgejo client's FindOpenPR mapping a
// 404 from the list endpoint to ErrRepoNotFound.
func TestClientFindOpenPRRepoNotFoundIsForgeErrRepoNotFound(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, _, found, err := client.FindOpenPR(t.Context(), "acme/nope", "wb-feature", "main")
	require.Error(t, err)
	assert.False(t, found)
	assert.ErrorIs(t, err, forge.ErrRepoNotFound)
}

// TestProviderClosePRMergedIsRejected covers the third defect: closing a PR
// that has already merged must not flip it to "closed". Verified against
// Forgejo 9.0.3, PATCH .../pulls/{merged} {"state":"closed"} returns 412
// Precondition Failed with state unchanged — merging is a one-way
// transition, not a form of "already closed" that a later close silently
// absorbs. loam-giq.8's best-effort close-after-merge path must special-
// case errPRMerged rather than assume the fake (or a real forge) treats
// this as success.
func TestProviderClosePRMergedIsRejected(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	require.NoError(t, srv.AdvanceBranch(ctx, "acme/widgets", "wb-feature", AdvanceOptions{Path: "feature.txt", Content: []byte("feature work\n")}))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, number, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t", "d")
	require.NoError(t, err)
	require.NoError(t, srv.MergePR(ctx, "acme/widgets", number))
	mergedTip := branchSHA(t, srv, "acme/widgets", "main")
	state, err := client.GetPRState(ctx, "acme/widgets", number)
	require.NoError(t, err)
	require.Equal(t, "merged", state)
	err = client.ClosePR(ctx, "acme/widgets", number)
	require.Error(t, err, "closing an already-merged PR must be rejected, not silently succeed")
	assert.ErrorIs(t, err, errPRMerged)
	assert.ErrorIs(t, err, forge.ErrPRAlreadyMerged, "a Client caller matches the forge-level class, not fakeforge's own sentinel: internal/mirrorsync's ClosePRAndCleanup treats exactly this class as success-equivalent")
	state, err = client.GetPRState(ctx, "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "merged", state, "the rejected close must not regress the PR's state")
	assert.Equal(t, mergedTip, branchSHA(t, srv, "acme/widgets", "main"), "the rejected close must not touch git state")
}

func TestClientGitCredentials(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	client := NewClient(ts.URL, "some-token")
	user, pass, err := client.GitCredentials(t.Context(), "some-token")
	require.NoError(t, err)
	assert.NotEmpty(t, user)
	assert.Equal(t, "some-token", pass)
}

// TestClientGitCredentialsEmptyTokenIsForgeErrInvalidToken covers
// loam-hza's second divergence: the fake previously returned
// ("fakeforge", "", nil) for an empty token with no error at all, while
// Forgejo.GitCredentials (internal/forge/forgejo.go) rejects an empty
// token with ErrInvalidToken before doing anything else. GitCredentials
// is a fixed local convention, not a network call, so this is a
// same-process comparison rather than something to verify against a live
// Forgejo container — but it is exactly the kind of row loam-li0.9's
// shared table would need to run unbranched against both providers.
func TestClientGitCredentialsEmptyTokenIsForgeErrInvalidToken(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	client := NewClient(ts.URL, "some-token")
	user, pass, err := client.GitCredentials(t.Context(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrInvalidToken)
	assert.Empty(t, user)
	assert.Empty(t, pass)
}
