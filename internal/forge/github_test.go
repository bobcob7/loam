package forge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGitHub_ValidateToken covers loam-tmds.2's AC2/AC5/AC6: each
// sentinel ValidateToken can produce, plus the rate-limit cases that
// must NOT map to ErrInvalidToken.
func TestGitHub_ValidateToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statusCode int
		headers    map[string]string
		wantErr    error
		wantNoErr  error // sentinel that must NOT be the error, when wantErr is a different, unclassified failure
	}{
		{name: "authenticated with repo scope", statusCode: http.StatusOK, headers: map[string]string{"X-OAuth-Scopes": "repo, gist"}, wantErr: nil},
		{name: "authenticated without repo scope", statusCode: http.StatusOK, headers: map[string]string{"X-OAuth-Scopes": "gist, notifications"}, wantErr: ErrInsufficientScope},
		{name: "authenticated with empty scopes header", statusCode: http.StatusOK, headers: map[string]string{"X-OAuth-Scopes": ""}, wantErr: ErrInsufficientScope},
		{name: "unauthenticated token rejected", statusCode: http.StatusUnauthorized, wantErr: ErrInvalidToken},
		{name: "primary rate limit: 403 with x-ratelimit-remaining=0", statusCode: http.StatusForbidden, headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1234567890"}, wantErr: errGitHubRateLimited, wantNoErr: ErrInvalidToken},
		{name: "429 too many requests", statusCode: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "30"}, wantErr: errGitHubRateLimited, wantNoErr: ErrInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/user", r.URL.Path)
				assert.Equal(t, "token good-token", r.Header.Get("Authorization"))
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()
			g := NewGitHub(server.URL, "", server.Client(), testLogger())
			err := g.ValidateToken(t.Context(), server.URL, "good-token")
			if tt.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantErr)
			if tt.wantNoErr != nil {
				assert.NotErrorIs(t, err, tt.wantNoErr, "a rate-limit rejection must never present as ErrInvalidToken")
			}
		})
	}
}

// TestGitHub_ValidateToken_SecondaryRateLimit_MessageOnly covers the
// secondary-limit case GitHub's docs say does NOT guarantee
// x-ratelimit-remaining=0: a 403 whose body merely mentions "rate
// limit" must still be classified as rate limiting, not ErrInvalidToken.
func TestGitHub_ValidateToken_SecondaryRateLimit_MessageOnly(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes."}`))
	}))
	defer server.Close()
	g := NewGitHub(server.URL, "", server.Client(), testLogger())
	err := g.ValidateToken(t.Context(), server.URL, "good-token")
	require.Error(t, err)
	assert.ErrorIs(t, err, errGitHubRateLimited)
	assert.NotErrorIs(t, err, ErrInvalidToken)
}

// TestGitHub_ValidateToken_EmptyToken mirrors
// TestForgejo_ValidateToken_EmptyToken: an empty token must be rejected
// before any request is sent.
func TestGitHub_ValidateToken_EmptyToken(t *testing.T) {
	t.Parallel()
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	g := NewGitHub(server.URL, "", server.Client(), testLogger())
	err := g.ValidateToken(t.Context(), server.URL, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.False(t, called)
}

// githubPRHandler builds an http.HandlerFunc serving GitHub's
// pulls-lifecycle routes against a single in-memory PR record, for
// CreatePR/GetPRState/ClosePR/FindOpenPR tests that need a believable
// round trip rather than a single canned response.
type githubPRHandler struct {
	number       int
	state        string
	merged       bool
	headBranch   string
	targetBranch string
}

func (h *githubPRHandler) wire(r *http.Request) githubPullWire {
	return githubPullWire{
		HTMLURL: fmt.Sprintf("http://%s/owner/repo/pull/%d", r.Host, h.number),
		Number:  h.number,
		State:   h.state,
		Merged:  h.merged,
		Head:    githubRefWire{Ref: h.headBranch},
		Base:    githubRefWire{Ref: h.targetBranch},
	}
}

// TestGitHub_CreatePR_PostsPlainHeadBranch pins loam-tmds.2's "head
// branch format" decision: same-repository PRs (the only kind loam
// opens — see githubCreatePullRequest's doc comment) send head as a
// PLAIN branch name, never "owner:branch".
func TestGitHub_CreatePR_PostsPlainHeadBranch(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/repos/acme/widgets/pulls", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusCreated)
		require.NoError(t, json.NewEncoder(w).Encode((&githubPRHandler{number: 7, state: "open", headBranch: "loam/wb-1", targetBranch: "main"}).wire(r)))
	}))
	defer server.Close()
	g := NewGitHub(server.URL, "tkn", server.Client(), testLogger())
	url, number, err := g.CreatePR(t.Context(), "acme/widgets", "loam/wb-1", "main", "title", "body")
	require.NoError(t, err)
	assert.Equal(t, 7, number)
	assert.Contains(t, url, "/pull/7")
	assert.Equal(t, "loam/wb-1", gotBody["head"], "head must be a plain branch name, never owner:branch, for a same-repository PR")
	assert.Equal(t, "main", gotBody["base"])
}

// TestGitHub_CreatePR_DuplicatePR_Maps422ToErrDuplicatePR covers
// loam-tmds.2's AC2 for ErrDuplicatePR: GitHub's 422 validation-error
// shape, not Forgejo's 409.
func TestGitHub_CreatePR_DuplicatePR_Maps422ToErrDuplicatePR(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"A pull request already exists for acme:loam/wb-1."}],"documentation_url":"https://docs.github.com/rest/pulls/pulls#create-a-pull-request"}`))
	}))
	defer server.Close()
	g := NewGitHub(server.URL, "tkn", server.Client(), testLogger())
	_, _, err := g.CreatePR(t.Context(), "acme/widgets", "loam/wb-1", "main", "title", "body")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDuplicatePR)
}

// TestGitHub_CreatePR_Other422IsNotDuplicatePR proves githubIsDuplicatePR
// does not treat every 422 as a duplicate — a genuinely different
// validation failure must fall through to the generic error, not be
// misreported.
func TestGitHub_CreatePR_Other422IsNotDuplicatePR(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"No commits between main and loam/wb-1."}]}`))
	}))
	defer server.Close()
	g := NewGitHub(server.URL, "tkn", server.Client(), testLogger())
	_, _, err := g.CreatePR(t.Context(), "acme/widgets", "loam/wb-1", "main", "title", "body")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrDuplicatePR)
}

// TestGitHub_CreatePR_ErrorMapping covers ErrInvalidToken/ErrRepoNotFound
// via the shared status classifier, and the rate-limit non-mapping at
// this call site too (loam-tmds.2 AC5 is not ValidateToken-only).
func TestGitHub_CreatePR_ErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statusCode int
		headers    map[string]string
		wantErr    error
		wantNoErr  error
	}{
		{name: "401 unauthenticated", statusCode: http.StatusUnauthorized, wantErr: ErrInvalidToken},
		{name: "403 without rate-limit signal", statusCode: http.StatusForbidden, wantErr: ErrInvalidToken},
		{name: "404 repo not found", statusCode: http.StatusNotFound, wantErr: ErrRepoNotFound},
		{name: "429 rate limited, must not be ErrInvalidToken", statusCode: http.StatusTooManyRequests, wantErr: errGitHubRateLimited, wantNoErr: ErrInvalidToken},
		{name: "403 rate limited, must not be ErrInvalidToken", statusCode: http.StatusForbidden, headers: map[string]string{"X-RateLimit-Remaining": "0"}, wantErr: errGitHubRateLimited, wantNoErr: ErrInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()
			g := NewGitHub(server.URL, "tkn", server.Client(), testLogger())
			_, _, err := g.CreatePR(t.Context(), "acme/widgets", "loam/wb-1", "main", "title", "body")
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantErr)
			if tt.wantNoErr != nil {
				assert.NotErrorIs(t, err, tt.wantNoErr)
			}
		})
	}
}

// TestGitHub_GetPRState_DistinguishesClosedFromMerged is loam-tmds.2's
// AC3, the test this bead's own notes call the most important one this
// method has: conflating a merged PR with a closed one would make loam
// treat a merged proposal as abandoned.
func TestGitHub_GetPRState_DistinguishesClosedFromMerged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		state  string
		merged bool
		want   string
	}{
		{name: "open", state: "open", merged: false, want: "open"},
		{name: "closed, not merged", state: "closed", merged: false, want: "closed"},
		{name: "closed AND merged", state: "closed", merged: true, want: "merged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &githubPRHandler{number: 3, state: tt.state, merged: tt.merged}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/repos/acme/widgets/pulls/3", r.URL.Path)
				require.NoError(t, json.NewEncoder(w).Encode(h.wire(r)))
			}))
			defer server.Close()
			g := NewGitHub(server.URL, "tkn", server.Client(), testLogger())
			got, err := g.GetPRState(t.Context(), "acme/widgets", 3)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestGitHub_ClosePR_AlreadyMerged_MapsToErrPRAlreadyMerged covers
// loam-tmds.2's AC2 for ErrPRAlreadyMerged via the documented response
// body (merged:true), the mechanism this package's own doc comment
// explains it uses instead of an unconfirmed distinct status code.
func TestGitHub_ClosePR_AlreadyMerged_MapsToErrPRAlreadyMerged(t *testing.T) {
	t.Parallel()
	h := &githubPRHandler{number: 9, state: "closed", merged: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "closed", body["state"])
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(h.wire(r)))
	}))
	defer server.Close()
	g := NewGitHub(server.URL, "tkn", server.Client(), testLogger())
	err := g.ClosePR(t.Context(), "acme/widgets", 9)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPRAlreadyMerged)
}

// TestGitHub_ClosePR_OpenPR_ClosesCleanly is ClosePR's happy path,
// establishing the contrast case for the already-merged test above.
func TestGitHub_ClosePR_OpenPR_ClosesCleanly(t *testing.T) {
	t.Parallel()
	h := &githubPRHandler{number: 9, state: "closed", merged: false}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(h.wire(r)))
	}))
	defer server.Close()
	g := NewGitHub(server.URL, "tkn", server.Client(), testLogger())
	require.NoError(t, g.ClosePR(t.Context(), "acme/widgets", 9))
}

// TestGitHub_FindOpenPR_ListsAndFilters_UsingHeadOwnerPrefix pins
// loam-tmds.2's AC4 and the head-filter format decision: GitHub's list
// endpoint requires "<owner>:<branch>" even for a same-repository PR.
func TestGitHub_FindOpenPR_ListsAndFilters_UsingHeadOwnerPrefix(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/repos/acme/widgets/pulls", r.URL.Path)
		assert.Equal(t, "open", r.URL.Query().Get("state"))
		assert.Equal(t, "acme:loam/wb-1", r.URL.Query().Get("head"), "head filter must be owner:branch even for a same-repository PR")
		assert.Equal(t, "main", r.URL.Query().Get("base"))
		h := &githubPRHandler{number: 4, state: "open", headBranch: "loam/wb-1", targetBranch: "main"}
		require.NoError(t, json.NewEncoder(w).Encode([]githubPullWire{h.wire(r)}))
	}))
	defer server.Close()
	g := NewGitHub(server.URL, "tkn", server.Client(), testLogger())
	url, number, found, err := g.FindOpenPR(t.Context(), "acme/widgets", "loam/wb-1", "main")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, 4, number)
	assert.Contains(t, url, "/pull/4")
}

// TestGitHub_FindOpenPR_NoMatch_ReturnsFoundFalseNotError proves an
// empty list is a normal, non-error result — FindOpenPR's own interface
// doc: "found is false ... when no such PR is open".
func TestGitHub_FindOpenPR_NoMatch_ReturnsFoundFalseNotError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode([]githubPullWire{}))
	}))
	defer server.Close()
	g := NewGitHub(server.URL, "tkn", server.Client(), testLogger())
	_, _, found, err := g.FindOpenPR(t.Context(), "acme/widgets", "loam/wb-1", "main")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestGitHub_GitCredentials mirrors Forgejo's convention: any username,
// token as password — the classic-PAT convention this provider's
// token-kind decision makes identical to Forgejo's.
func TestGitHub_GitCredentials(t *testing.T) {
	t.Parallel()
	g := NewGitHub("github.com", "", &http.Client{}, testLogger())
	username, password, err := g.GitCredentials(t.Context(), "a-classic-pat")
	require.NoError(t, err)
	assert.NotEmpty(t, username)
	assert.Equal(t, "a-classic-pat", password)
	_, _, err = g.GitCredentials(t.Context(), "")
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// TestGitHub_CheckRepo_SharesForgejosGitProbeImplementation proves
// CheckRepo actually reaches the shared checkRepoOverGit path (read +
// write probes, bound-host guard) rather than a GitHub-specific
// reimplementation that could silently diverge.
func TestGitHub_CheckRepo_SharesForgejosGitProbeImplementation(t *testing.T) {
	t.Parallel()
	g := NewGitHub("github.com", "tkn", &http.Client{}, testLogger())
	err := g.CheckRepo(t.Context(), "https://gitlab.example.com/acme/widgets")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match the bound credential's host",
		"the bound-host guard is checkRepoOverGit's, proving CheckRepo delegates to the shared implementation rather than skipping it")
}

// TestGitHub_ImplementsProvider is loam-tmds.2's AC1.
func TestGitHub_ImplementsProvider(t *testing.T) {
	t.Parallel()
	var _ Provider = (*GitHub)(nil)
}

// TestApiBaseURLForGitHub_RealHostUsesFixedAPIRoot pins the base-URL
// derivation decision: github.com and api.github.com both resolve to
// the fixed https://api.github.com root, never a host-derived one —
// the opposite of Forgejo's host+"/api/v1" rule.
func TestApiBaseURLForGitHub_RealHostUsesFixedAPIRoot(t *testing.T) {
	t.Parallel()
	assert.Equal(t, githubAPIRoot, apiBaseURLForGitHub("github.com"))
	assert.Equal(t, githubAPIRoot, apiBaseURLForGitHub("api.github.com"))
	assert.Equal(t, githubAPIRoot, apiBaseURLForGitHub(""))
}

// TestApiBaseURLForGitHub_TestDoubleHostUsedAsIs pins the other half:
// a scheme-qualified host (an httptest server, or internal/fakeforge's
// GitHub-shaped surface) is used verbatim, with no /api/v3 or other
// suffix appended — Enterprise Server's own base shape, deliberately
// unsupported (see GitHub's own doc comment).
func TestApiBaseURLForGitHub_TestDoubleHostUsedAsIs(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "http://127.0.0.1:9999", apiBaseURLForGitHub("http://127.0.0.1:9999"))
	assert.Equal(t, "http://127.0.0.1:9999", apiBaseURLForGitHub("http://127.0.0.1:9999/"))
}
