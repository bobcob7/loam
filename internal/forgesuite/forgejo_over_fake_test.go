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

// TestProviderContract_ForgejoOverFake is the THIRD leg loam-c8v adds: the
// same contractCases every other leg runs, but through the REAL
// *forge.Forgejo client (production code, unmodified) bound to a
// fakeforge.Server's Forgejo-REST-shaped surface (internal/fakeforge's
// forgejoapi.go) instead of either the fake's own /provider/* Client
// (TestProviderContract_FakeForge) or an actual Forgejo container
// (TestProviderContract_RealForgejo).
//
// Neither existing leg can catch what this one does. The fake leg never
// touches forge.Forgejo's own request encoding or response decoding at
// all -- it substitutes fakeforge.Client at the Provider seam, so a
// drift in forge.Forgejo's URL building, header names, or JSON field
// names would be invisible to it. The real-Forgejo leg exercises that
// encoding/decoding for real, but only nightly and only with
// LOAM_TEST_FORGEJO=1 and a container running. This leg runs in the
// ORDINARY `go test ./...` gate -- no build tag, no container, no opt-in
// -- because a fakeforge.Server is just an in-process http.Handler; the
// only external dependency is the git binary, already required by every
// other fakeforge-backed test.
//
// A pass here is exactly the evidence loam-c8v's bead notes ask for:
// proof that forge.Forgejo's own wire encoding survives a round trip
// against forgejoapi.go's PR-lifecycle routes, not merely that the fake
// can answer its own /provider/* shape.
func TestProviderContract_ForgejoOverFake(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available: the fake forge's smart-HTTP surface and every seeded repo need it")
	}
	Run(t, newForgejoOverFakeHarness(t))
}

// forgejoOverFakeTokens are the four credentials this leg's fakeforge.Server
// registers, one per TokenKind -- the same registrations
// TestProviderContract_FakeForge's fakeHarness makes, since both legs are
// backed by the SAME fake token model (fakeforge/server.go's
// tokenScope: canPush and canPR as independent axes, distinct from real
// Forgejo's single write:repository scope -- see the package doc's "one
// further divergence").
const (
	forgejoOverFakeTokenFull       = "contract-over-fake-full-token"
	forgejoOverFakeTokenNoGitWrite = "contract-over-fake-read-only-token"
	forgejoOverFakeTokenNoPRScope  = "contract-over-fake-no-pr-scope-token"
	forgejoOverFakeTokenBogus      = "contract-over-fake-never-issued-token"
)

// forgejoOverFakeHarness drives the contract against internal/fakeforge's
// Forgejo-REST-shaped surface, THROUGH the real *forge.Forgejo client.
// Fixture plumbing (SeedRepo, MissingRepo, MergePR) goes straight to the
// fakeforge.Server the same way fakeHarness's does, since none of that is
// part of forge.Provider; only Provider() differs from fakeHarness, which
// is the entire point of this leg existing as a separate one.
type forgejoOverFakeHarness struct {
	server *fakeforge.Server
	http   *httptest.Server
	client *http.Client
	repos  atomic.Int64
}

// Ensure forgejoOverFakeHarness satisfies Harness at compile time.
var _ Harness = (*forgejoOverFakeHarness)(nil)

func newForgejoOverFakeHarness(t *testing.T) *forgejoOverFakeHarness {
	t.Helper()
	server, err := fakeforge.New(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	server.SetBaseURL(ts.URL)
	server.AddToken(forgejoOverFakeTokenFull)
	server.AddReadOnlyToken(forgejoOverFakeTokenNoGitWrite)
	server.AddTokenWithoutPRScope(forgejoOverFakeTokenNoPRScope)
	return &forgejoOverFakeHarness{server: server, http: ts, client: &http.Client{}}
}

func (h *forgejoOverFakeHarness) Name() string { return "forgejo-over-fake" }

func (h *forgejoOverFakeHarness) Host(t *testing.T) string {
	t.Helper()
	return h.http.URL
}

func (h *forgejoOverFakeHarness) Token(t *testing.T, kind TokenKind) string {
	t.Helper()
	switch kind {
	case TokenFull:
		return forgejoOverFakeTokenFull
	case TokenNoGitWrite:
		return forgejoOverFakeTokenNoGitWrite
	case TokenNoPRScope:
		return forgejoOverFakeTokenNoPRScope
	case TokenBogus:
		return forgejoOverFakeTokenBogus
	}
	t.Fatalf("forgejo-over-fake harness: unknown token kind %s", kind)
	return ""
}

// Provider returns the REAL production client, bound to this leg's fake
// base URL and token exactly the way cmd/server's composition root binds
// one to a real Forgejo host (cmd/server/sync.go's forgePRTracker) --
// unmodified forge.Forgejo, the whole subject of this leg.
func (h *forgejoOverFakeHarness) Provider(t *testing.T, token string) forge.Provider {
	t.Helper()
	return forge.NewForgejo(h.http.URL, token, h.client, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func (h *forgejoOverFakeHarness) SeedRepo(t *testing.T) Repo {
	t.Helper()
	name := fmt.Sprintf("acme/over-fake-%03d", h.repos.Add(1))
	require.NoError(t, h.server.SeedRepoFiles(t.Context(), name,
		map[string][]byte{"README.md": []byte("# " + name + "\n")},
		fakeforge.SeedOptions{DefaultBranch: "main"}))
	return Repo{Path: name, GitURL: h.server.GitURL(name), MainBranch: "main"}
}

func (h *forgejoOverFakeHarness) MissingRepo(t *testing.T) Repo {
	t.Helper()
	name := fmt.Sprintf("acme/over-fake-never-seeded-%03d", h.repos.Add(1))
	return Repo{Path: name, GitURL: h.server.GitURL(name), MainBranch: "main"}
}

func (h *forgejoOverFakeHarness) MergePR(t *testing.T, repo Repo, prNumber int) {
	t.Helper()
	require.NoError(t, h.server.MergePR(t.Context(), repo.Path, prNumber))
}
