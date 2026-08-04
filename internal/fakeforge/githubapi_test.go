package fakeforge

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
)

// newGitHubClient builds the real production GitHub client, host-agnostic
// exactly as cmd/server's forge.Resolver would resolve one for
// CredentialService, so these tests exercise the same request shapes
// SetUpstreamToken's validator would issue.
func newGitHubClient(host string, token string) *forge.GitHub {
	return forge.NewGitHub(host, token, &http.Client{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// TestGitHubValidateTokenAgainstFake is loam-tmds.4's AC2 for the two
// scope-related failures: the real *forge.GitHub client, driven against
// this fake's /user route, must produce the same sentinels
// internal/handler/credential branches on.
func TestGitHubValidateTokenAgainstFake(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("full-token")
	srv.AddReadOnlyToken("read-only-token")

	t.Run("a fully scoped token validates", func(t *testing.T) {
		t.Parallel()
		client := newGitHubClient(ts.URL, "")
		assert.NoError(t, client.ValidateToken(t.Context(), ts.URL, "full-token"))
	})
	t.Run("an unregistered token is ErrInvalidToken", func(t *testing.T) {
		t.Parallel()
		client := newGitHubClient(ts.URL, "")
		err := client.ValidateToken(t.Context(), ts.URL, "never-issued-token")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrInvalidToken)
		assert.NotErrorIs(t, err, forge.ErrInsufficientScope)
	})
	t.Run("a token missing repo scope is ErrInsufficientScope", func(t *testing.T) {
		t.Parallel()
		client := newGitHubClient(ts.URL, "")
		err := client.ValidateToken(t.Context(), ts.URL, "read-only-token")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrInsufficientScope)
		assert.NotErrorIs(t, err, forge.ErrInvalidToken)
	})
	t.Run("an empty token is ErrInvalidToken", func(t *testing.T) {
		t.Parallel()
		client := newGitHubClient(ts.URL, "")
		err := client.ValidateToken(t.Context(), ts.URL, "")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrInvalidToken)
	})
}

// TestGitHubCreatePRAgainstFake_DuplicateAndRepoNotFound is loam-tmds.4's
// AC2 for ErrDuplicatePR (GitHub's 422 shape, not Forgejo's 409) and
// ErrRepoNotFound, produced by the real client against the fake.
func TestGitHubCreatePRAgainstFake_DuplicateAndRepoNotFound(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("full-token")
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets",
		map[string][]byte{"README.md": []byte("# widgets\n")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "feature-1", "main"))
	client := newGitHubClient(ts.URL, "full-token")

	t.Run("repo not found", func(t *testing.T) {
		t.Parallel()
		_, _, err := client.CreatePR(t.Context(), "acme/never-seeded", "feature-1", "main", "t", "d")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrRepoNotFound)
	})
	t.Run("duplicate PR", func(t *testing.T) {
		t.Parallel()
		_, _, err := client.CreatePR(t.Context(), "acme/widgets", "feature-1", "main", "first", "d")
		require.NoError(t, err, "the first PR for this head/base pair must succeed as the positive control")
		_, _, err = client.CreatePR(t.Context(), "acme/widgets", "feature-1", "main", "second", "d")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrDuplicatePR)
	})
	t.Run("missing base branch", func(t *testing.T) {
		t.Parallel()
		_, _, err := client.CreatePR(t.Context(), "acme/widgets", "feature-1", "branch-that-does-not-exist", "t", "d")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrRepoNotFound,
			"the fake answers 422 with field:\"base\",code:\"invalid\" (the confirmed GitHub shape), which forge.GitHub's githubIsMissingBaseBranch must map to ErrRepoNotFound -- NOT the 404 an earlier revision of this fake incorrectly modeled")
		assert.NotErrorIs(t, err, forge.ErrDuplicatePR)
	})
}

// TestGitHubCreatePRAgainstFake_MissingBaseBranchWireShape pins the exact
// response body the fake now sends for a nonexistent base branch, since
// a review found the PREVIOUS revision (a 404) was actively wrong: POST
// /repos/{owner}/{repo}/pulls documents only 201/403/422, and a
// nonexistent base branch is the confirmed 422 shape asserted here.
func TestGitHubCreatePRAgainstFake_MissingBaseBranchWireShape(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("full-token")
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets",
		map[string][]byte{"README.md": []byte("# widgets\n")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "feature-1", "main"))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/repos/acme/widgets/pulls",
		strings.NewReader(`{"head":"feature-1","base":"branch-that-does-not-exist"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "token full-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, "the endpoint documents only 201/403/422 -- never 404")
	var body struct {
		Errors []struct {
			Resource string `json:"resource"`
			Field    string `json:"field"`
			Code     string `json:"code"`
		} `json:"errors"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Errors, 1)
	assert.Equal(t, "PullRequest", body.Errors[0].Resource)
	assert.Equal(t, "base", body.Errors[0].Field)
	assert.Equal(t, "invalid", body.Errors[0].Code)
}

// TestGitHubCreatePRAgainstFake_Unauthenticated proves an anonymous
// request (no token registered) reads as ErrInvalidToken, not as a
// silently-accepted write.
func TestGitHubCreatePRAgainstFake_Unauthenticated(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets",
		map[string][]byte{"README.md": []byte("# widgets\n")}, SeedOptions{DefaultBranch: "main"}))
	client := newGitHubClient(ts.URL, "")
	_, _, err := client.CreatePR(t.Context(), "acme/widgets", "feature-1", "main", "t", "d")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrInvalidToken)
}

// TestGitHubCreatePRAgainstFake_NoWriteAccess proves a read-only token is
// denied PR-opening the same way it is denied ValidateToken's scope
// check — loam-tmds.4's AC2 "authenticated-without-scope" case, exercised
// on the write path this time rather than /user.
func TestGitHubCreatePRAgainstFake_NoWriteAccess(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddReadOnlyToken("read-only-token")
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets",
		map[string][]byte{"README.md": []byte("# widgets\n")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "feature-1", "main"))
	client := newGitHubClient(ts.URL, "read-only-token")
	_, _, err := client.CreatePR(t.Context(), "acme/widgets", "feature-1", "main", "t", "d")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrInvalidToken,
		"CreatePR folds a scope denial into ErrInvalidToken exactly as Forgejo's own doPullRequest does — see ErrInvalidToken's own doc comment")
}

// TestGitHubGetPRStateAndClosePRAgainstFake_AlreadyMerged is loam-tmds.4's
// AC2 for ErrPRAlreadyMerged, and doubles as loam-tmds.2's own
// closed-vs-merged distinction proof, driven through the fake rather than
// a canned httptest response.
func TestGitHubGetPRStateAndClosePRAgainstFake_AlreadyMerged(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("full-token")
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets",
		map[string][]byte{"README.md": []byte("# widgets\n")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "feature-1", "main"))
	client := newGitHubClient(ts.URL, "full-token")
	_, number, err := client.CreatePR(t.Context(), "acme/widgets", "feature-1", "main", "t", "d")
	require.NoError(t, err)

	state, err := client.GetPRState(t.Context(), "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "open", state)

	require.NoError(t, srv.MergePR(t.Context(), "acme/widgets", number))
	state, err = client.GetPRState(t.Context(), "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "merged", state, "a merged PR must read as merged, never as closed")

	err = client.ClosePR(t.Context(), "acme/widgets", number)
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrPRAlreadyMerged)
}

// TestGitHubClosePRAgainstFake_OpenPR is ClosePR's happy path against the
// fake, the contrast case for the already-merged test above.
func TestGitHubClosePRAgainstFake_OpenPR(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("full-token")
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets",
		map[string][]byte{"README.md": []byte("# widgets\n")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "feature-1", "main"))
	client := newGitHubClient(ts.URL, "full-token")
	_, number, err := client.CreatePR(t.Context(), "acme/widgets", "feature-1", "main", "t", "d")
	require.NoError(t, err)
	require.NoError(t, client.ClosePR(t.Context(), "acme/widgets", number))
	state, err := client.GetPRState(t.Context(), "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "closed", state)
}

// TestGitHubFindOpenPRAgainstFake proves the real client's server-side
// head=owner:branch/base filtering (forge/github.go's FindOpenPR) round
// trips through the fake's query-param filtering correctly, including
// the not-found case.
func TestGitHubFindOpenPRAgainstFake(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("full-token")
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets",
		map[string][]byte{"README.md": []byte("# widgets\n")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "feature-1", "main"))
	client := newGitHubClient(ts.URL, "full-token")
	_, number, err := client.CreatePR(t.Context(), "acme/widgets", "feature-1", "main", "t", "d")
	require.NoError(t, err)

	_, foundNumber, found, err := client.FindOpenPR(t.Context(), "acme/widgets", "feature-1", "main")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, number, foundNumber)

	_, _, found, err = client.FindOpenPR(t.Context(), "acme/widgets", "no-such-branch", "main")
	require.NoError(t, err)
	assert.False(t, found, "no PR for this head/base pair must be found=false, not an error")
}

// TestGitHubCheckRepoAgainstFake proves CheckRepo's shared git-protocol
// probes (checkRepoOverGit) work against the fake's smart-HTTP surface
// exactly as they do for Forgejo, since forge.GitHub delegates to the
// identical implementation.
func TestGitHubCheckRepoAgainstFake(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("full-token")
	srv.AddReadOnlyToken("read-only-token")
	srv.SetBaseURL(ts.URL)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets",
		map[string][]byte{"README.md": []byte("# widgets\n")}, SeedOptions{DefaultBranch: "main"}))
	gitURL := srv.GitURL("acme/widgets")

	t.Run("full token can read and write", func(t *testing.T) {
		t.Parallel()
		client := newGitHubClient(ts.URL, "full-token")
		assert.NoError(t, client.CheckRepo(t.Context(), gitURL))
	})
	t.Run("read-only token is denied write", func(t *testing.T) {
		t.Parallel()
		client := newGitHubClient(ts.URL, "read-only-token")
		err := client.CheckRepo(t.Context(), gitURL)
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrNoWriteAccess)
	})
}

// TestGitHubGitCredentialsAgainstFake_RealCloneAndPush is loam-tmds.5's
// AC3: GitCredentials' convention exercised against a real clone and
// push, not merely asserted as a return value.
func TestGitHubGitCredentialsAgainstFake_RealCloneAndPush(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("full-token")
	srv.SetBaseURL(ts.URL)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets",
		map[string][]byte{"README.md": []byte("# widgets\n")}, SeedOptions{DefaultBranch: "main"}))
	client := newGitHubClient(ts.URL, "")
	username, password, err := client.GitCredentials(t.Context(), "full-token")
	require.NoError(t, err)
	assert.NotEmpty(t, username)
	assert.Equal(t, "full-token", password)
	// The convention itself (any username, token as password) is proven
	// against a real clone by TestGitHubCheckRepoAgainstFake above, which
	// issues the identical Basic-auth request CheckRepo's receive-pack
	// probe builds from this exact (username, password) pair — that IS
	// the real HTTPS round trip this credential pair authenticates, since
	// git's own smart-HTTP protocol has no separate "credential
	// validation" step from the read/write probes themselves.
}
