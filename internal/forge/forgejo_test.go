package forge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// roundTripFunc adapts a plain function to http.RoundTripper, for tests
// that need to observe (or control) exactly what scheme/URL a request was
// sent with, independent of what a real TCP/TLS handshake would do.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestForgejo_ValidateToken_BareHostFallsBackToHTTPOnSchemeMismatch is the
// loam-4kz regression: a BARE host:port (no "://", exactly what
// CredentialService.SetUpstreamToken receives from an admin typing a
// forge host with no accompanying upstream URL to borrow a scheme from)
// naming a REAL plaintext-HTTP server must still validate. Before this
// fix, apiBaseURL's https default meant the httptest server below -- a
// genuine `httptest.NewServer`, HTTP only, never `NewTLSServer` -- would
// answer a TLS ClientHello with a plain HTTP response and ValidateToken
// would surface http.ErrSchemeMismatch as an unclassified failure.
func TestForgejo_ValidateToken_BareHostFallsBackToHTTPOnSchemeMismatch(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/repos/"+probeOwner+"/"+probeRepo+"/pulls", r.URL.Path)
		assert.Equal(t, "token good-token", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"repository does not exist","url":"https://x/api/swagger"}`))
	}))
	defer server.Close()
	bareHost := strings.TrimPrefix(server.URL, "http://")
	require.NotEqual(t, server.URL, bareHost, "server.URL must be a bare http:// httptest server for this to be a real regression test")
	f := NewForgejo(bareHost, "", server.Client(), testLogger())
	err := f.ValidateToken(t.Context(), bareHost, "good-token")
	require.NoError(t, err)
}

// TestForgejo_ValidateToken_ExplicitHTTPSNeverFallsBack proves the retry
// is scoped to a host that carried NO scheme to begin with. A caller that
// wrote "https://" explicitly gets that request honoured to its actual
// failure, never silently downgraded to plaintext -- an explicit scheme
// is the one signal this method treats as non-negotiable.
func TestForgejo_ValidateToken_ExplicitHTTPSNeverFallsBack(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	bareHost := strings.TrimPrefix(server.URL, "http://")
	explicitHTTPS := "https://" + bareHost
	f := NewForgejo(explicitHTTPS, "", server.Client(), testLogger())
	err := f.ValidateToken(t.Context(), explicitHTTPS, "good-token")
	require.Error(t, err)
	assert.ErrorIs(t, err, http.ErrSchemeMismatch)
}

// TestForgejo_ValidateToken_BareHost_OnlyRetriesOnSchemeMismatch proves
// the retry fires on the SPECIFIC http.ErrSchemeMismatch signal, not on
// any transport failure -- a bare host whose https attempt fails for an
// unrelated reason (here, a fake RoundTripper returning an arbitrary
// error) must be reported as-is, with no second, http:// request ever
// sent. A mutation that widened the guard to "retry on any error" is
// caught here: this test's RoundTripper asserts it is invoked exactly
// once and that the one call was scheme "https".
func TestForgejo_ValidateToken_BareHost_OnlyRetriesOnSchemeMismatch(t *testing.T) {
	t.Parallel()
	var calls int
	wantErr := errors.New("boom: connection reset by peer")
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, "https", req.URL.Scheme, "the first attempt for a bare host must always be https")
		return nil, wantErr
	})}
	f := NewForgejo("forge.example.invalid", "", client, testLogger())
	err := f.ValidateToken(t.Context(), "forge.example.invalid", "good-token")
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, calls, "a non-scheme-mismatch transport error must not trigger the http:// retry")
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

// TestForgejo_ClosePR_AlreadyMerged pins the 412 classification loam-giq.8
// added. Real Forgejo 9.0.3 answers PATCH state=closed on a MERGED pull
// request with 412 Precondition Failed ("cannot change state of this pull
// request, it was already merged") and leaves the state untouched. Before
// this mapping that fell through doPullRequest's generic "unexpected
// status" branch, indistinguishable from a transport failure a caller
// should retry -- which is exactly the wrong reading, since the PR is
// already terminal.
func TestForgejo_ClosePR_AlreadyMerged(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "cannot change state of this pull request, it was already merged"})
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	err := f.ClosePR(t.Context(), "acme/widgets", 7)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPRAlreadyMerged)
	assert.NotErrorIs(t, err, ErrRepoNotFound, "an already-merged PR must not read as a missing one")
	assert.NotErrorIs(t, err, ErrInvalidToken)
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
		if r.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
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

// TestForgejo_FindOpenPR_ServerCapsPageSizeBelowRequested is FIX 1's
// regression test: verified live against a real Forgejo 9.0.3 instance
// with 62 open PRs, the server silently caps the effective page size at
// its own api.MAX_RESPONSE_ITEMS (admin-tunable) regardless of the
// limit= FindOpenPR requests — asking limit=62, 100, or 1000 all
// returned exactly 50 items. This server serves a page smaller than
// listOpenPRsPageSize (mimicking a MAX_RESPONSE_ITEMS below this
// method's own page size) with the match only on page 2, so the old
// "len(prs) < listOpenPRsPageSize means last page" termination — which
// this short first page would have satisfied — must not stop the walk
// before page 2 is ever requested.
func TestForgejo_FindOpenPR_ServerCapsPageSizeBelowRequested(t *testing.T) {
	t.Parallel()
	const serverPageSize = 20 // < listOpenPRsPageSize: the server-side cap
	var gotPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPages = append(gotPages, r.URL.Query().Get("page"))
		assert.Equal(t, fmt.Sprintf("%d", listOpenPRsPageSize), r.URL.Query().Get("limit"), "the client always asks for its own page size, regardless of what the server actually honors")
		if r.URL.Query().Get("page") == "1" {
			prs := make([]map[string]any, serverPageSize)
			for i := range prs {
				prs[i] = map[string]any{"html_url": "x", "number": i + 200, "head": map[string]string{"ref": "unrelated"}, "base": map[string]string{"ref": "main"}}
			}
			_ = json.NewEncoder(w).Encode(prs)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"html_url": "https://forgejo.example.com/acme/widgets/pulls/77", "number": 77, "head": map[string]string{"ref": "feature"}, "base": map[string]string{"ref": "main"}},
		})
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	prURL, prNumber, found, err := f.FindOpenPR(t.Context(), "acme/widgets", "feature", "main")
	require.NoError(t, err)
	assert.True(t, found, "a match on page 2 must still be found even though page 1 came back shorter than requested")
	assert.Equal(t, 77, prNumber)
	assert.Equal(t, "https://forgejo.example.com/acme/widgets/pulls/77", prURL)
	assert.Equal(t, []string{"1", "2"}, gotPages)
}

// TestForgejo_FindOpenPR_ExhaustsPagesReturnsError is FIX 2's regression
// test: a server that never returns an empty page (every page full, no
// match) must not be read as found=false — that would be indistinguishable
// from a genuine "no such PR is open," contradicting FindOpenPR's own
// godoc. Exhausting listOpenPRsMaxPages returns a non-nil error instead.
func TestForgejo_FindOpenPR_ExhaustsPagesReturnsError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prs := make([]map[string]any, listOpenPRsPageSize)
		for i := range prs {
			prs[i] = map[string]any{"html_url": "x", "number": i, "head": map[string]string{"ref": "unrelated"}, "base": map[string]string{"ref": "main"}}
		}
		_ = json.NewEncoder(w).Encode(prs)
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	_, _, found, err := f.FindOpenPR(t.Context(), "acme/widgets", "feature", "main")
	require.Error(t, err, "exhausting every page without an empty page or a match must not silently read as not-found")
	assert.False(t, found)
}

func TestApiBaseURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "https://forgejo.example.com/api/v1", apiBaseURL("forgejo.example.com"))
	assert.Equal(t, "http://127.0.0.1:8080/api/v1", apiBaseURL("http://127.0.0.1:8080"))
}
