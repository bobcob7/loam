package forge

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestForgejo_ValidateToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    error
	}{
		{name: "authenticated with scope, probe repo not found as expected", statusCode: http.StatusNotFound, body: `{"message":"repository does not exist","url":"https://x/api/swagger"}`, wantErr: nil},
		{name: "authenticated with scope, unexpected 2xx", statusCode: http.StatusOK, wantErr: nil},
		{name: "unauthenticated token rejected", statusCode: http.StatusUnauthorized, wantErr: ErrInvalidToken},
		{name: "authenticated but missing scope rejected", statusCode: http.StatusForbidden, wantErr: ErrInsufficientScope},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/repos/"+probeOwner+"/"+probeRepo+"/pulls", r.URL.Path)
				assert.Equal(t, "token good-token", r.Header.Get("Authorization"))
				w.WriteHeader(tt.statusCode)
				if tt.body != "" {
					_, _ = w.Write([]byte(tt.body))
				}
			}))
			defer server.Close()
			f := NewForgejo(server.URL, "", server.Client(), testLogger())
			err := f.ValidateToken(t.Context(), server.URL, "good-token")
			if tt.wantErr == nil {
				require.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestForgejo_ValidateToken_UnexpectedStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "", server.Client(), testLogger())
	err := f.ValidateToken(t.Context(), server.URL, "good-token")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidToken)
	assert.NotErrorIs(t, err, ErrInsufficientScope)
}

func TestForgejo_ValidateToken_EmptyToken(t *testing.T) {
	t.Parallel()
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"repository does not exist","url":"https://x/api/swagger"}`))
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "", server.Client(), testLogger())
	err := f.ValidateToken(t.Context(), server.URL, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.False(t, called, "an empty token must be rejected before any request is sent — Forgejo reads it as anonymous and would 404 through to the success path")
}

func TestForgejo_ValidateToken_NotFoundWithoutForgejoBody(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "", server.Client(), testLogger())
	err := f.ValidateToken(t.Context(), server.URL, "good-token")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidToken)
	assert.NotErrorIs(t, err, ErrInsufficientScope)
}

func TestForgejo_ValidateToken_NetworkFailure(t *testing.T) {
	t.Parallel()
	f := NewForgejo("http://127.0.0.1:0", "", http.DefaultClient, testLogger())
	err := f.ValidateToken(t.Context(), "http://127.0.0.1:0", "token")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidToken)
}

func TestForgejo_CreatePR(t *testing.T) {
	t.Parallel()
	const wantPRURL = "https://forgejo.example.com/acme/widgets/pulls/7"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/repos/acme/widgets/pulls", r.URL.Path)
		assert.Equal(t, "token secret", r.Header.Get("Authorization"))
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "wb-9c2f1a", body["head"])
		assert.Equal(t, "main", body["base"])
		assert.Equal(t, "Add widget", body["title"])
		assert.Equal(t, "does widget things", body["body"])
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"html_url": wantPRURL, "number": 7})
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	prURL, prNumber, err := f.CreatePR(t.Context(), "acme/widgets", "wb-9c2f1a", "main", "Add widget", "does widget things")
	require.NoError(t, err)
	assert.Equal(t, wantPRURL, prURL)
	assert.Equal(t, 7, prNumber)
}

func TestForgejo_CreatePR_RepoNotFound(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	_, _, err := f.CreatePR(t.Context(), "acme/missing", "wb-1", "main", "t", "d")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRepoNotFound)
}

// TestForgejo_CreatePR_DuplicatePR covers doPullRequest's 409 handling:
// verified empirically against a real Forgejo 9.0.3 instance, a repeat
// CreatePR for a head/target pair that already has an open PR returns 409
// with a message embedding the existing PR's internal id (loam-hza). The
// id is unstructured text, not parsed here — this only asserts the status
// maps to ErrDuplicatePR.
func TestForgejo_CreatePR_DuplicatePR(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"pull request already exists for these targets [id: 1, issue_id: 1, head_repo_id: 1, base_repo_id: 1, head_branch: feature, base_branch: main]","url":"https://x/api/swagger"}`))
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	_, _, err := f.CreatePR(t.Context(), "acme/widgets", "feature", "main", "t", "d")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDuplicatePR)
}

func TestForgejo_GetPRState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		state     string
		merged    bool
		wantState string
	}{
		{name: "open", state: "open", merged: false, wantState: "open"},
		{name: "merged", state: "closed", merged: true, wantState: "merged"},
		{name: "closed without merge", state: "closed", merged: false, wantState: "closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/v1/repos/acme/widgets/pulls/7", r.URL.Path)
				_ = json.NewEncoder(w).Encode(map[string]any{"state": tt.state, "merged": tt.merged, "number": 7})
			}))
			defer server.Close()
			f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
			state, err := f.GetPRState(t.Context(), "acme/widgets", 7)
			require.NoError(t, err)
			assert.Equal(t, tt.wantState, state)
		})
	}
}

func TestForgejo_ClosePR(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/api/v1/repos/acme/widgets/pulls/7", r.URL.Path)
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "closed", body["state"])
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "closed", "number": 7})
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	err := f.ClosePR(t.Context(), "acme/widgets", 7)
	require.NoError(t, err)
}

func TestForgejo_ClosePR_AuthFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "bad", server.Client(), testLogger())
	err := f.ClosePR(t.Context(), "acme/widgets", 7)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestForgejo_GitCredentials(t *testing.T) {
	t.Parallel()
	f := NewForgejo("forgejo.example.com", "", http.DefaultClient, testLogger())
	username, password, err := f.GitCredentials(t.Context(), "the-token")
	require.NoError(t, err)
	assert.Equal(t, gitUsername, username)
	assert.Equal(t, "the-token", password)
}

func TestForgejo_GitCredentials_EmptyToken(t *testing.T) {
	t.Parallel()
	f := NewForgejo("forgejo.example.com", "", http.DefaultClient, testLogger())
	_, _, err := f.GitCredentials(t.Context(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// TestForgejo_FindOpenPR_Found covers the case FindOpenPR exists to serve:
// an open PR already recorded for the exact head/target pair. Verified
// empirically against a real Forgejo 9.0.3 instance that GET
// .../pulls?state=open takes no head/base query parameters — passing them
// is silently ignored (confirmed live) — so this asserts the request only
// carries state=open/limit/page, and that filtering on pr.head.ref/
// pr.base.ref happens client-side against the full open-PR list.
func TestForgejo_FindOpenPR_Found(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/repos/acme/widgets/pulls", r.URL.Path)
		assert.Equal(t, "open", r.URL.Query().Get("state"))
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"html_url": "https://forgejo.example.com/acme/widgets/pulls/3", "number": 3, "head": map[string]string{"ref": "other-branch"}, "base": map[string]string{"ref": "main"}},
			{"html_url": "https://forgejo.example.com/acme/widgets/pulls/9", "number": 9, "head": map[string]string{"ref": "feature"}, "base": map[string]string{"ref": "main"}},
		})
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	prURL, prNumber, found, err := f.FindOpenPR(t.Context(), "acme/widgets", "feature", "main")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "https://forgejo.example.com/acme/widgets/pulls/9", prURL)
	assert.Equal(t, 9, prNumber)
}

// TestForgejo_FindOpenPR_NotFound covers a repo whose open PRs never match
// the requested head/target pair — the not-found case, distinct from an
// error: found is false with a nil error.
func TestForgejo_FindOpenPR_NotFound(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"html_url": "https://forgejo.example.com/acme/widgets/pulls/3", "number": 3, "head": map[string]string{"ref": "other-branch"}, "base": map[string]string{"ref": "main"}},
		})
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	prURL, prNumber, found, err := f.FindOpenPR(t.Context(), "acme/widgets", "feature", "main")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, prURL)
	assert.Zero(t, prNumber)
}

// TestForgejo_FindOpenPR_NoOpenPRs covers the empty-list case (repo exists,
// no open PRs at all): still not-found, not an error.
func TestForgejo_FindOpenPR_NoOpenPRs(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	_, _, found, err := f.FindOpenPR(t.Context(), "acme/widgets", "feature", "main")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestForgejo_FindOpenPR_RepoNotFound covers a repo (or owner) that does not
// exist: verified live against a real Forgejo 9.0.3 instance, both a
// missing repo and a missing owner 404 the list-pulls endpoint the same way
// CreatePR/GetPRState/ClosePR's pulls endpoints do, so this maps to the same
// ErrRepoNotFound sentinel via the same 404 branch.
func TestForgejo_FindOpenPR_RepoNotFound(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"The target couldn't be found.","url":"https://x/api/swagger","errors":[]}`))
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	_, _, found, err := f.FindOpenPR(t.Context(), "acme/missing", "feature", "main")
	require.Error(t, err)
	assert.False(t, found)
	assert.ErrorIs(t, err, ErrRepoNotFound)
}

// TestForgejo_FindOpenPR_AuthFailure covers a rejected token on the list
// endpoint, mapped the same way doPullRequest maps 401/403 on the single-PR
// endpoints: verified live that an unrecognized token 401s the list
// endpoint too.
func TestForgejo_FindOpenPR_AuthFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "bad", server.Client(), testLogger())
	_, _, found, err := f.FindOpenPR(t.Context(), "acme/widgets", "feature", "main")
	require.Error(t, err)
	assert.False(t, found)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// TestForgejo_FindOpenPR_Paginates covers a repo with more open PRs than
// fit on one page: the matching PR only appears on the second page, so
// FindOpenPR must keep paging (real Forgejo's list endpoint pages via
// page=/limit= with X-Total-Count, verified live) rather than stopping
// after the first full page.
func TestForgejo_FindOpenPR_Paginates(t *testing.T) {
	t.Parallel()
	var gotPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		gotPages = append(gotPages, page)
		if page == "1" {
			prs := make([]map[string]any, listOpenPRsPageSize)
			for i := range prs {
				prs[i] = map[string]any{"html_url": "x", "number": i + 100, "head": map[string]string{"ref": "unrelated"}, "base": map[string]string{"ref": "main"}}
			}
			_ = json.NewEncoder(w).Encode(prs)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"html_url": "https://forgejo.example.com/acme/widgets/pulls/42", "number": 42, "head": map[string]string{"ref": "feature"}, "base": map[string]string{"ref": "main"}},
		})
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	prURL, prNumber, found, err := f.FindOpenPR(t.Context(), "acme/widgets", "feature", "main")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, 42, prNumber)
	assert.Equal(t, "https://forgejo.example.com/acme/widgets/pulls/42", prURL)
	assert.Equal(t, []string{"1", "2"}, gotPages)
}

func TestApiBaseURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "https://forgejo.example.com/api/v1", apiBaseURL("forgejo.example.com"))
	assert.Equal(t, "http://127.0.0.1:8080/api/v1", apiBaseURL("http://127.0.0.1:8080"))
}
