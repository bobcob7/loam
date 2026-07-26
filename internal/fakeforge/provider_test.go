package fakeforge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.False(t, branchExists(t, srv, "acme/widgets", "wb-never-created"))
}

// TestClientCreatePRRejectsNonexistentTargetBranch is the target-branch
// half of the same validation: the head branch is real, but the target is
// not.
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
	assert.ErrorIs(t, err, errBranchNotFound)
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

// TestProviderClosePRMergedIsNoOp covers the third defect: closing a PR that
// has already merged must not flip it to "closed", since real Forgejo's
// issue model treats a merged PR as permanently concluded rather than a
// re-closeable open issue. This matters for loam-giq.8's best-effort
// close-after-merge path: if sync detects the merge and defensively closes
// the same PR, the recorded state must still read "merged" afterward, not
// regress to "closed".
func TestProviderClosePRMergedIsNoOp(t *testing.T) {
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
	require.NoError(t, client.ClosePR(ctx, "acme/widgets", number))
	state, err = client.GetPRState(ctx, "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "merged", state, "closing an already-merged PR must not regress its state")
	assert.Equal(t, mergedTip, branchSHA(t, srv, "acme/widgets", "main"), "closing must not touch git state")
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
