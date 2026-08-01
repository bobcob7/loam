//go:build integration

package forgesuite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	testcontainers "github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bobcob7/loam/internal/forge"
)

// forgejoImage is the Forgejo the contract is verified against. Pinned to
// an exact tag, not a floating one: every empirical claim in internal/forge
// and internal/fakeforge ("verified against Forgejo 9.0.3") is a claim
// about THIS version's wire behaviour, and a suite that silently drifted to
// a newer image would stop being evidence for those comments while still
// reporting green.
const forgejoImage = "codeberg.org/forgejo/forgejo:9.0.3"

// forgejoOptInEnv gates the real-Forgejo leg. Per docs/testing-spec.md's CI
// Stages table this leg belongs to the NIGHTLY stage, so `task
// test:integration` and the per-PR `integration` job must not need a
// Forgejo container: the build tag alone is not enough, because everything
// else under that tag runs per-PR.
const forgejoOptInEnv = "LOAM_TEST_FORGEJO"

const (
	forgejoAdminUser     = "loamadmin"
	forgejoAdminPassword = "loam-Contract-Passw0rd"
	forgejoAdminEmail    = "loamadmin@example.invalid"
	// forgejoBogusToken is 40 hex characters — the shape Forgejo issues —
	// that Forgejo has never issued.
	forgejoBogusToken = "0000000000000000000000000000000000000bad"
)

// TestProviderContract_RealForgejo is the half of the Provider contract
// that runs against a REAL Forgejo. It executes contract_test.go's
// contractCases unchanged — the same code TestProviderContract_FakeForge
// runs — so any disagreement between the fake and Forgejo about an error
// class, a not-found shape, an idempotency path, or pagination fails here.
//
// When the opt-in is absent it SKIPS with a banner naming exactly what did
// NOT get verified — because a contract suite that quietly ran only the
// fake, and reported the same "ok" either way, would license the fake on
// the strength of a comparison that never happened.
//
// Be precise about where that banner is visible, since it is easy to
// over-promise: `go test` BUFFERS a passing package's stdout and stderr and
// prints it only on failure or under -v. That is a property of the go tool,
// not of this print — internal/storesuite's own TestMain banner is
// swallowed the same way. So the banner shows under `go test -v` (which is
// how `task test:contract:forgejo` runs this package), and in a plain
// `go test -tags=integration ./...` this leg is indistinguishable from any
// other skipped test. The durable, always-visible signals that the real
// half is opt-in are this file's build tag, this test's name next to
// TestProviderContract_FakeForge, and the package doc.
func TestProviderContract_RealForgejo(t *testing.T) {
	t.Parallel()
	if os.Getenv(forgejoOptInEnv) != "1" {
		banner := fmt.Sprintf(
			"\n=== SKIPPING the REAL-FORGEJO half of the Provider contract suite ===\n"+
				"    %s is not set to 1, so only the fake half (TestProviderContract_FakeForge)\n"+
				"    verified anything in this run. Nothing here compared the fake against %s.\n"+
				"    Run it with: %s=1 go test -tags=integration -count=1 ./internal/forgesuite/...\n"+
				"    (or `task test:contract:forgejo`)\n",
			forgejoOptInEnv, forgejoImage, forgejoOptInEnv)
		fmt.Fprint(os.Stderr, banner)
		t.Skipf("%s is not set to 1: the real-Forgejo contract leg is nightly-only (docs/testing-spec.md, CI Stages)", forgejoOptInEnv)
	}
	Run(t, newForgejoHarness(t))
}

// forgejoHarness drives the contract against a real Forgejo container. One
// container backs the whole leg (containers are the expensive part, not
// repos), and isolation comes from every SeedRepo call creating a fresh,
// private repository.
type forgejoHarness struct {
	baseURL     string
	fullToken   string
	scopedToken string
	http        *http.Client
	repos       atomic.Int64
}

// Ensure forgejoHarness satisfies Harness at compile time.
var _ Harness = (*forgejoHarness)(nil)

func newForgejoHarness(t *testing.T) *forgejoHarness {
	t.Helper()
	ctx := t.Context()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        forgejoImage,
			ExposedPorts: []string{"3000/tcp"},
			Env: map[string]string{
				// INSTALL_LOCK skips the interactive first-run installer;
				// without it every API call answers the setup page instead.
				"FORGEJO__security__INSTALL_LOCK":        "true",
				"FORGEJO__database__DB_TYPE":             "sqlite3",
				"FORGEJO__database__PATH":                "/data/gitea/forgejo.db",
				"FORGEJO__server__OFFLINE_MODE":          "true",
				"FORGEJO__service__DISABLE_REGISTRATION": "true",
				"FORGEJO__log__LEVEL":                    "Warn",
			},
			WaitingFor: wait.ForHTTP("/api/v1/version").
				WithPort("3000/tcp").
				WithStartupTimeout(3 * time.Minute),
		},
	})
	require.NoError(t, err, "starting the Forgejo container")
	t.Cleanup(func() {
		if err := container.Terminate(context.WithoutCancel(ctx)); err != nil {
			t.Logf("terminating the Forgejo container: %v", err)
		}
	})
	endpoint, err := container.PortEndpoint(ctx, "3000/tcp", "http")
	require.NoError(t, err)
	h := &forgejoHarness{baseURL: strings.TrimSuffix(endpoint, "/"), http: &http.Client{Timeout: 30 * time.Second}}
	// Forgejo refuses to run its CLI as root, and the image's own service
	// user is "git"; the admin user and its tokens have to exist before any
	// API call can authenticate.
	forgejoExec(t, container, "creating the admin user",
		"forgejo", "admin", "user", "create",
		"--admin", "--username", forgejoAdminUser, "--password", forgejoAdminPassword,
		"--email", forgejoAdminEmail, "--must-change-password=false")
	h.fullToken = forgejoExec(t, container, "issuing the full-scope token",
		"forgejo", "admin", "user", "generate-access-token",
		"-u", forgejoAdminUser, "--token-name", "contract-full", "--scopes", "all", "--raw")
	// ONE read:repository token covers TokenReadOnly, because Forgejo 9.0.3
	// gates git push and PR creation on the SAME write:repository scope
	// (verified live): there is no real token that can push but not open
	// PRs, or vice versa. See Token below.
	h.scopedToken = forgejoExec(t, container, "issuing the read-only token",
		"forgejo", "admin", "user", "generate-access-token",
		"-u", forgejoAdminUser, "--token-name", "contract-readonly", "--scopes", "read:repository", "--raw")
	return h
}

// forgejoExec runs one command inside the container as the "git" service
// user and returns its trimmed stdout, failing the test on a non-zero exit.
func forgejoExec(t *testing.T, container testcontainers.Container, what string, cmd ...string) string {
	t.Helper()
	code, reader, err := container.Exec(t.Context(), cmd, tcexec.WithUser("git"), tcexec.Multiplexed())
	require.NoError(t, err, "%s", what)
	out, err := io.ReadAll(reader)
	require.NoError(t, err, "%s: reading output", what)
	require.Zero(t, code, "%s: exit %d: %s", what, code, out)
	return strings.TrimSpace(string(out))
}

func (h *forgejoHarness) Name() string { return "forgejo" }

func (h *forgejoHarness) Host(t *testing.T) string {
	t.Helper()
	return h.baseURL
}

func (h *forgejoHarness) Token(t *testing.T, kind TokenKind) string {
	t.Helper()
	switch kind {
	case TokenFull:
		return h.fullToken
	case TokenReadOnly:
		return h.scopedToken
	case TokenBogus:
		return forgejoBogusToken
	}
	t.Fatalf("forgejo harness: unknown token kind %s", kind)
	return ""
}

func (h *forgejoHarness) Provider(t *testing.T, token string) forge.Provider {
	t.Helper()
	return forge.NewForgejo(h.baseURL, token, h.http, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func (h *forgejoHarness) SeedRepo(t *testing.T) Repo {
	t.Helper()
	name := fmt.Sprintf("contract-%03d", h.repos.Add(1))
	// private:true matters: on a PUBLIC repo Forgejo serves git reads
	// anonymously, so CheckRepo's read probe would succeed with a rejected
	// credential and the bogus-token row would disagree with the fake for a
	// reason that has nothing to do with the Provider. Loam's own repos are
	// private, so this is also the realistic shape.
	h.apiDo(t, http.MethodPost, "/user/repos", h.fullToken, map[string]any{
		"name": name, "auto_init": true, "default_branch": "main", "private": true,
	}, http.StatusCreated, nil)
	return h.repoAt(name)
}

func (h *forgejoHarness) MissingRepo(t *testing.T) Repo {
	t.Helper()
	return h.repoAt(fmt.Sprintf("never-created-%03d", h.repos.Add(1)))
}

func (h *forgejoHarness) repoAt(name string) Repo {
	path := forgejoAdminUser + "/" + name
	return Repo{Path: path, GitURL: h.baseURL + "/" + path + ".git", MainBranch: "main"}
}

func (h *forgejoHarness) MergePR(t *testing.T, repo Repo, prNumber int) {
	t.Helper()
	h.apiDo(t, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo.Path, prNumber), h.fullToken,
		map[string]any{"Do": "merge"}, http.StatusOK, nil)
}

// apiDo issues one authenticated Forgejo REST call for HARNESS plumbing
// only — repo creation and the forge-side merge, neither of which is part
// of forge.Provider. Nothing the contract asserts goes through here.
func (h *forgejoHarness) apiDo(t *testing.T, method, path, token string, body any, wantStatus int, out any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, h.baseURL+"/api/v1"+path, reader)
	require.NoError(t, err)
	req.Header.Set("Authorization", "token "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	require.NoError(t, err, "%s %s", method, path)
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, wantStatus, resp.StatusCode, "%s %s: %s", method, path, payload)
	if out != nil {
		require.NoError(t, json.Unmarshal(payload, out))
	}
}
