package fakeforge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
)

// postControl posts body as JSON to path on ts and returns the response,
// registering cleanup for its body.
func postControl(t *testing.T, ts *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+path, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestControlAdvanceBranchOverHTTP(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	before := branchSHA(t, srv, "acme/widgets", "main")
	resp := postControl(t, ts, "/control/advance-branch", advanceBranchRequest{Repo: "acme/widgets", Branch: "main"})
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	after := branchSHA(t, srv, "acme/widgets", "main")
	assert.NotEqual(t, before, after)
	_, err := srv.runGit(ctx, "", "--git-dir="+srv.repoDir("acme/widgets"), "merge-base", "--is-ancestor", before, after)
	assert.NoError(t, err, "advance must append to history, not rewrite it")
}

func TestControlAdvanceBranchUnknownRepoNotFound(t *testing.T) {
	t.Parallel()
	requireGit(t)
	_, ts := newTestServer(t)
	resp := postControl(t, ts, "/control/advance-branch", advanceBranchRequest{Repo: "acme/nope", Branch: "main"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestControlForcePushBranchOverHTTP(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	require.NoError(t, srv.AdvanceBranch(ctx, "acme/widgets", "main", AdvanceOptions{}))
	before := branchSHA(t, srv, "acme/widgets", "main")
	resp := postControl(t, ts, "/control/force-push-branch", forcePushBranchRequest{Repo: "acme/widgets", Branch: "main"})
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	after := branchSHA(t, srv, "acme/widgets", "main")
	assert.NotEqual(t, before, after)
	_, err := srv.runGit(ctx, "", "--git-dir="+srv.repoDir("acme/widgets"), "merge-base", "--is-ancestor", before, after)
	assert.Error(t, err, "force-push must rewrite history, not fast-forward it")
}

func TestControlDeleteBranchOverHTTP(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-temp", ""))
	require.True(t, branchExists(t, srv, "acme/widgets", "wb-temp"))
	resp := postControl(t, ts, "/control/delete-branch", branchRequest{Repo: "acme/widgets", Branch: "wb-temp"})
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.False(t, branchExists(t, srv, "acme/widgets", "wb-temp"))
}

func TestControlCreateBranchOverHTTP(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	mainTip := branchSHA(t, srv, "acme/widgets", "main")
	resp := postControl(t, ts, "/control/create-branch", createBranchRequest{Repo: "acme/widgets", Name: "wb-collide"})
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, mainTip, branchSHA(t, srv, "acme/widgets", "wb-collide"))
}

func TestControlCreateBranchRejectsNonWbPrefix(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	resp := postControl(t, ts, "/control/create-branch", createBranchRequest{Repo: "acme/widgets", Name: "feature-x"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.False(t, branchExists(t, srv, "acme/widgets", "feature-x"))
}

func TestControlMergePRFastForwardOverHTTP(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	require.NoError(t, srv.AdvanceBranch(ctx, "acme/widgets", "wb-feature", AdvanceOptions{Path: "feature.txt", Content: []byte("feature work\n")}))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, number, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "add feature", "desc")
	require.NoError(t, err)
	resp := postControl(t, ts, "/control/merge-pr", prActionRequest{Repo: "acme/widgets", Number: number})
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, branchSHA(t, srv, "acme/widgets", "wb-feature"), branchSHA(t, srv, "acme/widgets", "main"))
	state, err := client.GetPRState(ctx, "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "merged", state)
}

func TestControlClosePROverHTTP(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, number, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "t", "d")
	require.NoError(t, err)
	resp := postControl(t, ts, "/control/close-pr", prActionRequest{Repo: "acme/widgets", Number: number})
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	state, err := client.GetPRState(ctx, "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "closed", state)
}

// TestControlClosePROnMergedIsRejected covers the control API's
// direct-on-forge ClosePR (as opposed to the provider REST path
// Client.ClosePR exercises): closing a PR that a prior /control/merge-pr
// already merged must be rejected with a 412, the same guard
// handleProviderClosePR applies to the provider REST path, per Forgejo
// 9.0.3's verified behavior of rejecting a close on a merged PR rather
// than absorbing it as a no-op.
func TestControlClosePROnMergedIsRejected(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	require.NoError(t, srv.AdvanceBranch(ctx, "acme/widgets", "wb-feature", AdvanceOptions{Path: "feature.txt", Content: []byte("feature work\n")}))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, number, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "add feature", "desc")
	require.NoError(t, err)
	require.NoError(t, srv.MergePR(ctx, "acme/widgets", number))
	resp := postControl(t, ts, "/control/close-pr", prActionRequest{Repo: "acme/widgets", Number: number})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
	state, err := client.GetPRState(ctx, "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "merged", state, "the rejected close through the control API must not regress the PR's state")
}

func TestMergePRThreeWay(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	require.NoError(t, srv.AdvanceBranch(ctx, "acme/widgets", "main", AdvanceOptions{Path: "main-only.txt", Content: []byte("main advance\n")}))
	require.NoError(t, srv.AdvanceBranch(ctx, "acme/widgets", "wb-feature", AdvanceOptions{Path: "feature-only.txt", Content: []byte("feature advance\n")}))
	pr := srv.prs.create("acme/widgets", "wb-feature", "main", "t", "d")
	require.NoError(t, srv.MergePR(ctx, "acme/widgets", pr.number))
	repoDir := srv.repoDir("acme/widgets")
	out, err := srv.runGit(ctx, "", "--git-dir="+repoDir, "rev-list", "--parents", "-1", "main")
	require.NoError(t, err)
	assert.Len(t, strings.Fields(string(out)), 3, "merge commit should have two parents plus itself")
	state, ok := srv.prs.get("acme/widgets", pr.number)
	require.True(t, ok)
	assert.Equal(t, "merged", state.state)
}

func TestMergePRConflict(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"shared.txt": []byte("base\n")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-feature", ""))
	require.NoError(t, srv.AdvanceBranch(ctx, "acme/widgets", "main", AdvanceOptions{Path: "shared.txt", Content: []byte("main change\n")}))
	require.NoError(t, srv.AdvanceBranch(ctx, "acme/widgets", "wb-feature", AdvanceOptions{Path: "shared.txt", Content: []byte("feature change\n")}))
	pr := srv.prs.create("acme/widgets", "wb-feature", "main", "t", "d")
	err := srv.MergePR(ctx, "acme/widgets", pr.number)
	assert.ErrorIs(t, err, errMergeConflict)
}

func TestRemoveRepoAllowsReseedingTheSameName(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("first\n")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-1a2b", ""))
	first := branchSHA(t, srv, "acme/widgets", "main")
	require.NoError(t, srv.RemoveRepo(ctx, "acme/widgets"))
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("second\n")}, SeedOptions{}))
	assert.NotEqual(t, first, branchSHA(t, srv, "acme/widgets", "main"), "re-seeding must build a fresh repo, not reuse the removed one's history")
	_, err := srv.runGit(ctx, "", "--git-dir="+srv.repoDir("acme/widgets"), "rev-parse", "--verify", "refs/heads/wb-1a2b")
	assert.Error(t, err, "the removed repo's branches must not survive into the re-seeded one")
}

func TestRemoveRepoForgetsItsPullRequests(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	srv.AddToken("t0k")
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-1a2b", ""))
	client := NewClient(ts.URL, "t0k")
	_, number, err := client.CreatePR(ctx, "acme/widgets", "wb-1a2b", "main", "t", "d")
	require.NoError(t, err)
	require.NoError(t, srv.MergePR(ctx, "acme/widgets", number))
	require.NoError(t, srv.RemoveRepo(ctx, "acme/widgets"))
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hi\n")}, SeedOptions{}))
	_, err = client.GetPRState(ctx, "acme/widgets", number)
	assert.ErrorIs(t, err, forge.ErrRepoNotFound, "a PR recorded against the removed repo must not answer with its stale merged state")
	require.NoError(t, srv.CreateCollidingBranch(ctx, "acme/widgets", "wb-1a2b", ""))
	_, reused, err := client.CreatePR(ctx, "acme/widgets", "wb-1a2b", "main", "t", "d")
	require.NoError(t, err)
	assert.Equal(t, number, reused, "PR numbering must restart for a re-seeded repo")
}

func TestRemoveRepoUnknownRepoIsNotFound(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	assert.ErrorIs(t, srv.RemoveRepo(t.Context(), "acme/nope"), forge.ErrRepoNotFound)
}
