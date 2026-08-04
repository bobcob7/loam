//go:build integration

package forgesuite

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
)

// TestProviderContract_RealGitHub is loam-tmds.3's real-GitHub leg. It is
// written, gated, and — per this bead's own explicit instruction —
// DELIBERATELY NEVER EXECUTED by the agent that wrote it. Enabling it
// creates, mutates, and deletes real repositories under whatever account
// LOAM_TEST_GITHUB_TOKEN belongs to; that is the repository owner's
// decision to make, not an agent's (mirroring loam-ytt2.10's precedent:
// report the gap with evidence rather than reach for credentials nobody
// handed you, or ship something unvalidated).
//
// # What a maintainer must supply to run this leg
//
//  1. LOAM_TEST_GITHUB=1 — the opt-in, mirroring forgejoOptInEnv exactly.
//     Absent, this test SKIPS with a banner, the same contract
//     forgejo_integration_test.go's own doc comment explains in detail.
//  2. LOAM_TEST_GITHUB_TOKEN — a classic PAT with "repo" AND
//     "delete_repo" scope, for a DEDICATED, THROWAWAY GitHub account —
//     never a maintainer's own. This leg creates and deletes real
//     repositories every run; delete_repo is what lets SeedRepo's
//     t.Cleanup actually remove them rather than leaking
//     "contract-NNN" repos on the account forever.
//  3. LOAM_TEST_GITHUB_READONLY_TOKEN — a SECOND classic PAT for the
//     same account, created with NO scopes granted at all. This is not
//     a simplification: classic PATs have no scope that grants
//     READ-ONLY access to a repo the way Forgejo's read:repository
//     does. "repo" grants full read+write to private repos; there is
//     no narrower private-repo-read scope. A scopeless token still
//     authenticates (ValidateToken's "does it authenticate at all"
//     check must pass) but can neither open a PR (no "repo" scope) nor
//     push (git's receive-pack advertisement requires write access) —
//     while git READS still succeed because of decision 4 below.
//  4. SeedRepo creates PUBLIC repos, unlike the real-Forgejo leg's
//     PRIVATE ones. This is the consequence of decision 3: a scopeless
//     token can still read a PUBLIC repo over git (GitHub serves public
//     reads to any authenticated — or even anonymous — request), which
//     is what lets CheckRepo/ReadOnlyTokenIsNoWriteAccess and
//     ValidateToken/ReadOnlyTokenIsInsufficientScope both hold: read
//     succeeds, write and PR-opening are both denied. A private repo
//     would instead make the read-only token's git READ fail too,
//     collapsing this leg's "read-only" case into "no access at all" —
//     a different case the contract does not have a row for.
//
// None of the four decisions above have been verified against a live
// GitHub account: this bead's instructions are explicit that this leg
// must be written and gated, not run, so "no scope grants read-only
// private-repo access" and "a scopeless classic PAT still authenticates"
// are this author's understanding of GitHub's documented scope model
// (docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-
// oauth-apps), not empirical findings the way forgejo_integration_test.go's
// comments are. A maintainer running this leg for the first time should
// expect to correct this file.
func TestProviderContract_RealGitHub(t *testing.T) {
	t.Parallel()
	if os.Getenv(githubOptInEnv) != "1" {
		banner := fmt.Sprintf(
			"\n=== SKIPPING the REAL-GITHUB half of the Provider contract suite ===\n"+
				"    %s is not set to 1, so only the fake half (TestProviderContract_GitHubOverFake)\n"+
				"    verified anything in this run. Nothing here compared the fake against real GitHub.\n"+
				"    Run it with: %s=1 %s=<classic PAT, repo+delete_repo scope, DEDICATED bot account> \\\n"+
				"      %s=<classic PAT, NO scopes, same account> \\\n"+
				"      go test -tags=integration -count=1 ./internal/forgesuite/... -v\n"+
				"    This has never been executed — see this test's own doc comment for what it mutates\n"+
				"    and why the credentials must belong to a dedicated, throwaway account.\n",
			githubOptInEnv, githubOptInEnv, githubTokenEnv, githubReadOnlyTokenEnv)
		fmt.Fprint(os.Stderr, banner)
		t.Skipf("%s is not set to 1: the real-GitHub contract leg is nightly-only and has never been run — see TestProviderContract_RealGitHub's doc comment", githubOptInEnv)
	}
	Run(t, newRealGitHubHarness(t))
}

// githubOptInEnv gates the real-GitHub leg, mirroring forgejoOptInEnv.
const githubOptInEnv = "LOAM_TEST_GITHUB"

// githubTokenEnv and githubReadOnlyTokenEnv name the two credentials a
// maintainer must supply — see TestProviderContract_RealGitHub's doc
// comment for the exact scopes each needs and why.
const (
	githubTokenEnv         = "LOAM_TEST_GITHUB_TOKEN"
	githubReadOnlyTokenEnv = "LOAM_TEST_GITHUB_READONLY_TOKEN"
)

// githubBogusToken is shaped like a classic PAT (the "ghp_" prefix
// GitHub's own token-scanning documentation describes) that GitHub has
// never issued.
const githubBogusToken = "ghp_0000000000000000000000000000000000"

// realGitHubHarness drives the contract against the real
// https://api.github.com, using whatever GitHub account
// LOAM_TEST_GITHUB_TOKEN belongs to. Unlike forgejoHarness (a fresh
// container per test run) there is no isolated instance here: every run
// creates and deletes real repos on a real, shared account, which is
// exactly why decision 2 in TestProviderContract_RealGitHub's doc
// comment insists on a dedicated one.
type realGitHubHarness struct {
	token         string
	readOnlyToken string
	owner         string
	http          *http.Client
	repos         atomic.Int64
}

// Ensure realGitHubHarness satisfies Harness at compile time.
var _ Harness = (*realGitHubHarness)(nil)

func newRealGitHubHarness(t *testing.T) *realGitHubHarness {
	t.Helper()
	token := os.Getenv(githubTokenEnv)
	readOnlyToken := os.Getenv(githubReadOnlyTokenEnv)
	require.NotEmpty(t, token, "%s must be set when %s=1", githubTokenEnv, githubOptInEnv)
	require.NotEmpty(t, readOnlyToken, "%s must be set when %s=1", githubReadOnlyTokenEnv, githubOptInEnv)
	h := &realGitHubHarness{token: token, readOnlyToken: readOnlyToken, http: &http.Client{Timeout: 30 * time.Second}}
	var user struct {
		Login string `json:"login"`
	}
	h.apiDo(t, http.MethodGet, "/user", h.token, nil, http.StatusOK, &user)
	require.NotEmpty(t, user.Login, "GET /user must report the token's own login, which every repo this leg creates is owned by")
	h.owner = user.Login
	return h
}

func (h *realGitHubHarness) Name() string { return "github" }

func (h *realGitHubHarness) Host(t *testing.T) string {
	t.Helper()
	return "github.com"
}

func (h *realGitHubHarness) Token(t *testing.T, kind TokenKind) string {
	t.Helper()
	switch kind {
	case TokenFull:
		return h.token
	case TokenReadOnly:
		return h.readOnlyToken
	case TokenBogus:
		return githubBogusToken
	}
	t.Fatalf("github harness: unknown token kind %s", kind)
	return ""
}

func (h *realGitHubHarness) Provider(t *testing.T, token string) forge.Provider {
	t.Helper()
	return forge.NewGitHub("github.com", token, h.http, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// SeedRepo creates a fresh, PUBLIC, non-empty repository — see
// TestProviderContract_RealGitHub's doc comment, decision 4, for why
// public rather than private — and registers its deletion so a run of
// this leg does not leak repositories onto the dedicated account
// forever.
func (h *realGitHubHarness) SeedRepo(t *testing.T) Repo {
	t.Helper()
	name := fmt.Sprintf("loam-contract-%03d-%d", h.repos.Add(1), time.Now().UnixNano())
	var created struct {
		DefaultBranch string `json:"default_branch"`
	}
	h.apiDo(t, http.MethodPost, "/user/repos", h.token, map[string]any{
		"name": name, "auto_init": true, "private": false,
	}, http.StatusCreated, &created)
	t.Cleanup(func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, "https://api.github.com/repos/"+h.owner+"/"+name, nil)
		if err != nil {
			t.Logf("github harness cleanup: building delete request for %s: %v", name, err)
			return
		}
		req.Header.Set("Authorization", "token "+h.token)
		resp, err := h.http.Do(req)
		if err != nil {
			t.Logf("github harness cleanup: deleting %s: %v", name, err)
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Logf("github harness cleanup: deleting %s: unexpected status %s (token needs delete_repo scope)", name, resp.Status)
		}
	})
	branch := created.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	path := h.owner + "/" + name
	return Repo{Path: path, GitURL: "https://github.com/" + path + ".git", MainBranch: branch}
}

func (h *realGitHubHarness) MissingRepo(t *testing.T) Repo {
	t.Helper()
	name := fmt.Sprintf("loam-contract-never-created-%03d-%d", h.repos.Add(1), time.Now().UnixNano())
	path := h.owner + "/" + name
	return Repo{Path: path, GitURL: "https://github.com/" + path + ".git", MainBranch: "main"}
}

func (h *realGitHubHarness) MergePR(t *testing.T, repo Repo, prNumber int) {
	t.Helper()
	h.apiDo(t, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo.Path, prNumber), h.token, nil, http.StatusOK, nil)
}

// apiDo issues one authenticated GitHub REST call for HARNESS plumbing
// only (repo creation/deletion, the forge-side merge, and the initial
// /user identity lookup) — none of which is part of forge.Provider, and
// none of which the contract itself asserts through.
func (h *realGitHubHarness) apiDo(t *testing.T, method, path, token string, body any, wantStatus int, out any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "https://api.github.com"+path, reader)
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
	if out != nil && len(payload) > 0 {
		require.NoError(t, json.Unmarshal(payload, out))
	}
}
