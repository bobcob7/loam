package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/forge"
)

const (
	testForgejoToken = "demoenv-forgejo-shim-token"
	testForgejoRepo  = "loam-demo/doc-server"
	testHeadBranch   = "loam/wb-abc123"
	testTargetBranch = "main"
)

// startShim stands up one fakeforge.Server behind the demo's Forgejo REST
// shim on a real httptest listener, seeded with a repo carrying the target
// and head branches a pull request needs, and returns the base URL.
//
// The handler is installed through an indirection because the shim needs
// the server's own URL at construction (it talks to the fake's provider
// surface over loopback) and httptest only publishes that URL once the
// listener is up -- the same ordering runUpstreamM5 resolves by
// constructing the shim after net.Listen returns.
func startShim(t *testing.T) string {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	server, err := fakeforge.New(logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	server.AddToken(testForgejoToken)
	var handler http.Handler = server
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpSrv.Close)
	server.SetBaseURL(httpSrv.URL)
	handler = newForgejoAPI(server, httpSrv.URL, testForgejoToken, logger)
	require.NoError(t, server.SeedRepoFiles(t.Context(), testForgejoRepo,
		map[string][]byte{"README.md": []byte("# doc-server\n")},
		fakeforge.SeedOptions{DefaultBranch: testTargetBranch}))
	// The head branch has to exist upstream before a PR can be opened from
	// it, exactly as it does in the demo, where the accept's push creates
	// it moments before CreatePR runs. CreateCollidingBranch is the fake's
	// only branch-creating control call and it insists on a "wb-" prefix,
	// so the head is created under that name and the PR is opened from it.
	require.NoError(t, server.CreateCollidingBranch(t.Context(), testForgejoRepo, "wb-abc123", ""))
	return httpSrv.URL
}

// productionClient is a real *forge.Forgejo -- the very type
// cmd/server/sync.go's forgePRTracker builds -- pointed at the shim.
func productionClient(baseURL string) *forge.Forgejo {
	return forge.NewForgejo(baseURL, testForgejoToken, &http.Client{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// TestForgejoAPI_ProductionClientRoundTrip is the assertion the whole shim
// exists for: the REAL forge.Forgejo client -- not a fakeforge.Client, not
// a stub -- completes every pull-request operation demo:m5 performs
// against it, and reads back what it wrote.
func TestForgejoAPI_ProductionClientRoundTrip(t *testing.T) {
	t.Parallel()
	baseURL := startShim(t)
	client := productionClient(baseURL)
	ctx := t.Context()
	url, number, err := client.CreatePR(ctx, testForgejoRepo, "wb-abc123", testTargetBranch, "A title", "A body")
	require.NoError(t, err)
	assert.Positive(t, number)
	assert.NotEmpty(t, url)
	state, err := client.GetPRState(ctx, testForgejoRepo, number)
	require.NoError(t, err)
	assert.Equal(t, "open", state)
	foundURL, foundNumber, found, err := client.FindOpenPR(ctx, testForgejoRepo, "wb-abc123", testTargetBranch)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, number, foundNumber)
	assert.Equal(t, url, foundURL)
	_, _, otherFound, err := client.FindOpenPR(ctx, testForgejoRepo, "wb-abc123", "no-such-branch")
	require.NoError(t, err)
	assert.False(t, otherFound, "FindOpenPR filters on head AND base client-side; a mismatched base must not match")
	require.NoError(t, client.ClosePR(ctx, testForgejoRepo, number))
	state, err = client.GetPRState(ctx, testForgejoRepo, number)
	require.NoError(t, err)
	assert.Equal(t, "closed", state)
}

// TestForgejoAPI_MergeReadsBackAsMerged pins the encoding demo:m5's
// completion assertion depends on. A merge performed through the fake's
// own control API -- behind the shim's back -- must surface to the
// production client as "merged", which only works if the shim emits
// Forgejo's two-field encoding (state "closed" WITH merged true) and
// GetPRState folds it back.
func TestForgejoAPI_MergeReadsBackAsMerged(t *testing.T) {
	t.Parallel()
	baseURL := startShim(t)
	client := productionClient(baseURL)
	ctx := t.Context()
	_, number, err := client.CreatePR(ctx, testForgejoRepo, "wb-abc123", testTargetBranch, "A title", "A body")
	require.NoError(t, err)
	require.NoError(t, mergeThroughControlAPI(ctx, baseURL, testForgejoRepo, number))
	state, err := client.GetPRState(ctx, testForgejoRepo, number)
	require.NoError(t, err)
	assert.Equal(t, "merged", state)
	_, _, found, err := client.FindOpenPR(ctx, testForgejoRepo, "wb-abc123", testTargetBranch)
	require.NoError(t, err)
	assert.False(t, found, "a merged PR is no longer open, so the open list must not carry it")
	err = client.ClosePR(ctx, testForgejoRepo, number)
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrPRAlreadyMerged, "merging is a one-way transition; closing a merged PR is a 412")
}

// TestForgejoAPI_DuplicateIsAConflict pins the classification the
// accepter's adopt-the-existing-PR path turns on: a repeat CreatePR for a
// head/target pair that already has an open PR must reach the production
// client as forge.ErrDuplicatePR, not as an opaque failure.
func TestForgejoAPI_DuplicateIsAConflict(t *testing.T) {
	t.Parallel()
	client := productionClient(startShim(t))
	ctx := t.Context()
	_, _, err := client.CreatePR(ctx, testForgejoRepo, "wb-abc123", testTargetBranch, "A title", "A body")
	require.NoError(t, err)
	_, _, err = client.CreatePR(ctx, testForgejoRepo, "wb-abc123", testTargetBranch, "A title", "A body")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrDuplicatePR)
}

// TestForgejoAPI_UnknownRepoIsRepoNotFound pins the other classification:
// every 404 from the pulls endpoints is ErrRepoNotFound to the production
// client, which is what real Forgejo 9.0.3 does.
func TestForgejoAPI_UnknownRepoIsRepoNotFound(t *testing.T) {
	t.Parallel()
	client := productionClient(startShim(t))
	_, _, err := client.CreatePR(t.Context(), "nobody/nothing", "wb-abc123", testTargetBranch, "t", "b")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrRepoNotFound)
}

// TestForgejoAPI_ValidateTokenClassifiesBothWays pins the probe
// forge.Forgejo.ValidateToken makes: a POST with no body against a
// never-existing repo path. A good token must read as valid (the 404 is
// accompanied by a genuine error envelope, which is what distinguishes
// "the API evaluated this" from "nothing here 404s everything"), and an
// unregistered token must read as ErrInvalidToken.
//
// Nothing in demo:m5 calls ValidateToken -- the credential is seeded
// directly -- so this is not load-bearing for the demo. It is here because
// the shim's error envelope is: if it stopped emitting `message`,
// ValidateToken would start reporting a live forge as unclassifiable, and
// this is the only test that would notice.
func TestForgejoAPI_ValidateTokenClassifiesBothWays(t *testing.T) {
	t.Parallel()
	baseURL := startShim(t)
	require.NoError(t, productionClient(baseURL).ValidateToken(t.Context(), baseURL, testForgejoToken))
	bad := forge.NewForgejo(baseURL, "not-a-real-token", &http.Client{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	err := bad.ValidateToken(t.Context(), baseURL, "not-a-real-token")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrInvalidToken)
}

// TestForgejoAPI_ListStateAll is the endpoint demo:m5's "no second PR"
// assertion reads. state=all must keep returning a PR after it has left
// the open list, because a count taken against state=open would report
// zero once the PR merged and pass for the wrong reason.
func TestForgejoAPI_ListStateAll(t *testing.T) {
	t.Parallel()
	baseURL := startShim(t)
	client := productionClient(baseURL)
	ctx := t.Context()
	_, number, err := client.CreatePR(ctx, testForgejoRepo, "wb-abc123", testTargetBranch, "A title", "A body")
	require.NoError(t, err)
	open := listPRs(t, baseURL, "open")
	require.Len(t, open, 1)
	assert.Equal(t, "A body", open[0].Body, "the body is only readable back through the shim's own memo; the fake's provider surface has no read-back for it")
	assert.Equal(t, "A title", open[0].Title)
	assert.Equal(t, "wb-abc123", open[0].Head.Ref)
	require.NoError(t, mergeThroughControlAPI(ctx, baseURL, testForgejoRepo, number))
	assert.Empty(t, listPRs(t, baseURL, "open"), "a merged PR leaves the open list")
	all := listPRs(t, baseURL, "all")
	require.Len(t, all, 1, "state=all must still carry every PR the repo ever had")
	assert.Equal(t, number, all[0].Number)
	assert.True(t, all[0].Merged)
	assert.Equal(t, "closed", all[0].State)
}

// TestForgejoAPI_RejectsAnUnauthenticatedRequest pins the token gate: the
// shim is a stand-in for a real forge, and a real forge does not answer
// pull-request calls to an anonymous caller.
func TestForgejoAPI_RejectsAnUnauthenticatedRequest(t *testing.T) {
	t.Parallel()
	baseURL := startShim(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		baseURL+"/api/v1/repos/"+testForgejoRepo+"/pulls", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestForgejoAPI_PassesEverythingElseThrough pins the fall-through: the
// git smart-HTTP surface and the /control/* test API are reachable
// unchanged with the shim mounted, which is what lets the demo push to the
// same host it opens pull requests against.
func TestForgejoAPI_PassesEverythingElseThrough(t *testing.T) {
	t.Parallel()
	baseURL := startShim(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		baseURL+"/git/"+testForgejoRepo+".git/info/refs?service=git-upload-pack", nil)
	require.NoError(t, err)
	req.SetBasicAuth("anyone", testForgejoToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the git surface must still be served with the shim in front")
}

// listPRs reads the shim's list endpoint with the given state filter.
func listPRs(t *testing.T, baseURL, state string) []pullRequestDoc {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		baseURL+"/api/v1/repos/"+testForgejoRepo+"/pulls?state="+state+"&limit=50", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "token "+testForgejoToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var prs []pullRequestDoc
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&prs))
	return prs
}

// mergeThroughControlAPI merges a PR the way demo:m5 does -- through the
// fake forge's own control API, which performs a real git merge -- rather
// than through anything the shim knows about.
func mergeThroughControlAPI(ctx context.Context, baseURL, repo string, number int) error {
	return control(ctx, baseURL, "/control/merge-pr", map[string]any{"repo": repo, "number": number})
}
