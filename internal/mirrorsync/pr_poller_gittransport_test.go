package mirrorsync

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/gittransport"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// This file proves StorePRPoller's branch cleanup against a real git
// subprocess and a real (in-process) fakeforge git smart-HTTP server, the
// same style fetcher_gittransport_test.go uses: no container, no mocked
// git. internal/gittransport's own tests already prove DeleteRemoteRef
// deletes a ref; what is proven here is the composition -- that a merged
// PR makes this poller delete THAT ref and nothing else.

// pollWithCreds embeds user/pass in rawURL so an external verifier can
// reach the fakeforge git endpoint without gittransport's credential
// injection.
func pollWithCreds(t *testing.T, rawURL, user, pass string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	u.User = url.UserPassword(user, pass)
	return u.String()
}

// pollVerificationGit runs a real git command as an external verifier
// would -- never through Transport or the poller -- isolated from the
// host's own git config, and with an explicit identity. The identity is
// not optional: the three isolation vars above it cut this git off from
// every config file that would otherwise supply one, and git's fallback
// guess of user@hostname succeeds on a laptop but fails outright on a CI
// runner ("Please tell me who you are"). Copied from
// internal/gittransport/helpers_test.go's runVerificationGit, which
// carries the full rationale.
func pollVerificationGit(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+home+"/.config",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=loam-test", "GIT_AUTHOR_EMAIL=loam-test@example.invalid",
		"GIT_COMMITTER_NAME=loam-test", "GIT_COMMITTER_EMAIL=loam-test@example.invalid",
	)
	return cmd.CombinedOutput()
}

// TestPollPRsDeletesOnlyTheProposalBranchUpstream is the end-to-end proof
// of the one destructive thing this bead does: a merged PR removes the
// upstream loam/<name> branch the proposal was pushed to, and leaves the
// target branch -- and every other upstream ref -- untouched. Both halves
// matter; a delete that also took out refs/heads/main would be
// unrecoverable, and only the second assertion would catch it.
func TestPollPRsDeletesOnlyTheProposalBranchUpstream(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "pr-poller-cleanup-token"
	srv := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOfURL(t, upstreamURL)
	transport := gittransport.New(&staticCredentialSource{token: token}, fakeforge.NewClient("", ""), testLogger())
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	out, err := pollVerificationGit(t, "init", "--bare", "-q", mirrorDir)
	require.NoErrorf(t, err, "git init --bare: %s", out)
	_, err = transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{branchesRefspec, tagsRefspec})
	require.NoError(t, err)
	mainSHA, err := mirrorRevParse(t, mirrorDir, "refs/heads/main")
	require.NoError(t, err)
	_, err = transport.Push(t.Context(), host, mirrorDir, upstreamURL, mainSHA+":refs/heads/loam/wb-9c2f1a")
	require.NoError(t, err)
	authedURL := pollWithCreds(t, upstreamURL, "any", token)
	out, err = pollVerificationGit(t, "ls-remote", authedURL, "refs/heads/loam/wb-9c2f1a")
	require.NoErrorf(t, err, "ls-remote: %s", out)
	require.Contains(t, string(out), mainSHA, "precondition: the proposal branch must exist upstream before the poll")

	repoID, wbID := uuid.New(), uuid.New()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: repoID, Name: "acme/widgets", ForgeHost: host, UpstreamURL: upstreamURL}, nil
		},
	}
	wb := workBranchFixture("wb-9c2f1a", wbID, 7)
	transitions := &workBranchTerminatorMock{
		CompleteFunc: func(_ context.Context, id uuid.UUID) (workbranchstore.WorkBranch, error) {
			return workbranchstore.WorkBranch{ID: id, State: workbranchstore.StateComplete}, nil
		},
		CloseFunc: func(context.Context, uuid.UUID, string) (workbranchstore.WorkBranch, error) {
			return workbranchstore.WorkBranch{}, nil
		},
	}
	tracker := &pullRequestTrackerMock{
		GetPRStateFunc: func(context.Context, string, int) (string, error) { return prStateMerged, nil },
		ClosePRFunc:    func(context.Context, string, int) error { return nil },
	}
	poller := NewStorePRPoller(dataDir, testLogger(), repos, pollBranchLister(repoID, wb), transitions, tracker, transport)
	require.NoError(t, poller.PollPRs(t.Context(), RepoID("acme/widgets")))

	out, err = pollVerificationGit(t, "ls-remote", authedURL, "refs/heads/loam/wb-9c2f1a")
	require.NoErrorf(t, err, "ls-remote: %s", out)
	assert.Empty(t, strings.TrimSpace(string(out)), "the proposal branch must be gone upstream after its PR merged")
	out, err = pollVerificationGit(t, "ls-remote", authedURL, "refs/heads/main")
	require.NoErrorf(t, err, "ls-remote: %s", out)
	assert.Contains(t, string(out), mainSHA, "cleanup must never touch the target branch")
}
