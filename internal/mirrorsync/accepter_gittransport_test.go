package mirrorsync

import (
	"context"
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

// This file proves StoreProposalAccepter's push leg against a real git
// subprocess and a real (in-process) fakeforge git smart-HTTP server, in
// the same style fetcher_gittransport_test.go and
// pr_poller_gittransport_test.go use: no container, no mocked git. What is
// proven here is the property no mock can prove -- that the refspec this
// engine builds is one REAL git actually refuses to force through when the
// upstream ref has moved somewhere the work branch's history does not
// contain.

// acceptFixture is one prepared repo: a fake forge with an "acme/widgets"
// repo, a bare mirror at production's own derived path, and an
// authenticated transport pointed at both.
type acceptFixture struct {
	srv         *fakeforge.Server
	transport   *gittransport.Transport
	dataDir     string
	mirrorDir   string
	upstreamURL string
	host        string
	authedURL   string
	repoID      uuid.UUID
}

const acceptGitToken = "proposal-accept-token"

// newAcceptFixture seeds the forge, initializes the mirror at
// mirrorpath.Dir(dataDir, repo), and fetches every upstream ref into it --
// the state an enrolled repo is in before any proposal is accepted.
func newAcceptFixture(t *testing.T) acceptFixture {
	t.Helper()
	requireGit(t)
	srv := newFakeForgeServer(t)
	srv.AddToken(acceptGitToken)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOfURL(t, upstreamURL)
	transport := gittransport.New(&staticCredentialSource{token: acceptGitToken}, fakeforge.NewClient("", ""), testLogger())
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	out, err := acceptVerificationGit(t, "init", "--bare", "-q", mirrorDir)
	require.NoErrorf(t, err, "git init --bare: %s", out)
	_, err = transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{allRefsRefspec})
	require.NoError(t, err)
	return acceptFixture{
		srv:         srv,
		transport:   transport,
		dataDir:     dataDir,
		mirrorDir:   mirrorDir,
		upstreamURL: upstreamURL,
		host:        host,
		authedURL:   acceptWithCreds(t, upstreamURL, "any", acceptGitToken),
		repoID:      uuid.New(),
	}
}

// acceptVerificationGit runs a real git command as an external verifier
// would -- never through Transport or the accepter -- isolated from the
// host's own git config, and with an explicit identity. The identity is
// not optional: the isolation vars above it cut this git off from every
// config file that would otherwise supply one, and git's fallback guess of
// user@hostname succeeds on a laptop but fails outright on a CI runner
// ("Please tell me who you are"). Copied from
// internal/gittransport/helpers_test.go's runVerificationGit, which
// carries the full rationale -- including why macOS's SYSTEM gitconfig
// (credential.helper=osxkeychain, which keys by protocol+host while
// ignoring the port) has to be dropped here too.
func acceptVerificationGit(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	home := t.TempDir()
	cmd := acceptGitCmd(t, home, args...)
	return cmd.CombinedOutput()
}

// acceptGitCmd builds the isolated git command acceptVerificationGit and
// acceptGitIn both run. GIT_CONFIG_NOSYSTEM drops macOS's system
// gitconfig (which sets credential.helper=osxkeychain, keyed by
// protocol+host while IGNORING the port -- an ambient cached credential
// there once authenticated a request that was supposed to fail); HOME and
// XDG_CONFIG_HOME are redirected at a fresh temp dir so no user-global
// config is read either; and the four identity variables are supplied
// explicitly because that isolation is exactly what leaves `git commit`
// with no committer otherwise.
func acceptGitCmd(t *testing.T, home string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=loam-test", "GIT_AUTHOR_EMAIL=loam-test@example.invalid",
		"GIT_COMMITTER_NAME=loam-test", "GIT_COMMITTER_EMAIL=loam-test@example.invalid",
	)
	return cmd
}

// acceptWithCreds embeds user/pass in rawURL so an external verifier can
// reach the fakeforge git endpoint without gittransport's own credential
// injection.
func acceptWithCreds(t *testing.T, rawURL, user, pass string) string {
	t.Helper()
	return pollWithCreds(t, rawURL, user, pass)
}

// newRealAccepter builds a StoreProposalAccepter over the fixture's real
// transport and mirror, with mocked stores and a mocked forge REST surface
// (this file is about the git leg; accepter_test.go covers the rest). Both
// forge calls and the recorder are fully configured, so a test asserting
// "CreatePR never ran" fails on a length assertion rather than a panic.
func newRealAccepter(t *testing.T, f acceptFixture, wb workbranchstore.WorkBranch) (*StoreProposalAccepter, *[]createPRCall, *[]recordCall) {
	t.Helper()
	creates, records := new([]createPRCall), new([]recordCall)
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: f.repoID, Name: "acme/widgets", ForgeHost: f.host, UpstreamURL: f.upstreamURL}, nil
		},
	}
	branches := &workBranchByNameLookupMock{
		GetByNameFunc: func(context.Context, uuid.UUID, string) (workbranchstore.WorkBranch, error) { return wb, nil },
	}
	prForge := &pullRequestOpenerMock{
		CreatePRFunc: func(_ context.Context, repo, head, target, title, description string) (string, int, error) {
			*creates = append(*creates, createPRCall{repo: repo, head: head, target: target, title: title, description: description})
			return "https://forge.example.com/acme/widgets/pulls/1", 1, nil
		},
		FindOpenPRFunc: func(context.Context, string, string, string) (string, int, bool, error) {
			return "", 0, false, nil
		},
	}
	recorder := &workBranchPRRecorderMock{
		RecordUpstreamPRFunc: func(_ context.Context, id uuid.UUID, prURL string, number int32) (workbranchstore.WorkBranch, error) {
			*records = append(*records, recordCall{id: id, prURL: prURL, number: number})
			return wb, nil
		},
	}
	return NewStoreProposalAccepter(f.dataDir, testLogger(), true, repos, branches, recorder, prForge, f.transport), creates, records
}

// acceptWorkBranch builds a reviewed, unconflicted work branch row for the
// real-git tests.
func acceptWorkBranch(name string) workbranchstore.WorkBranch {
	title, description := "Real push", "A real push over the real transport."
	return workbranchstore.WorkBranch{
		ID:          uuid.New(),
		Name:        name,
		Target:      "main",
		Title:       &title,
		Description: &description,
		State:       workbranchstore.StateReviewed,
		Conflict:    workbranchstore.ConflictNone,
	}
}

// TestAcceptProposal_CreatesTheNamespacedUpstreamBranch is the create half
// of "fast-forward-or-create": a first accept, against an upstream that
// has no loam/ ref at all, lands the work branch's exact tip at
// refs/heads/loam/<name> and touches nothing else.
func TestAcceptProposal_CreatesTheNamespacedUpstreamBranch(t *testing.T) {
	t.Parallel()
	f := newAcceptFixture(t)
	mainSHA, err := mirrorRevParse(t, f.mirrorDir, "refs/heads/main")
	require.NoError(t, err)
	out, err := acceptVerificationGit(t, "--git-dir="+f.mirrorDir, "update-ref", "refs/heads/wb-9c2f1a", mainSHA)
	require.NoErrorf(t, err, "seeding the mirror's work-branch ref: %s", out)
	out, err = acceptVerificationGit(t, "ls-remote", f.authedURL, "refs/heads/loam/wb-9c2f1a")
	require.NoErrorf(t, err, "ls-remote: %s", out)
	require.Empty(t, strings.TrimSpace(string(out)), "precondition: upstream must have no loam/ branch before the first accept")

	accepter, creates, records := newRealAccepter(t, f, acceptWorkBranch("wb-9c2f1a"))
	result, err := accepter.AcceptProposal(t.Context(), RepoID("acme/widgets"), "wb-9c2f1a")
	require.NoError(t, err)
	assert.Equal(t, "loam/wb-9c2f1a", result.UpstreamBranch)
	assert.Len(t, *creates, 1)
	assert.Len(t, *records, 1)

	out, err = acceptVerificationGit(t, "ls-remote", f.authedURL, "refs/heads/loam/wb-9c2f1a")
	require.NoErrorf(t, err, "ls-remote: %s", out)
	assert.Contains(t, string(out), mainSHA, "the work branch tip must be at loam/wb-9c2f1a upstream")
	out, err = acceptVerificationGit(t, "ls-remote", f.authedURL, "refs/heads/main")
	require.NoErrorf(t, err, "ls-remote: %s", out)
	assert.Contains(t, string(out), mainSHA, "accepting must never touch the target branch")
}

// TestAcceptProposal_RefusesANonFastForwardPush is the property that
// cannot be faked: upstream's loam/ branch has moved to a commit the
// mirror's work branch does not contain, so the push this engine issues is
// a rewind. Real git refuses it (it carries no '+', and gittransport adds
// no --force), the upstream ref is left exactly where it was, and no PR is
// opened or recorded off the back of a push that never landed.
//
// If the engine ever gained a force -- a '+' in the refspec, a --force in
// the transport, any route at all -- this test goes green on the push and
// then fails on the ls-remote assertion, because the upstream commit would
// be gone.
func TestAcceptProposal_RefusesANonFastForwardPush(t *testing.T) {
	t.Parallel()
	f := newAcceptFixture(t)
	mainSHA, err := mirrorRevParse(t, f.mirrorDir, "refs/heads/main")
	require.NoError(t, err)
	// The mirror's work branch sits at main's tip; upstream's loam/ branch
	// is then advanced one commit past it, so pushing the mirror's tip
	// would DISCARD that upstream commit.
	out, err := acceptVerificationGit(t, "--git-dir="+f.mirrorDir, "update-ref", "refs/heads/wb-9c2f1a", mainSHA)
	require.NoErrorf(t, err, "seeding the mirror's work-branch ref: %s", out)
	_, err = f.transport.Push(t.Context(), f.host, f.mirrorDir, f.upstreamURL, "refs/heads/wb-9c2f1a:refs/heads/loam/wb-9c2f1a")
	require.NoError(t, err)
	require.NoError(t, f.srv.AdvanceBranch(t.Context(), "acme/widgets", "loam/wb-9c2f1a", fakeforge.AdvanceOptions{Message: "upstream work nobody asked to destroy"}))
	out, err = acceptVerificationGit(t, "ls-remote", f.authedURL, "refs/heads/loam/wb-9c2f1a")
	require.NoErrorf(t, err, "ls-remote: %s", out)
	upstreamSHA := strings.Fields(string(out))[0]
	require.NotEqual(t, mainSHA, upstreamSHA, "precondition: upstream must be ahead of the mirror's work branch")

	accepter, creates, records := newRealAccepter(t, f, acceptWorkBranch("wb-9c2f1a"))
	_, err = accepter.AcceptProposal(t.Context(), RepoID("acme/widgets"), "wb-9c2f1a")
	require.Error(t, err, "a non-fast-forward push must be refused, not forced through")

	out, err = acceptVerificationGit(t, "ls-remote", f.authedURL, "refs/heads/loam/wb-9c2f1a")
	require.NoErrorf(t, err, "ls-remote: %s", out)
	assert.Contains(t, string(out), upstreamSHA, "the refused push must leave the upstream commit exactly where it was")
	assert.Empty(t, *creates, "a refused push must not open a PR")
	assert.Empty(t, *records, "a refused push must not record a PR")
}

// TestAcceptProposal_FastForwardsAnExistingUpstreamBranch is the
// fast-forward half: a re-accept after a catch-up moves upstream's loam/
// branch forward to the new work-branch tip, with the same non-forced
// refspec that the rewind above was refused for.
func TestAcceptProposal_FastForwardsAnExistingUpstreamBranch(t *testing.T) {
	t.Parallel()
	f := newAcceptFixture(t)
	mainSHA, err := mirrorRevParse(t, f.mirrorDir, "refs/heads/main")
	require.NoError(t, err)
	out, err := acceptVerificationGit(t, "--git-dir="+f.mirrorDir, "update-ref", "refs/heads/wb-9c2f1a", mainSHA)
	require.NoErrorf(t, err, "seeding the mirror's work-branch ref: %s", out)
	_, err = f.transport.Push(t.Context(), f.host, f.mirrorDir, f.upstreamURL, "refs/heads/wb-9c2f1a:refs/heads/loam/wb-9c2f1a")
	require.NoError(t, err)
	// The agent pushes one more commit onto the work branch: the mirror is
	// now strictly ahead of upstream, so the re-accept is a fast-forward.
	caughtUpSHA := acceptCommitIntoMirror(t, f.mirrorDir, "wb-9c2f1a")
	require.NotEqual(t, mainSHA, caughtUpSHA)

	wb := acceptWorkBranch("wb-9c2f1a")
	number, prURL := int32(3), "https://forge.example.com/acme/widgets/pulls/3"
	wb.UpstreamPRNumber, wb.UpstreamPRURL = &number, &prURL
	accepter, creates, records := newRealAccepter(t, f, wb)
	result, err := accepter.AcceptProposal(t.Context(), RepoID("acme/widgets"), "wb-9c2f1a")
	require.NoError(t, err)
	assert.False(t, result.CreatedPR)
	assert.Equal(t, 3, result.PRNumber)
	assert.Empty(t, *creates, "a re-accept must not open a second PR")
	assert.Empty(t, *records, "a re-accept must not re-record the PR")

	out, err = acceptVerificationGit(t, "ls-remote", f.authedURL, "refs/heads/loam/wb-9c2f1a")
	require.NoErrorf(t, err, "ls-remote: %s", out)
	assert.Contains(t, string(out), caughtUpSHA, "the re-accept must fast-forward the upstream branch to the new tip")
}

// acceptCommitIntoMirror writes one commit onto branch inside the bare
// mirror by cloning it, committing, and pushing back -- the only way to
// move a bare repo's ref to a NEW commit without hand-rolling plumbing.
// Returns the new tip SHA.
func acceptCommitIntoMirror(t *testing.T, mirrorDir, branch string) string {
	t.Helper()
	work := t.TempDir()
	clone := filepath.Join(work, "clone")
	out, err := acceptVerificationGit(t, "clone", "--quiet", mirrorDir, clone)
	require.NoErrorf(t, err, "cloning the mirror: %s", out)
	out, err = acceptGitIn(t, clone, "checkout", "--quiet", "-B", branch, "origin/"+branch)
	require.NoErrorf(t, err, "checking out %s: %s", branch, out)
	require.NoError(t, os.WriteFile(filepath.Join(clone, "CATCHUP.txt"), []byte("caught up\n"), 0o644))
	out, err = acceptGitIn(t, clone, "add", "CATCHUP.txt")
	require.NoErrorf(t, err, "git add: %s", out)
	out, err = acceptGitIn(t, clone, "commit", "--quiet", "-m", "acceptance: catch-up commit")
	require.NoErrorf(t, err, "git commit: %s", out)
	out, err = acceptGitIn(t, clone, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	require.NoErrorf(t, err, "pushing into the mirror: %s", out)
	sha, err := mirrorRevParse(t, mirrorDir, "refs/heads/"+branch)
	require.NoError(t, err)
	return sha
}

// acceptGitIn runs acceptVerificationGit's isolated git inside dir.
func acceptGitIn(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	home := t.TempDir()
	cmd := acceptGitCmd(t, home, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
