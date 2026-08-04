package forgesuite

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/forge"
)

// TestProviderContract_GitHubOverFake is loam-tmds.3's fake-backed leg:
// the same contractCases every other leg runs, through the REAL
// *forge.GitHub client (production code, unmodified) bound to a
// fakeforge.Server's GitHub-REST-shaped surface (internal/fakeforge's
// githubapi.go, loam-tmds.4) instead of a real GitHub host.
//
// This is GitHub's counterpart to TestProviderContract_ForgejoOverFake,
// and for the identical reason that leg exists rather than relying on
// the Forgejo fake's simplified /provider/* Client: a pass here is
// evidence that forge.GitHub's own request encoding (URL building,
// header names, the head=owner:branch query format, JSON field names)
// and response decoding survive a real round trip, not merely that some
// double can be made to satisfy forge.Provider. Unlike Forgejo,
// GitHub has no separate simplified-protocol Client in fakeforge — see
// githubapi.go's own doc comment on why a dialect was added there
// instead of a third wire format — so this IS the whole fake-backed leg
// for GitHub, not one of several.
//
// Runs in the ORDINARY `go test ./...` gate: no build tag, no
// container, no opt-in env var. A fakeforge.Server is just an in-process
// http.Handler; the only external dependency is the git binary, already
// required by every other fakeforge-backed test in this package.
func TestProviderContract_GitHubOverFake(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available: the fake forge's smart-HTTP surface and every seeded repo need it")
	}
	Run(t, newGitHubOverFakeHarness(t))
}

// githubOverFakeTokens are the three credentials this leg's
// fakeforge.Server registers, one per TokenKind — the same single-axis
// tokenScope model (server.go's AddToken/AddReadOnlyToken) the Forgejo
// legs use, since loam-tmds.2's GitHub provider makes the identical
// choice: "repo" scope is required unconditionally, with no
// independent git-push-only or PR-only axis (github.go's
// githubRequiredScope doc comment).
const (
	githubOverFakeTokenFull     = "ghp_contractOverFakeFullToken0000000000"
	githubOverFakeTokenReadOnly = "ghp_contractOverFakeReadOnlyToken000000"
	githubOverFakeTokenBogus    = "ghp_contractOverFakeNeverIssuedToken000"
)

// githubOverFakeHarness drives the contract against internal/fakeforge's
// GitHub-REST-shaped surface, THROUGH the real *forge.GitHub client.
// Fixture plumbing (SeedRepo, MissingRepo, MergePR) goes straight to the
// fakeforge.Server the same way forgejoOverFakeHarness's does, since
// none of that is part of forge.Provider; only Provider() is
// GitHub-specific.
type githubOverFakeHarness struct {
	server *fakeforge.Server
	http   *httptest.Server
	client *http.Client
	repos  atomic.Int64
}

// Ensure githubOverFakeHarness satisfies Harness at compile time.
var _ Harness = (*githubOverFakeHarness)(nil)

func newGitHubOverFakeHarness(t *testing.T) *githubOverFakeHarness {
	t.Helper()
	server, err := fakeforge.New(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	server.SetBaseURL(ts.URL)
	server.AddToken(githubOverFakeTokenFull)
	server.AddReadOnlyToken(githubOverFakeTokenReadOnly)
	return &githubOverFakeHarness{server: server, http: ts, client: &http.Client{}}
}

func (h *githubOverFakeHarness) Name() string { return "github-over-fake" }

func (h *githubOverFakeHarness) Host(t *testing.T) string {
	t.Helper()
	return h.http.URL
}

func (h *githubOverFakeHarness) Token(t *testing.T, kind TokenKind) string {
	t.Helper()
	switch kind {
	case TokenFull:
		return githubOverFakeTokenFull
	case TokenReadOnly:
		return githubOverFakeTokenReadOnly
	case TokenBogus:
		return githubOverFakeTokenBogus
	}
	t.Fatalf("github-over-fake harness: unknown token kind %s", kind)
	return ""
}

// Provider returns the REAL production client, bound to this leg's fake
// base URL and token exactly the way cmd/server's forge.NewProvider
// would bind one for a repo whose forge_host resolved to KindGitHub —
// unmodified forge.GitHub, the whole subject of this leg.
func (h *githubOverFakeHarness) Provider(t *testing.T, token string) forge.Provider {
	t.Helper()
	return forge.NewGitHub(h.http.URL, token, h.client, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func (h *githubOverFakeHarness) SeedRepo(t *testing.T) Repo {
	t.Helper()
	name := fmt.Sprintf("acme/gh-over-fake-%03d", h.repos.Add(1))
	require.NoError(t, h.server.SeedRepoFiles(t.Context(), name,
		map[string][]byte{"README.md": []byte("# " + name + "\n")},
		fakeforge.SeedOptions{DefaultBranch: "main"}))
	return Repo{Path: name, GitURL: h.server.GitURL(name), MainBranch: "main"}
}

func (h *githubOverFakeHarness) MissingRepo(t *testing.T) Repo {
	t.Helper()
	name := fmt.Sprintf("acme/gh-over-fake-never-seeded-%03d", h.repos.Add(1))
	return Repo{Path: name, GitURL: h.server.GitURL(name), MainBranch: "main"}
}

func (h *githubOverFakeHarness) MergePR(t *testing.T, repo Repo, prNumber int) {
	t.Helper()
	require.NoError(t, h.server.MergePR(t.Context(), repo.Path, prNumber))
}
