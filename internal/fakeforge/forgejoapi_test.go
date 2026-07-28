package fakeforge

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
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

// TestForgejoScopeProbeRefusesToCreateAPullRequest pins forgejoapi.go's
// deliberate stopping point: this route models ValidateToken's probe and
// nothing else, so a repo that DOES exist gets an explicit 501 rather than
// a fabricated 201 or a 404 that would be a lie about a repo that is
// plainly there. loam-c8v tracks extending it.
func TestForgejoScopeProbeRefusesToCreateAPullRequest(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	srv.AddToken("full-token")
	resp := postProbeAt(t, ts.URL, "full-token", "acme", "widgets")
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode,
		"an existing repo must not be answered with the probe's 404, which a caller would read as a successful validation of a real PR-creation attempt")
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
