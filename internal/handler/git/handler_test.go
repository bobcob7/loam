package git

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/reposstore"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestParseGitRequest_RecognizedShapes proves every request shape docs/
// git-spec.md "Endpoint & Protocol" defines parses to the expected repo
// name, service, and info/refs flag.
func TestParseGitRequest_RecognizedShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		method   string
		target   string
		wantRepo string
		wantSvc  string
		wantInfo bool
	}{
		{
			name:     "info refs upload-pack",
			method:   http.MethodGet,
			target:   "/git/acme/widgets.git/info/refs?service=git-upload-pack",
			wantRepo: "acme/widgets",
			wantSvc:  serviceUploadPack,
			wantInfo: true,
		},
		{
			name:     "info refs receive-pack",
			method:   http.MethodGet,
			target:   "/git/acme/widgets.git/info/refs?service=git-receive-pack",
			wantRepo: "acme/widgets",
			wantSvc:  serviceReceivePack,
			wantInfo: true,
		},
		{
			name:     "post upload-pack",
			method:   http.MethodPost,
			target:   "/git/acme/widgets.git/git-upload-pack",
			wantRepo: "acme/widgets",
			wantSvc:  serviceUploadPack,
		},
		{
			name:     "post receive-pack",
			method:   http.MethodPost,
			target:   "/git/acme/widgets.git/git-receive-pack",
			wantRepo: "acme/widgets",
			wantSvc:  serviceReceivePack,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.target, nil)
			got, ok := parseGitRequest(req)
			require.True(t, ok)
			assert.Equal(t, tc.wantRepo, got.repoName)
			assert.Equal(t, tc.wantSvc, got.service)
			assert.Equal(t, tc.wantInfo, got.isInfoRefs)
		})
	}
}

// TestParseGitRequest_RejectsEverythingElse proves the shapes docs/git-
// spec.md says are not served (dumb HTTP, unrecognized ?service=, wrong
// method, no ".git/" boundary at all) are all rejected -- this handler's
// own defense in depth for whatever internal/handler.GitRoleGate does not
// reject first (see ServeHTTP's doc comment on the 403/404 overlap).
func TestParseGitRequest_RejectsEverythingElse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		target string
	}{
		{"dumb HEAD", http.MethodGet, "/git/acme/widgets.git/HEAD"},
		{"dumb objects/info/packs", http.MethodGet, "/git/acme/widgets.git/objects/info/packs"},
		{"info/refs no service", http.MethodGet, "/git/acme/widgets.git/info/refs"},
		{"info/refs unknown service", http.MethodGet, "/git/acme/widgets.git/info/refs?service=git-upload-archive"},
		{"POST info/refs", http.MethodPost, "/git/acme/widgets.git/info/refs?service=git-upload-pack"},
		{"GET upload-pack", http.MethodGet, "/git/acme/widgets.git/git-upload-pack"},
		{"no .git boundary", http.MethodGet, "/git/acme/widgets/info/refs?service=git-upload-pack"},
		{"empty repo name", http.MethodGet, "/git/.git/info/refs?service=git-upload-pack"},
		{"outside prefix", http.MethodGet, "/other/acme/widgets.git/info/refs?service=git-upload-pack"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.target, nil)
			_, ok := parseGitRequest(req)
			assert.False(t, ok, "expected %s %s to be rejected", tc.method, tc.target)
		})
	}
}

// TestServeHTTP_UnenrolledRepoIs404 proves docs/git-spec.md's "Repo not
// enrolled -> 404" for a request shape that is otherwise entirely valid --
// the mutation this kills is a handler that returns 200 (or any non-404)
// for a repo reposstore.ErrNotFound resolves as absent.
func TestServeHTTP_UnenrolledRepoIs404(t *testing.T) {
	t.Parallel()
	repos := &RepoStoreMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{}, reposstore.ErrNotFound
		},
	}
	h := New(t.TempDir(), repos, discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/git/acme/ghost.git/info/refs?service=git-upload-pack", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
}

// enrolledButUnmirroredRepoStore resolves repoName as enrolled -- the repos
// row exists -- independent of whatever is (or is not) on disk at its
// mirror path. Used to reproduce loam-1gq: the window between EnrollRepo
// writing the row and the first successful clone/sync landing.
func enrolledButUnmirroredRepoStore(repoName string) *RepoStoreMock {
	return &RepoStoreMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{Name: repoName}, nil
		},
	}
}

// TestServeHTTP_EnrolledRepoWithNoMirrorIsNotReady reproduces loam-1gq
// directly: repos.GetRepoByName resolves "acme/widgets" (the repo IS
// enrolled), but h's dataDir (a fresh t.TempDir()) has no mirrors/
// subtree at all -- exactly the on-disk state between EnrollRepo's row
// write and the first successful clone/sync, which stays open
// indefinitely if that first clone fails (docs/sync-spec.md: sync_state
// goes to "error", not back to unenrolled).
//
// Before the fix, ServeHTTP wrote StatusOK and the pkt-line service
// header + flush unconditionally, then let `git upload-pack
// --advertise-refs` fail against the missing directory with nothing
// else written -- 200 with a body that LOOKS like a valid, empty ref
// advertisement (indistinguishable on the wire from a real repo with no
// refs), not the distinct "not mirrored yet" case it actually is. This
// test pins both the status code AND the body shape, so a regression
// that fixes the code but leaves the old advertisement-shaped body (or
// vice versa) cannot pass silently -- see the bead's own "vacuous
// assertion" warning.
func TestServeHTTP_EnrolledRepoWithNoMirrorIsNotReady(t *testing.T) {
	t.Parallel()
	repos := enrolledButUnmirroredRepoStore("acme/widgets")
	h := New(t.TempDir(), repos, discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/git/acme/widgets.git/info/refs?service=git-upload-pack", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "an enrolled repo with no mirror on disk must not be a bare 200")
	assert.NotEqual(t, http.StatusNotFound, rec.Code, "must not be 404 either: the repo IS enrolled, unlike TestServeHTTP_UnenrolledRepoIs404")
	body := rec.Body.String()
	wantAdvertisementHeader := string(append(pktLine("# service=git-upload-pack\n"), flushPkt...))
	assert.NotContains(t, body, wantAdvertisementHeader, "the body must not look like a valid (if empty) git ref advertisement")
	assert.Contains(t, body, "acme/widgets", "the message must name the repo")
	assert.Contains(t, body, "not yet mirrored", "the message must say why, not merely that something failed")
}

// TestServeHTTP_EnrolledRepoWithNoMirrorIsNotReady_UploadPackRPC proves the
// same fix covers the POST git-upload-pack RPC endpoint, not just GET
// info/refs: serveRPC has the identical shape (WriteHeader(200) before
// running the subprocess), so it shared the same hole before ServeHTTP's
// check moved ahead of both dispatch paths.
func TestServeHTTP_EnrolledRepoWithNoMirrorIsNotReady_UploadPackRPC(t *testing.T) {
	t.Parallel()
	repos := enrolledButUnmirroredRepoStore("acme/widgets")
	h := New(t.TempDir(), repos, discardLogger())
	req := httptest.NewRequest(http.MethodPost, "/git/acme/widgets.git/git-upload-pack", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "not yet mirrored")
}

// TestServeHTTP_EnrolledRepoWithNoMirrorIsNotReady_ReceivePackRPC is the
// receive-pack (push) analogue: it must fail closed on the missing mirror
// before ever reaching the identity/hook-environment logic, since there is
// no mirror for receive-pack to run against either.
func TestServeHTTP_EnrolledRepoWithNoMirrorIsNotReady_ReceivePackRPC(t *testing.T) {
	t.Parallel()
	repos := enrolledButUnmirroredRepoStore("acme/widgets")
	h := New(t.TempDir(), repos, discardLogger())
	req := httptest.NewRequest(http.MethodPost, "/git/acme/widgets.git/git-receive-pack", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "not yet mirrored")
}

// TestServeHTTP_MalformedShapeIs404 proves a request outside the three
// smart-HTTP shapes gets the same 404 as an unenrolled repo, never a 500
// or a panic -- exercised directly against Handler (bypassing
// internal/handler.GitRoleGate entirely), which is exactly the case
// ServeHTTP's own doc comment says stays this handler's job.
func TestServeHTTP_MalformedShapeIs404(t *testing.T) {
	t.Parallel()
	repos := &RepoStoreMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			t.Fatal("repo store must not be consulted for a malformed shape")
			return reposstore.Repo{}, nil
		},
	}
	h := New(t.TempDir(), repos, discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/git/acme/widgets.git/HEAD", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestServeHTTP_RepoStoreErrorIs500NotSilent proves a genuine repo-store
// failure (not "unenrolled") maps to 500, not a 404 that would
// misleadingly read as "not enrolled" to whoever is debugging it.
func TestServeHTTP_RepoStoreErrorIs500NotSilent(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	repos := &RepoStoreMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{}, boom
		},
	}
	h := New(t.TempDir(), repos, discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/git/acme/widgets.git/info/refs?service=git-upload-pack", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestParseGitRequest_RejectsPathTraversalSegments proves the MUST-FIX
// this bead's review found: a repo name containing a ".." segment --
// whether written literally or arriving via a percent-encoded slash
// net/http already decodes before this handler ever sees r.URL.Path
// (e.g. "..%2f..%2f..%2ftmp%2fevil" decodes to the literal path segment
// "../../../tmp/evil", confirmed against Go's own url.Parse) -- is
// rejected by parseGitRequest itself, by construction (validRepoName's
// allowlist), before anything downstream ever calls the repo store or
// joins the name into a filesystem path via mirrorpath.Dir.
func TestParseGitRequest_RejectsPathTraversalSegments(t *testing.T) {
	t.Parallel()
	targets := []string{
		"http://example.invalid/git/acme/../../../tmp/evil.git/info/refs?service=git-upload-pack",
		"http://example.invalid/git/acme/..%2f..%2f..%2ftmp%2fevil.git/info/refs?service=git-upload-pack",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodGet, target, nil)
			require.NoError(t, err)
			require.Contains(t, req.URL.Path, "..", "the request's DECODED path must actually contain a traversal segment for this test to mean anything -- otherwise it would pass vacuously")
			_, ok := parseGitRequest(req)
			assert.False(t, ok, "a repo name containing a traversal segment must be rejected: %q", req.URL.Path)
		})
	}
}

// TestServeHTTP_PathTraversalRepoNameIs404BeforeAnyStoreLookup proves the
// same rejection end-to-end through ServeHTTP, with the repo store itself
// wired to fail the test (via t.Fatal) if it is ever consulted: the
// traversal segment must never reach GetRepoByName, or mirrorpath.Dir,
// at all -- so a permissive or missing CHECK constraint on repos.name (as
// this bead's review found: 0001_init.up.sql has none) can never turn
// this into a real filesystem access outside LOAM_DATA_DIR. The
// rejection happens at parseGitRequest, strictly before any store call.
func TestServeHTTP_PathTraversalRepoNameIs404BeforeAnyStoreLookup(t *testing.T) {
	t.Parallel()
	repos := &RepoStoreMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			t.Fatal("the repo store must never be consulted for a path-traversal repo name")
			return reposstore.Repo{}, nil
		},
	}
	h := New(t.TempDir(), repos, discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/git/acme/..%2f..%2f..%2ftmp%2fevil.git/info/refs?service=git-upload-pack", nil)
	require.Contains(t, req.URL.Path, "..", "the request's DECODED path must actually contain a traversal segment for this test to mean anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestValidRepoName_Table pins validRepoName's allowlist behavior
// directly against the specific shapes this bead's review named: exactly
// two non-empty segments, each starting with an alphanumeric and
// containing only alphanumerics/'.'/'_'/'-'.
func TestValidRepoName_Table(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ok   bool
	}{
		{"acme/widgets", true},
		{"acme/doc-server.wiki", true},
		{"acme.corp/wid_gets.v2", true},
		{"../../../tmp/evil", false},
		{"acme/..", false},
		{"acme/.", false},
		{"acme//evil", false},
		{"/acme", false},
		{"acme/", false},
		{"acme", false},
		{"acme/sub/widgets", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.ok, validRepoName(tc.name))
		})
	}
}
