package fakeforge

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
)

// The tests below come in two layers, on purpose.
//
// The CLIENT layer drives the REAL, unmodified *forge.Forgejo against the
// fake over real HTTP. That is the whole point of forgejoapi.go: what has
// to hold is not "the fake returns 403" but "the production client, given
// this fake, produces forge.ErrInsufficientScope" -- the sentinel
// internal/handler/credential maps to a distinct Connect code. A test that
// asserted only status codes would keep passing if ValidateToken's probe
// path, header form or classification changed underneath it.
//
// The WIRE layer then pins the response bytes the client layer cannot see
// through: which status code carried the verdict, and that the 404 body
// really has a non-empty Forgejo "message". ValidateToken treats a 404
// WITHOUT one as unclassifiable rather than as success, so an empty body
// would turn every successful validation into an error -- a failure mode
// the client layer detects but cannot localise.

// newForgejoClient builds the real production Forgejo client, host-agnostic
// exactly as cmd/server wires it for CredentialService (main.go:
// forge.NewForgejo("", "", ...)), so these tests exercise the same object
// SetUpstreamToken holds.
func newForgejoClient() *forge.Forgejo {
	return forge.NewForgejo("", "", &http.Client{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// TestForgejoValidateTokenAgainstFake is the surface's reason to exist:
// every class internal/handler/credential branches on, produced by the real
// client against the fake.
func TestForgejoValidateTokenAgainstFake(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("full-token")
	srv.AddTokenWithoutPRScope("push-only-token")
	client := newForgejoClient()

	t.Run("a fully scoped token validates", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, client.ValidateToken(t.Context(), ts.URL, "full-token"),
			"the probe repo does not exist, which is the SUCCESS shape: a 404 carrying a Forgejo error body")
	})
	t.Run("an unregistered token is ErrInvalidToken", func(t *testing.T) {
		t.Parallel()
		err := client.ValidateToken(t.Context(), ts.URL, "never-issued-token")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrInvalidToken)
		assert.NotErrorIs(t, err, forge.ErrInsufficientScope,
			"a token the fake never issued must not read as merely underscoped -- the two map to different Connect codes")
	})
	t.Run("a token missing PR scope is ErrInsufficientScope", func(t *testing.T) {
		t.Parallel()
		err := client.ValidateToken(t.Context(), ts.URL, "push-only-token")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrInsufficientScope)
		assert.NotErrorIs(t, err, forge.ErrInvalidToken,
			"a scope failure must not also read as an auth failure")
	})
	t.Run("an empty token is ErrInvalidToken", func(t *testing.T) {
		t.Parallel()
		// The client short-circuits this one before any request is sent,
		// but the fake must agree anyway: were the guard ever removed, an
		// "Authorization: token " header must still 401 here rather than
		// fall through to the 404 that means success.
		err := client.ValidateToken(t.Context(), ts.URL, "")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrInvalidToken)
	})
}

// TestForgejoValidateTokenAgainstFakeUnauthenticatedProbe closes the hole
// the empty-token case above can only half-close, since the client refuses
// to send that request: an anonymous probe -- no Authorization header at
// all -- must be REFUSED by the fake, not silently answered with the 404
// that reads as success. This is the fake-side counterpart of the reason
// ValidateToken guards the empty token client-side (forgejo.go: a real
// Forgejo reads an empty token value as anonymous, bypasses the scope
// middleware, and 404s exactly like the success case).
func TestForgejoValidateTokenAgainstFakeUnauthenticatedProbe(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	resp := postProbe(t, ts.URL, "")
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"an anonymous probe must be rejected, never answered with the 404 that ValidateToken reads as success")
}

// TestForgejoScopeProbeWireContract pins the three response codes and the
// one body field ValidateToken actually consumes.
func TestForgejoScopeProbeWireContract(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("full-token")
	srv.AddTokenWithoutPRScope("push-only-token")
	for _, tc := range []struct {
		name       string
		token      string
		wantStatus int
	}{
		{"unregistered token", "never-issued-token", http.StatusUnauthorized},
		{"token without PR scope", "push-only-token", http.StatusForbidden},
		{"fully scoped token", "full-token", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := postProbe(t, ts.URL, tc.token)
			defer func() { assert.NoError(t, resp.Body.Close()) }()
			assert.Equal(t, tc.wantStatus, resp.StatusCode)
			var envelope forgejoErrorEnvelope
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&envelope))
			assert.NotEmpty(t, envelope.Message,
				"every response must carry Forgejo's error body: ValidateToken reads a 404 WITHOUT one as unclassifiable, not as success")
		})
	}
}

// TestForgejoScopeProbeChecksScopeBeforeResolvingTheRepo pins the ordering
// the whole probe technique rests on. Forgejo's scope middleware runs
// before the owner/repo lookup, which is why ValidateToken can aim at a
// path picked never to exist and still get an unambiguous verdict. A fake
// that resolved the repo first would answer 404 -- i.e. SUCCESS -- for a
// token that authenticates nowhere.
func TestForgejoScopeProbeChecksScopeBeforeResolvingTheRepo(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddTokenWithoutPRScope("push-only-token")
	resp := postProbe(t, ts.URL, "push-only-token")
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"the scope verdict must win over the nonexistent repo, or an underscoped token would validate")
}

// TestForgejoCreateGetClosePRAgainstFake is loam-c8v's reason to exist:
// the full PR lifecycle 1lrp needs, driven through the REAL, unmodified
// *forge.Forgejo the exact way internal/mirrorsync's StorePRPoller and the
// admin CloseWorkBranch path use it, against the fake's Forgejo-REST-shaped
// surface rather than the fake's own /provider/* API.
func TestForgejoCreateGetClosePRAgainstFake(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "wb-feature", ""))
	srv.AddToken("full-token")
	client := forge.NewForgejo(ts.URL, "full-token", &http.Client{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	url, number, err := client.CreatePR(t.Context(), "acme/widgets", "wb-feature", "main", "my title", "my description")
	require.NoError(t, err)
	assert.NotEmpty(t, url, "CreatePR must return a browsable PR URL")
	assert.Positive(t, number, "CreatePR must return the per-repo PR number")

	state, err := client.GetPRState(t.Context(), "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "open", state)

	require.NoError(t, client.ClosePR(t.Context(), "acme/widgets", number))

	state, err = client.GetPRState(t.Context(), "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "closed", state)
}

// TestForgejoClosePRAgainstFakeIsVisibleThroughTheControlAPI proves the
// coherence property the bead's design constraints require: a PR closed
// through the REAL client's REST call must be observably closed through
// the fake's OWN control-shaped read (Server.PullRequests), because both
// surfaces share the one prRegistry rather than keeping independent state.
func TestForgejoClosePRAgainstFakeIsVisibleThroughTheControlAPI(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "wb-feature", ""))
	srv.AddToken("full-token")
	client := forge.NewForgejo(ts.URL, "full-token", &http.Client{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	_, number, err := client.CreatePR(t.Context(), "acme/widgets", "wb-feature", "main", "title", "description")
	require.NoError(t, err)

	require.NoError(t, client.ClosePR(t.Context(), "acme/widgets", number))

	prs := srv.PullRequests("acme/widgets")
	require.Len(t, prs, 1)
	assert.Equal(t, "closed", prs[0].State, "a close issued through the Forgejo-REST surface must be visible through the control-API read")
}

// TestForgejoClosePRAlreadyMergedAgainstFake pins the loam-giq.8 guard on
// this surface: PATCH state=closed against a PR the /control/merge-pr API
// has already merged must 412, leaving the state untouched, not silently
// succeed or 404.
func TestForgejoClosePRAlreadyMergedAgainstFake(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "wb-feature", ""))
	srv.AddToken("full-token")
	client := forge.NewForgejo(ts.URL, "full-token", &http.Client{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	_, number, err := client.CreatePR(t.Context(), "acme/widgets", "wb-feature", "main", "title", "description")
	require.NoError(t, err)
	require.NoError(t, srv.MergePR(t.Context(), "acme/widgets", number))

	err = client.ClosePR(t.Context(), "acme/widgets", number)
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrPRAlreadyMerged)

	state, err := client.GetPRState(t.Context(), "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "merged", state, "a refused close must leave the state untouched")
}

// TestForgejoCreatePRDuplicateAndFindOpenPRAgainstFake pins the
// idempotency path loam-giq.7 relies on, against the real client rather
// than the fake's own /provider/* Client: a second CreatePR for a pair
// that already has an open PR is a conflict, and FindOpenPR's paged list
// adopts the original.
func TestForgejoCreatePRDuplicateAndFindOpenPRAgainstFake(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "wb-dup", ""))
	srv.AddToken("full-token")
	client := forge.NewForgejo(ts.URL, "full-token", &http.Client{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	firstURL, firstNumber, err := client.CreatePR(t.Context(), "acme/widgets", "wb-dup", "main", "title", "description")
	require.NoError(t, err)

	_, _, err = client.CreatePR(t.Context(), "acme/widgets", "wb-dup", "main", "title again", "description again")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrDuplicatePR)

	adoptedURL, adoptedNumber, found, err := client.FindOpenPR(t.Context(), "acme/widgets", "wb-dup", "main")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, firstNumber, adoptedNumber)
	assert.Equal(t, firstURL, adoptedURL)
}

// TestForgejoGetPRStateUnknownNumberAgainstFake pins the fold
// internal/forge/errors.go documents: a PR number the repo does not have
// is indistinguishable, at the wire level, from the repo not existing at
// all -- both are Forgejo's identical generic 404.
func TestForgejoGetPRStateUnknownNumberAgainstFake(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{DefaultBranch: "main"}))
	srv.AddToken("full-token")
	client := forge.NewForgejo(ts.URL, "full-token", &http.Client{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	_, err := client.GetPRState(t.Context(), "acme/widgets", 4242)
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrRepoNotFound)
}

// TestForgejoPatchPullRefusesAnUnmodelledEditAgainstFake pins the "keep it
// honest" property loam-c8v's design constraints require: PATCH now
// implements the ONE edit forge.Forgejo.ClosePR ever sends
// ({"state":"closed"}), and anything else -- reopening a PR, editing its
// title -- gets the same explicit 501 the whole route used to answer for
// every create, never a silently-accepted no-op.
func TestForgejoPatchPullRefusesAnUnmodelledEditAgainstFake(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{DefaultBranch: "main"}))
	require.NoError(t, srv.CreateBranch(t.Context(), "acme/widgets", "wb-feature", ""))
	srv.AddToken("full-token")
	client := forge.NewForgejo(ts.URL, "full-token", &http.Client{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	_, number, err := client.CreatePR(t.Context(), "acme/widgets", "wb-feature", "main", "title", "description")
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch,
		fmt.Sprintf("%s/api/v1/repos/acme/widgets/pulls/%d", ts.URL, number), strings.NewReader(`{"state":"open"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "token full-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode,
		"an edit this route does not model must not be answered with a fabricated success")
	var envelope forgejoErrorEnvelope
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&envelope))
	assert.NotEmpty(t, envelope.Message)
}

// TestForgejoAuthOrderingAppliesToGetAndPatchAndListAgainstFake extends
// TestForgejoScopeProbeChecksScopeBeforeResolvingTheRepo's ordering
// guarantee to the three routes loam-c8v adds: a token the fake never
// issued must 401 before any repo or PR lookup happens, on every route,
// not only create.
func TestForgejoAuthOrderingAppliesToGetAndPatchAndListAgainstFake(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{DefaultBranch: "main"}))
	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"get", http.MethodGet, "/api/v1/repos/acme/widgets/pulls/1"},
		{"patch", http.MethodPatch, "/api/v1/repos/acme/widgets/pulls/1"},
		{"list", http.MethodGet, "/api/v1/repos/acme/widgets/pulls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(t.Context(), tc.method, ts.URL+tc.path, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "token never-issued-token")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { assert.NoError(t, resp.Body.Close()) }()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
				"an unregistered token must be refused before the repo or PR is ever resolved")
		})
	}
}

// postProbe issues the exact request forge.Forgejo.ValidateToken issues,
// against the same never-existing probe path, with token in Forgejo's
// "Authorization: token <t>" form. An empty token sends NO Authorization
// header at all, which is the anonymous case.
func postProbe(t *testing.T, baseURL, token string) *http.Response {
	t.Helper()
	return postProbeAt(t, baseURL, token, "loam-scope-probe-9f3c2e71", "does-not-exist")
}

func postProbeAt(t *testing.T, baseURL, token, owner, repo string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		baseURL+"/api/v1/repos/"+owner+"/"+repo+"/pulls", nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}
