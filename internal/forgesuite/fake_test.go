package forgesuite

import (
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/forge"
)

// TestProviderContract_FakeForge is the half of the Provider contract that
// runs in the ORDINARY gate: plain `go test ./...`, no build tag, no
// container, no opt-in env var. Everything it asserts is in
// contract_test.go, byte for byte the same code the real-Forgejo leg runs
// (TestProviderContract_RealForgejo, forgejo_integration_test.go).
//
// A reader checking which half ran where should look at exactly two
// things: this test's absence of a build tag, and that file's
// `//go:build integration` plus its LOAM_TEST_FORGEJO gate, which prints a
// SKIPPING banner to stderr rather than passing quietly.
func TestProviderContract_FakeForge(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available: the fake forge's smart-HTTP surface and every seeded repo need it")
	}
	Run(t, newFakeHarness(t))
}

// fakeTokens are the four credentials the fake forge registers, one per
// TokenKind. The fake models canPush and canPR as INDEPENDENT axes, which
// real Forgejo does not (see the harness's Token method), so the two
// restricted tokens here are genuinely different registrations where
// Forgejo needs only one.
const (
	fakeTokenFull       = "contract-full-token"
	fakeTokenNoGitWrite = "contract-read-only-token"
	fakeTokenNoPRScope  = "contract-no-pr-scope-token"
	fakeTokenBogus      = "contract-never-issued-token"
)

// fakeHarness drives the contract against internal/fakeforge. One Server
// backs the whole leg; isolation between cases comes from each SeedRepo
// call minting a uniquely-named repo, the same way the real leg isolates
// on one Forgejo instance.
type fakeHarness struct {
	server *fakeforge.Server
	http   *httptest.Server
	repos  atomic.Int64
}

// Ensure fakeHarness satisfies Harness at compile time.
var _ Harness = (*fakeHarness)(nil)

func newFakeHarness(t *testing.T) *fakeHarness {
	t.Helper()
	server, err := fakeforge.New(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	server.SetBaseURL(ts.URL)
	server.AddToken(fakeTokenFull)
	server.AddReadOnlyToken(fakeTokenNoGitWrite)
	server.AddTokenWithoutPRScope(fakeTokenNoPRScope)
	return &fakeHarness{server: server, http: ts}
}

func (h *fakeHarness) Name() string { return "fakeforge" }

func (h *fakeHarness) Host(t *testing.T) string {
	t.Helper()
	return h.http.URL
}

func (h *fakeHarness) Token(t *testing.T, kind TokenKind) string {
	t.Helper()
	switch kind {
	case TokenFull:
		return fakeTokenFull
	case TokenNoGitWrite:
		return fakeTokenNoGitWrite
	case TokenNoPRScope:
		return fakeTokenNoPRScope
	case TokenBogus:
		return fakeTokenBogus
	}
	t.Fatalf("fakeforge harness: unknown token kind %s", kind)
	return ""
}

func (h *fakeHarness) Provider(t *testing.T, token string) forge.Provider {
	t.Helper()
	return fakeforge.NewClient(h.http.URL, token)
}

func (h *fakeHarness) SeedRepo(t *testing.T) Repo {
	t.Helper()
	name := fmt.Sprintf("acme/contract-%03d", h.repos.Add(1))
	require.NoError(t, h.server.SeedRepoFiles(t.Context(), name,
		map[string][]byte{"README.md": []byte("# " + name + "\n")},
		fakeforge.SeedOptions{DefaultBranch: "main"}))
	return Repo{Path: name, GitURL: h.server.GitURL(name), MainBranch: "main"}
}

func (h *fakeHarness) MissingRepo(t *testing.T) Repo {
	t.Helper()
	name := fmt.Sprintf("acme/never-seeded-%03d", h.repos.Add(1))
	return Repo{Path: name, GitURL: h.server.GitURL(name), MainBranch: "main"}
}

func (h *fakeHarness) MergePR(t *testing.T, repo Repo, prNumber int) {
	t.Helper()
	require.NoError(t, h.server.MergePR(t.Context(), repo.Path, prNumber))
}
