package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/refnames"
)

// --- splitRepo / cloneURL: pure helpers, no collaborators needed ---

func TestSplitRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		repo      string
		wantGroup string
		wantName  string
		wantOK    bool
	}{
		{"group and name", "bobcob7/doc-server", "bobcob7", "doc-server", true},
		{"nested group", "acme/sub/doc-server", "acme/sub", "doc-server", true},
		{"no slash", "doc-server", "", "", false},
		{"leading slash only", "/doc-server", "", "", false},
		{"trailing slash", "bobcob7/", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			group, name, ok := splitRepo(tt.repo)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantGroup, group)
				assert.Equal(t, tt.wantName, name)
			}
		})
	}
}

func TestCloneURL_ComposesGitPath(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "https://loam.example/git/bobcob7/doc-server.git", cloneURL("https://loam.example", "bobcob7", "doc-server"))
}

func TestCloneURL_TrimsTrailingSlashOnServerURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "https://loam.example/git/bobcob7/doc-server.git", cloneURL("https://loam.example/", "bobcob7", "doc-server"))
}

// --- runCloneCommand: full command orchestration, collaborators mocked ---

// cloneTestConfig builds a ConfigMock carrying a fixed agent identity and
// server URL — the shape every runCloneCommand test needs.
func cloneTestConfig(serverURL string) *ConfigMock {
	return &ConfigMock{
		ServerURLFunc:  func() string { return serverURL },
		AgentNameFunc:  func() string { return "grace-hopper" },
		AgentIDFunc:    func() string { return "3" },
		AgentRoleFunc:  func() string { return "author" },
		IdentifierFunc: func() string { return "grace-hopper-3-author" },
	}
}

// cloneTestDeps wires a Deps for runCloneCommand tests: getRepo governs the
// RepoService.GetRepo response, cloner is the gitCloner double, and encoded
// captures whatever the handler encodes on success.
func cloneTestDeps(cfg Config, getRepo func(context.Context, *connect.Request[loamv1.GetRepoRequest]) (*connect.Response[loamv1.GetRepoResponse], error), cloner gitCloner, encoded *any) *Deps {
	return cloneTestDepsWithRefs(cfg, getRepo, cloner, okGetWorkBranch("main"), okCloneRefs(), encoded)
}

// cloneTestDepsWithRefs is cloneTestDeps with the two collaborators
// loam-hwru added to `clone` -- WorkBranchService.GetWorkBranch (which
// names the branch's target) and gitRefs (which fetches that target and
// resolves the range) -- under the test's own control.
func cloneTestDepsWithRefs(cfg Config, getRepo func(context.Context, *connect.Request[loamv1.GetRepoRequest]) (*connect.Response[loamv1.GetRepoResponse], error), cloner gitCloner, getWorkBranch func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error), refs gitRefs, encoded *any) *Deps {
	repoClient := &RepoClientMock{GetRepoFunc: getRepo}
	workBranchClient := &WorkBranchClientMock{GetWorkBranchFunc: getWorkBranch}
	connectClient := &ConnectClientMock{
		RepoFunc:       func() RepoClient { return repoClient },
		WorkBranchFunc: func() WorkBranchClient { return workBranchClient },
	}
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { *encoded = v; return nil }}
	return NewDeps(testLogger(), cfg, encoder, newErrorMapper(), &WorkspaceResolverMock{}, connectClient, cloner, nil, refs)
}

// okGetWorkBranch stubs GetWorkBranch with a work branch whose target is
// target -- the one field `clone` reads from it.
func okGetWorkBranch(target string) func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
	return func(_ context.Context, req *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
		wb := &loamv1.WorkBranch{Repo: req.Msg.GetRepo(), Name: req.Msg.GetWorkBranch(), Target: target}
		return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: wb}), nil
	}
}

// cloneTestBaseSHA and cloneTestHeadSHA are the two commits okCloneRefs
// reports, distinct from each other so a test cannot pass by conflating
// "the merge base" with "HEAD".
const (
	cloneTestBaseSHA = "1111111111111111111111111111111111111111"
	cloneTestHeadSHA = "2222222222222222222222222222222222222222"
)

// okCloneRefs is a gitRefs double for which every operation `clone` performs
// succeeds.
func okCloneRefs() *gitRefsMock {
	return &gitRefsMock{
		FetchFunc:     func(context.Context, string, ...string) error { return nil },
		RevParseFunc:  func(context.Context, string, string) (string, error) { return cloneTestHeadSHA, nil },
		MergeBaseFunc: func(context.Context, string, string, string) (string, error) { return cloneTestBaseSHA, nil },
	}
}

func okGetRepo(repo string) func(context.Context, *connect.Request[loamv1.GetRepoRequest]) (*connect.Response[loamv1.GetRepoResponse], error) {
	return func(_ context.Context, req *connect.Request[loamv1.GetRepoRequest]) (*connect.Response[loamv1.GetRepoResponse], error) {
		if req.Msg.Repo != repo {
			return nil, fmt.Errorf("unexpected GetRepo request: got %q, want %q", req.Msg.Repo, repo)
		}
		return connect.NewResponse(&loamv1.GetRepoResponse{Repo: repo}), nil
	}
}

// TestRunCloneCommand_Success proves the full happy path: GetRepo confirms
// enrollment, Clone runs against the composed URL/branch/destination
// carrying all three Loam-Agent-* headers (matching connect.go's header
// constants exactly, the same ones httpauth's GitIdentity wrapper
// requires) so the clone's OWN initial fetch is authorized, the clone is
// then bootstrapped with the agent's author identity, and the encoder
// receives {repo, path, branch} (see docs/cli-spec.md -> clone).
func TestRunCloneCommand_Success(t *testing.T) {
	t.Parallel()
	cfg := cloneTestConfig("https://loam.example")
	cloner := &gitClonerMock{
		CloneFunc:        func(context.Context, string, string, string, []string) error { return nil },
		SetConfigFunc:    func(context.Context, string, string, string) error { return nil },
		AddConfigFunc:    func(context.Context, string, string, string) error { return nil },
		RenameBranchFunc: func(context.Context, string, string, string) error { return nil },
	}
	var encoded any
	deps := cloneTestDeps(cfg, okGetRepo("bobcob7/doc-server"), cloner, &encoded)

	err := runCloneCommand(t.Context(), deps, "bobcob7/doc-server", "wb-9c2f1a")
	require.NoError(t, err)

	require.Len(t, cloner.CloneCalls(), 1)
	cloneCall := cloner.CloneCalls()[0]
	assert.Equal(t, "https://loam.example/git/bobcob7/doc-server.git", cloneCall.URL)
	assert.Equal(t, "loam-reserved/wb-9c2f1a", cloneCall.Branch, "a work branch is not among the repo's target branches, so it is cloned at its reserved ref path")
	assert.Equal(t, "./doc-server", cloneCall.Dest)
	assert.Equal(t, []string{
		"Loam-Agent-Name: grace-hopper",
		"Loam-Agent-Id: 3",
		"Loam-Agent-Role: author",
	}, cloneCall.Headers, "Clone itself must carry all three identity headers -- writing them into the clone's config only after Clone returns would be too late for its own initial fetch")

	require.Len(t, cloner.SetConfigCalls(), 3, "user.name, user.email, and remote.origin.push")
	assert.Equal(t, "user.name", cloner.SetConfigCalls()[0].Key)
	assert.Equal(t, "grace-hopper", cloner.SetConfigCalls()[0].Value)
	assert.Equal(t, "user.email", cloner.SetConfigCalls()[1].Key)
	assert.Equal(t, "grace-hopper-3-author@loam", cloner.SetConfigCalls()[1].Value)
	// remote.origin.push is a SetConfig (single-valued, overwrite) while
	// remote.origin.fetch is an AddConfig (multi-valued, append): the
	// clone's own --single-branch refspec already occupies the latter, and
	// overwriting it would leave the clone unable to fetch the branch it
	// was cloned at.
	assert.Equal(t, "remote.origin.push", cloner.SetConfigCalls()[2].Key)
	assert.Equal(t, "refs/heads/wb-*:refs/heads/loam-reserved/wb-*", cloner.SetConfigCalls()[2].Value)
	require.Len(t, cloner.AddConfigCalls(), 2, "remote.origin.fetch, appended never replaced -- once for the reserved namespace, once for the target branch")
	assert.Equal(t, "remote.origin.fetch", cloner.AddConfigCalls()[0].Key)
	assert.Equal(t, "+refs/heads/loam-reserved/*:refs/remotes/origin/*", cloner.AddConfigCalls()[0].Value)
	assert.Equal(t, "remote.origin.fetch", cloner.AddConfigCalls()[1].Key)
	assert.Equal(t, "+refs/heads/main:refs/remotes/origin/main", cloner.AddConfigCalls()[1].Value, "the target's refspec must be PERSISTED, not only fetched once, or origin/main silently ages as main moves")

	require.Len(t, cloner.RenameBranchCalls(), 1, "the cloned branch must be renamed back to its bare name")
	assert.Equal(t, "loam-reserved/wb-9c2f1a", cloner.RenameBranchCalls()[0].From)
	assert.Equal(t, "wb-9c2f1a", cloner.RenameBranchCalls()[0].To)

	out, ok := encoded.(cloneOutput)
	require.True(t, ok, "clone must encode a cloneOutput")
	assert.Equal(t, cloneOutput{
		Repo:    "bobcob7/doc-server",
		Path:    "./doc-server",
		Branch:  "wb-9c2f1a",
		Target:  "main",
		BaseSHA: cloneTestBaseSHA,
		HeadSHA: cloneTestHeadSHA,
	}, out)
}

// TestRunCloneCommand_FetchesTheTargetBranchIntoTheClone is loam-hwru's
// primary fix, asserted on the collaborator rather than on the output: the
// clone must actively FETCH refs/heads/<target> into
// refs/remotes/origin/<target>. Asserting only that base_sha came back
// would not distinguish "the target ref is in the clone" from "the mock
// answered a merge-base question" -- so this pins the fetch itself, and
// TestExecGitRefs_Fetch_MakesTheTargetRefPresentInASingleBranchClone below
// pins the same thing against a REAL git.
func TestRunCloneCommand_FetchesTheTargetBranchIntoTheClone(t *testing.T) {
	t.Parallel()
	cloner := okCloner()
	refs := okCloneRefs()
	var encoded any
	deps := cloneTestDepsWithRefs(cloneTestConfig("https://loam.example"), okGetRepo("bobcob7/doc-server"), cloner, okGetWorkBranch("release-2"), refs, &encoded)

	require.NoError(t, runCloneCommand(t.Context(), deps, "bobcob7/doc-server", "wb-9c2f1a"))

	require.Len(t, refs.FetchCalls(), 1, "the target branch must be fetched, not merely configured")
	assert.Equal(t, "./doc-server", refs.FetchCalls()[0].Dest)
	assert.Equal(t, []string{"+refs/heads/release-2:refs/remotes/origin/release-2"}, refs.FetchCalls()[0].Refspecs)
	require.Len(t, refs.MergeBaseCalls(), 1)
	assert.Equal(t, "refs/remotes/origin/release-2", refs.MergeBaseCalls()[0].A)
	assert.Equal(t, "HEAD", refs.MergeBaseCalls()[0].B, "base_sha must be the MERGE BASE, not the target's tip")
}

// TestRunCloneCommand_TargetFetchFailure_FailsTheClone pins that a clone
// which cannot obtain its base ref is an ERROR, not a clone that quietly
// lacks one. The whole bead exists because the missing ref was silent.
func TestRunCloneCommand_TargetFetchFailure_FailsTheClone(t *testing.T) {
	t.Parallel()
	refs := okCloneRefs()
	refs.FetchFunc = func(context.Context, string, ...string) error {
		return errors.New("could not read from remote repository")
	}
	var encoded any
	deps := cloneTestDepsWithRefs(cloneTestConfig("https://loam.example"), okGetRepo("bobcob7/doc-server"), okCloner(), okGetWorkBranch("main"), refs, &encoded)

	err := runCloneCommand(t.Context(), deps, "bobcob7/doc-server", "wb-9c2f1a")
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded, "a clone missing its base ref must not report success")
}

// okCloner is a gitCloner double for which every bootstrap step succeeds.
func okCloner() *gitClonerMock {
	return &gitClonerMock{
		CloneFunc:        func(context.Context, string, string, string, []string) error { return nil },
		SetConfigFunc:    func(context.Context, string, string, string) error { return nil },
		AddConfigFunc:    func(context.Context, string, string, string) error { return nil },
		RenameBranchFunc: func(context.Context, string, string, string) error { return nil },
	}
}

// TestRunCloneCommand_TargetBranch_ClonedAtItsOwnRefAndNeverRenamed is the
// other half of cloneBranchFor: a branch the repo reports as a TARGET is a
// mirrored ref, so it is cloned exactly as named and no rename happens.
// Without this, a change that sent every branch through the reserved path
// would still pass the work-branch test above.
func TestRunCloneCommand_TargetBranch_ClonedAtItsOwnRefAndNeverRenamed(t *testing.T) {
	t.Parallel()
	cfg := cloneTestConfig("https://loam.example")
	cloner := &gitClonerMock{
		CloneFunc:        func(context.Context, string, string, string, []string) error { return nil },
		SetConfigFunc:    func(context.Context, string, string, string) error { return nil },
		AddConfigFunc:    func(context.Context, string, string, string) error { return nil },
		RenameBranchFunc: func(context.Context, string, string, string) error { return nil },
	}
	getRepo := func(_ context.Context, req *connect.Request[loamv1.GetRepoRequest]) (*connect.Response[loamv1.GetRepoResponse], error) {
		return connect.NewResponse(&loamv1.GetRepoResponse{Repo: req.Msg.Repo, TargetBranches: []string{"main", "release"}}), nil
	}
	var encoded any
	deps := cloneTestDeps(cfg, getRepo, cloner, &encoded)

	require.NoError(t, runCloneCommand(t.Context(), deps, "bobcob7/doc-server", "main"))

	require.Len(t, cloner.CloneCalls(), 1)
	assert.Equal(t, "main", cloner.CloneCalls()[0].Branch)
	assert.Empty(t, cloner.RenameBranchCalls(), "a target branch is already at the ref it was cloned from")
}

// TestRunCloneCommand_RepoNotEnrolled_ExitsThree proves the bead's "exit 3
// unenrolled repo": a NotFound from RepoService.GetRepo classifies to exit
// 3 via the standard Connect error mapping, and clone never shells out to
// git for a repo that was never confirmed enrolled.
func TestRunCloneCommand_RepoNotEnrolled_ExitsThree(t *testing.T) {
	t.Parallel()
	cfg := cloneTestConfig("https://loam.example")
	cloneCalled := false
	cloner := &gitClonerMock{
		// SetConfig is a harmless no-op here, deliberately: if a regression
		// ever lets Clone run despite an unenrolled repo, this test must
		// still fail on the cloneCalled assertion below, not on an
		// unrelated nil-func panic one call deeper into bootstrap.
		CloneFunc:        func(context.Context, string, string, string, []string) error { cloneCalled = true; return nil },
		SetConfigFunc:    func(context.Context, string, string, string) error { return nil },
		AddConfigFunc:    func(context.Context, string, string, string) error { return nil },
		RenameBranchFunc: func(context.Context, string, string, string) error { return nil },
	}
	notFound := func(context.Context, *connect.Request[loamv1.GetRepoRequest]) (*connect.Response[loamv1.GetRepoResponse], error) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("repo bobcob7/doc-server is not enrolled"))
	}
	var encoded any
	deps := cloneTestDeps(cfg, notFound, cloner, &encoded)

	err := runCloneCommand(t.Context(), deps, "bobcob7/doc-server", "wb-9c2f1a")
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.False(t, cloneCalled, "clone must not shell out to git for an unenrolled repo")
}

// TestRunCloneCommand_BranchMissing_ExitsTwo proves the bead's "exit 2
// missing branch": once enrollment is confirmed, a git-clone failure (the
// shape a missing remote branch takes) maps to exit 2, and bootstrap never
// runs against a clone that was never made.
func TestRunCloneCommand_BranchMissing_ExitsTwo(t *testing.T) {
	t.Parallel()
	cfg := cloneTestConfig("https://loam.example")
	bootstrapped := false
	cloner := &gitClonerMock{
		CloneFunc: func(context.Context, string, string, string, []string) error {
			return errors.New("exit status 128: fatal: Remote branch wb-missing not found in upstream origin")
		},
		SetConfigFunc:    func(context.Context, string, string, string) error { bootstrapped = true; return nil },
		AddConfigFunc:    func(context.Context, string, string, string) error { bootstrapped = true; return nil },
		RenameBranchFunc: func(context.Context, string, string, string) error { return nil },
	}
	var encoded any
	deps := cloneTestDeps(cfg, okGetRepo("bobcob7/doc-server"), cloner, &encoded)

	err := runCloneCommand(t.Context(), deps, "bobcob7/doc-server", "wb-missing")
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.False(t, bootstrapped, "identity must not be bootstrapped into a clone that was never made")
	assert.Nil(t, encoded, "a failed clone must not encode success output")
}

// TestRunCloneCommand_MalformedRepo_ExitsTwo proves a repo argument with no
// "/group/" shape is a usage error (exit 2) caught before any RPC or git
// invocation, rather than reaching the server with an unparseable repo. The
// cloner mock is a full no-op (not bare &gitClonerMock{}), deliberately: if
// a regression ever drops the splitRepo guard, Clone/SetConfig would run
// with a zero-value group ("splitRepo failed" shape), and this test must
// still fail on the getRepoCalled/exit-code assertions below rather than on
// an unrelated nil-func panic from an incomplete mock.
func TestRunCloneCommand_MalformedRepo_ExitsTwo(t *testing.T) {
	t.Parallel()
	cfg := cloneTestConfig("https://loam.example")
	getRepoCalled := false
	getRepo := func(context.Context, *connect.Request[loamv1.GetRepoRequest]) (*connect.Response[loamv1.GetRepoResponse], error) {
		getRepoCalled = true
		return connect.NewResponse(&loamv1.GetRepoResponse{}), nil
	}
	cloner := &gitClonerMock{
		CloneFunc:        func(context.Context, string, string, string, []string) error { return nil },
		SetConfigFunc:    func(context.Context, string, string, string) error { return nil },
		AddConfigFunc:    func(context.Context, string, string, string) error { return nil },
		RenameBranchFunc: func(context.Context, string, string, string) error { return nil },
	}
	var encoded any
	deps := cloneTestDeps(cfg, getRepo, cloner, &encoded)

	err := runCloneCommand(t.Context(), deps, "doc-server", "wb-9c2f1a")
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.False(t, getRepoCalled, "a malformed repo must never reach the server")
	assert.Empty(t, cloner.CloneCalls(), "a malformed repo must never reach git either")
}

// TestRunCloneCommand_BootstrapFailure_PropagatesAsError proves a git-config
// write failure after a successful clone is reported as an error (rather
// than silently emitting success output over a half-bootstrapped clone).
func TestRunCloneCommand_BootstrapFailure_PropagatesAsError(t *testing.T) {
	t.Parallel()
	cfg := cloneTestConfig("https://loam.example")
	cloner := &gitClonerMock{
		CloneFunc:        func(context.Context, string, string, string, []string) error { return nil },
		AddConfigFunc:    func(context.Context, string, string, string) error { return nil },
		RenameBranchFunc: func(context.Context, string, string, string) error { return nil },
		SetConfigFunc: func(context.Context, string, string, string) error {
			return errors.New("git config failed: permission denied")
		},
	}
	var encoded any
	deps := cloneTestDeps(cfg, okGetRepo("bobcob7/doc-server"), cloner, &encoded)

	err := runCloneCommand(t.Context(), deps, "bobcob7/doc-server", "wb-9c2f1a")
	require.Error(t, err)
	assert.Nil(t, encoded, "a failed bootstrap must not encode success output")
}

// TestRouterDispatch_Clone_ReachesRealHandler proves the router dispatches
// "clone" through to the real runCloneCommand handler (not the old
// errNotImplemented stub, and not a routing usageError): with every
// collaborator wired to succeed, dispatching "clone" end to end through the
// real command tree returns nil, exactly as runCloneCommand's own success
// test does when called directly.
func TestRouterDispatch_Clone_ReachesRealHandler(t *testing.T) {
	t.Parallel()
	cfg := cloneTestConfig("https://loam.example")
	cloner := &gitClonerMock{
		CloneFunc:        func(context.Context, string, string, string, []string) error { return nil },
		SetConfigFunc:    func(context.Context, string, string, string) error { return nil },
		AddConfigFunc:    func(context.Context, string, string, string) error { return nil },
		RenameBranchFunc: func(context.Context, string, string, string) error { return nil },
	}
	var encoded any
	deps := cloneTestDeps(cfg, okGetRepo("acme/repo"), cloner, &encoded)
	router := NewRouter(deps)

	err := router.Dispatch(t.Context(), []string{"clone", "acme/repo", "wb-1"})
	require.NoError(t, err)
	require.Len(t, cloner.CloneCalls(), 1, "dispatch must reach the real handler, which shells out to git exactly once")
}

// --- execGitCloner: real git subprocess behavior ---
//
// These exercise the actual git binary -- most against a local bare
// repository over a plain filesystem path (equivalent to file://), one
// against a real HTTP server -- proving what a mocked gitCloner cannot
// prove on its own: single-branch clone genuinely fetches only the named
// branch, the identity headers reach git's OWN initial fetch (not just
// later operations), and every config write lands in the clone's real
// .git/config where plain git push/commit would read it from.

// mustRunGit runs git with args in dir, failing t on error. Deliberately
// independent of runGitCommand (the code under test) so a bug there cannot
// hide from these tests.
func mustRunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v (dir=%q): %s", args, dir, out)
	return strings.TrimSpace(string(out))
}

// newBareRepoWithTwoBranches builds a bare repo (suitable as a clone
// source) with two branches, "main" and "wb-1", each with one commit not
// reachable from the other — a shape that makes single-branch clone's
// effect observable: a full clone would carry both, a single-branch clone
// of "wb-1" must carry only it.
func newBareRepoWithTwoBranches(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	mustRunGit(t, src, "init", "--quiet", "-b", "main")
	env := []string{"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com"}
	cmd := exec.Command("git", "commit", "--quiet", "--allow-empty", "-m", "main commit")
	cmd.Dir = src
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1"), env...)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "commit on main: %s", out)
	mustRunGit(t, src, "checkout", "--quiet", "-b", "wb-1")
	cmd = exec.Command("git", "commit", "--quiet", "--allow-empty", "-m", "wb-1 commit")
	cmd.Dir = src
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1"), env...)
	out, err = cmd.CombinedOutput()
	require.NoErrorf(t, err, "commit on wb-1: %s", out)
	mustRunGit(t, src, "checkout", "--quiet", "main")
	bare := filepath.Join(t.TempDir(), "upstream.git")
	mustRunGit(t, "", "clone", "--quiet", "--bare", src, bare)
	return bare
}

// TestExecGitCloner_Clone_SingleBranch_FetchesOnlyTheNamedBranch proves the
// bead's "single branch matters too" requirement against a real git
// process: cloning "wb-1" out of a two-branch upstream leaves the clone
// with no knowledge of "main" at all -- not merely checked out elsewhere,
// genuinely never fetched.
func TestExecGitCloner_Clone_SingleBranch_FetchesOnlyTheNamedBranch(t *testing.T) {
	t.Parallel()
	upstream := newBareRepoWithTwoBranches(t)
	dest := filepath.Join(t.TempDir(), "doc-server")
	cloner := execGitCloner{}

	err := cloner.Clone(t.Context(), upstream, "wb-1", dest, nil)
	require.NoError(t, err)

	branch := mustRunGit(t, dest, "symbolic-ref", "--short", "HEAD")
	assert.Equal(t, "wb-1", branch, "single-branch clone must check out the requested branch")

	remoteBranches := mustRunGit(t, dest, "branch", "-r")
	assert.Contains(t, remoteBranches, "origin/wb-1")
	assert.NotContains(t, remoteBranches, "origin/main", "single-branch clone must not have fetched the other branch at all")

	remotes := mustRunGit(t, dest, "remote")
	assert.Equal(t, "origin", remotes, "the clone's only remote must be the server endpoint")
}

// TestExecGitCloner_Clone_HeadersPersistIntoRealGitConfig proves the
// identity headers passed to Clone (as clone-time --config arguments, not a
// separate write afterward) land in the clone's actual .git/config, all
// three, in order -- exactly where plain `git push`/`git fetch` read
// http.extraHeader from -- not merely that the subprocess exited 0.
func TestExecGitCloner_Clone_HeadersPersistIntoRealGitConfig(t *testing.T) {
	t.Parallel()
	upstream := newBareRepoWithTwoBranches(t)
	dest := filepath.Join(t.TempDir(), "doc-server")
	cloner := execGitCloner{}
	headers := []string{
		"Loam-Agent-Name: grace-hopper",
		"Loam-Agent-Id: 3",
		"Loam-Agent-Role: author",
	}

	require.NoError(t, cloner.Clone(t.Context(), upstream, "main", dest, headers))

	got := mustRunGit(t, dest, "config", "--get-all", "http.extraHeader")
	assert.Equal(t, headers, strings.Split(got, "\n"), "all three headers must land in the clone's config exactly once each, in order, from Clone itself")
}

// TestExecGitCloner_SetConfig_LandsInRealGitConfig proves SetConfig writes
// into the clone's actual .git/config, exactly where plain `git commit`
// reads user.name/user.email from.
func TestExecGitCloner_SetConfig_LandsInRealGitConfig(t *testing.T) {
	t.Parallel()
	upstream := newBareRepoWithTwoBranches(t)
	dest := filepath.Join(t.TempDir(), "doc-server")
	cloner := execGitCloner{}
	require.NoError(t, cloner.Clone(t.Context(), upstream, "main", dest, nil))

	require.NoError(t, cloner.SetConfig(t.Context(), dest, "user.name", "grace-hopper"))
	require.NoError(t, cloner.SetConfig(t.Context(), dest, "user.email", "grace-hopper-3-author@loam"))

	assert.Equal(t, "grace-hopper", mustRunGit(t, dest, "config", "user.name"))
	assert.Equal(t, "grace-hopper-3-author@loam", mustRunGit(t, dest, "config", "user.email"))
}

// TestExecGitCloner_Clone_SendsIdentityHeadersOnTheInitialFetch is the
// direct proof for MUST-FIX 1's failure mode: it captures the request
// headers git itself sends on the very FIRST request a clone makes (the
// upload-pack info/refs GET, before any destination directory or config
// exists), against a bare HTTP handler standing in for /git/* -- exactly
// the request httpauth.Auth.GitIdentity would 403 if it arrived without
// all three Loam-Agent-* headers. The server always answers 404 (it isn't
// a real smart-HTTP backend), so `git clone` itself fails -- what matters
// is what arrived before that.
func TestExecGitCloner_Clone_SendsIdentityHeadersOnTheInitialFetch(t *testing.T) {
	t.Parallel()
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "doc-server")
	cloner := execGitCloner{}
	headers := []string{
		"Loam-Agent-Name: grace-hopper",
		"Loam-Agent-Id: 3",
		"Loam-Agent-Role: author",
	}

	err := cloner.Clone(t.Context(), srv.URL+"/git/bobcob7/doc-server.git", "wb-1", dest, headers)
	require.Error(t, err, "the stub server is not a real smart-HTTP backend, so the clone itself must fail")
	require.NotNil(t, captured, "git must have sent at least one request before failing")
	assert.Equal(t, "grace-hopper", captured.Get("Loam-Agent-Name"), "the clone's own initial fetch must carry the identity headers, not just later fetches/pushes")
	assert.Equal(t, "3", captured.Get("Loam-Agent-Id"))
	assert.Equal(t, "author", captured.Get("Loam-Agent-Role"))
}

// TestExecGitCloner_Clone_MissingBranch_ReturnsGitsOwnReason proves a
// nonexistent branch surfaces git's own error text (what
// runCloneCommand's precondition_failed classification reports to the
// caller), rather than a bare exit-status error with no explanation.
func TestExecGitCloner_Clone_MissingBranch_ReturnsGitsOwnReason(t *testing.T) {
	t.Parallel()
	upstream := newBareRepoWithTwoBranches(t)
	dest := filepath.Join(t.TempDir(), "doc-server")
	cloner := execGitCloner{}

	err := cloner.Clone(t.Context(), upstream, "does-not-exist", dest, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
}

// TestExecGitCloner_AddConfig_AppendsRatherThanReplacing proves AddConfig's
// reason for existing as a method separate from SetConfig, against real
// git: remote.origin.fetch is multi-valued and `git clone --single-branch`
// has already written the cloned branch's own refspec into it, so the
// work-branch refspec must be APPENDED. A SetConfig here would silently
// leave the clone unable to fetch the very branch it was cloned at.
func TestExecGitCloner_AddConfig_AppendsRatherThanReplacing(t *testing.T) {
	t.Parallel()
	upstream := newBareRepoWithTwoBranches(t)
	dest := filepath.Join(t.TempDir(), "doc-server")
	cloner := execGitCloner{}
	require.NoError(t, cloner.Clone(t.Context(), upstream, "main", dest, nil))
	before := mustRunGit(t, dest, "config", "--get-all", "remote.origin.fetch")
	require.Contains(t, before, "refs/heads/main", "precondition: --single-branch wrote the cloned branch's own refspec")

	require.NoError(t, cloner.AddConfig(t.Context(), dest, "remote.origin.fetch", refnames.ClientFetchRefspec))

	got := strings.Split(mustRunGit(t, dest, "config", "--get-all", "remote.origin.fetch"), "\n")
	assert.Len(t, got, 2, "the clone's own refspec must survive alongside the added one")
	assert.Contains(t, got[0], "refs/heads/main")
	assert.Equal(t, refnames.ClientFetchRefspec, got[1])
}
