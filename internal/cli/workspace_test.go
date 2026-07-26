package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorkspace_InsideClone_InfersRepoAndWorkBranch proves the
// loam-0pj.5 acceptance criterion: inside a clone directory, ResolveRepo
// and ResolveWorkBranch return the directory name and the current git
// branch respectively, with no explicit argument.
func TestWorkspace_InsideClone_InfersRepoAndWorkBranch(t *testing.T) {
	t.Parallel()
	lookup := &gitBranchLookupMock{
		CurrentBranchFunc: func(dir string) (string, error) {
			assert.Equal(t, filepath.FromSlash("/workspace/doc-server"), dir)
			return "wb-9c2f1a", nil
		},
	}
	ws := newWorkspace(filepath.FromSlash("/workspace/doc-server"), "ada-lovelace-7-reviewer", lookup)
	repo, err := ws.ResolveRepo()
	require.NoError(t, err)
	assert.Equal(t, "doc-server", repo)
	branch, err := ws.ResolveWorkBranch()
	require.NoError(t, err)
	assert.Equal(t, "wb-9c2f1a", branch)
}

// TestWorkspace_OutsideClone_ResolveFails proves the other half of the
// acceptance criterion: outside a clone, both ResolveRepo and
// ResolveWorkBranch fail (the resolution layer above turns this into exit
// 2 when no explicit argument covers the gap).
func TestWorkspace_OutsideClone_ResolveFails(t *testing.T) {
	t.Parallel()
	lookup := &gitBranchLookupMock{
		CurrentBranchFunc: func(dir string) (string, error) {
			return "", errors.New("not a git repository")
		},
	}
	ws := newWorkspace(filepath.FromSlash("/workspace"), "ada-lovelace-7-reviewer", lookup)
	_, err := ws.ResolveRepo()
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotInClone)
	_, err = ws.ResolveWorkBranch()
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotInClone)
}

// TestWorkspace_StagingPath_DiffersPerRepoWorkBranchAndAgent proves the
// other loam-0pj.5 acceptance criterion: the staging path is keyed by all
// three of repo, work branch, and agent.
func TestWorkspace_StagingPath_DiffersPerRepoWorkBranchAndAgent(t *testing.T) {
	t.Parallel()
	lookup := &gitBranchLookupMock{CurrentBranchFunc: func(string) (string, error) { return "", errors.New("outside a clone") }}
	base := newWorkspace(filepath.FromSlash("/workspace"), "ada-lovelace-7-reviewer", lookup)
	other := newWorkspace(filepath.FromSlash("/workspace"), "grace-hopper-3-author", lookup)
	paths := map[string]string{
		"repo-a":       base.StagingPath("repo-a", "wb-1"),
		"repo-b":       base.StagingPath("repo-b", "wb-1"),
		"branch-b":     base.StagingPath("repo-a", "wb-2"),
		"other-agent":  other.StagingPath("repo-a", "wb-1"),
		"repo-a-again": base.StagingPath("repo-a", "wb-1"),
	}
	assert.NotEqual(t, paths["repo-a"], paths["repo-b"], "staging path must vary by repo")
	assert.NotEqual(t, paths["repo-a"], paths["branch-b"], "staging path must vary by work branch")
	assert.NotEqual(t, paths["repo-a"], paths["other-agent"], "staging path must vary by agent")
	assert.Equal(t, paths["repo-a"], paths["repo-a-again"], "staging path must be stable for the same key")
	assert.True(t, filepath.IsAbs(paths["repo-a"]))
}

// TestResolveWorkBranchIdentity_ExplicitArgsWinOverInference proves an
// explicit positional argument is used even when inference would resolve
// to something different — explicit always wins.
func TestResolveWorkBranchIdentity_ExplicitArgsWinOverInference(t *testing.T) {
	t.Parallel()
	ws := &WorkspaceResolverMock{
		ResolveRepoFunc:       func() (string, error) { return "inferred-repo", nil },
		ResolveWorkBranchFunc: func() (string, error) { return "inferred-branch", nil },
	}
	repo, branch, err := resolveWorkBranchIdentity(ws, []string{"explicit-repo", "explicit-branch"})
	require.NoError(t, err)
	assert.Equal(t, "explicit-repo", repo)
	assert.Equal(t, "explicit-branch", branch)
}

// TestResolveWorkBranchIdentity_InfersOmittedArgs proves both positionals
// fall back to inference when omitted entirely.
func TestResolveWorkBranchIdentity_InfersOmittedArgs(t *testing.T) {
	t.Parallel()
	ws := &WorkspaceResolverMock{
		ResolveRepoFunc:       func() (string, error) { return "doc-server", nil },
		ResolveWorkBranchFunc: func() (string, error) { return "wb-9c2f1a", nil },
	}
	repo, branch, err := resolveWorkBranchIdentity(ws, nil)
	require.NoError(t, err)
	assert.Equal(t, "doc-server", repo)
	assert.Equal(t, "wb-9c2f1a", branch)
}

// TestResolveWorkBranchIdentity_OnlyRepoGiven_InfersWorkBranch proves the
// two positionals resolve independently: an explicit repo does not
// suppress work-branch inference.
func TestResolveWorkBranchIdentity_OnlyRepoGiven_InfersWorkBranch(t *testing.T) {
	t.Parallel()
	ws := &WorkspaceResolverMock{
		ResolveRepoFunc: func() (string, error) {
			t.Fatal("ResolveRepo must not be called when repo is explicit")
			return "", nil
		},
		ResolveWorkBranchFunc: func() (string, error) { return "wb-9c2f1a", nil },
	}
	repo, branch, err := resolveWorkBranchIdentity(ws, []string{"explicit-repo"})
	require.NoError(t, err)
	assert.Equal(t, "explicit-repo", repo)
	assert.Equal(t, "wb-9c2f1a", branch)
}

// TestResolveWorkBranchIdentity_Unresolvable_IsUsageError proves the third
// loam-0pj.5 acceptance criterion: with no explicit argument and inference
// failing, the result is a usage error (exit 2 via the ErrorMapper).
func TestResolveWorkBranchIdentity_Unresolvable_IsUsageError(t *testing.T) {
	t.Parallel()
	ws := &WorkspaceResolverMock{
		ResolveRepoFunc:       func() (string, error) { return "", errNotInClone },
		ResolveWorkBranchFunc: func() (string, error) { return "", errNotInClone },
	}
	_, _, err := resolveWorkBranchIdentity(ws, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errUsage)
	assert.ErrorIs(t, err, errNotInClone)
	mapper := newErrorMapper()
	assert.Equal(t, 2, mapper.ExitCode(err))
}

// TestResolveWorkBranchIdentity_WorkBranchUnresolvable_IsUsageError proves
// the repo/work-branch positionals fail independently: an explicit repo
// with an unresolvable work branch is still a usage error, and ResolveRepo
// is never consulted.
func TestResolveWorkBranchIdentity_WorkBranchUnresolvable_IsUsageError(t *testing.T) {
	t.Parallel()
	ws := &WorkspaceResolverMock{
		ResolveRepoFunc: func() (string, error) {
			t.Fatal("ResolveRepo must not be called when repo is explicit")
			return "", nil
		},
		ResolveWorkBranchFunc: func() (string, error) { return "", errNotInClone },
	}
	_, _, err := resolveWorkBranchIdentity(ws, []string{"explicit-repo"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errUsage)
}

// TestExecGitBranchLookup_RealGitRepo drives execGitBranchLookup — the real
// gitBranchLookup implementation — against an actual git repository in a
// temp dir, so the mocked-lookup tests above are backed by at least one
// end-to-end proof that the real adapter behaves the same way.
func TestExecGitBranchLookup_RealGitRepo(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	run("init", "--initial-branch=main")
	run("commit", "--allow-empty", "-m", "init")
	run("checkout", "-b", "wb-9c2f1a")
	lookup := execGitBranchLookup{}
	branch, err := lookup.CurrentBranch(dir)
	require.NoError(t, err)
	assert.Equal(t, "wb-9c2f1a", branch)
}

// TestExecGitBranchLookup_NotAGitDirectory_Errors proves execGitBranchLookup
// rejects a plain (non-clone) directory, the signal workspace inference
// relies on to know it is not inside a clone.
func TestExecGitBranchLookup_NotAGitDirectory_Errors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lookup := execGitBranchLookup{}
	_, err := lookup.CurrentBranch(dir)
	assert.Error(t, err)
}

// TestExecGitBranchLookup_NestedInsideClone_Errors proves execGitBranchLookup
// requires dir to be exactly the clone root, matching docs/cli-spec.md's
// "inside a clone at /<repo_name>" (the root, not an arbitrary
// subdirectory) — a directory nested inside a repo must not be mistaken
// for the clone itself.
func TestExecGitBranchLookup_NestedInsideClone_Errors(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	run(root, "init", "--initial-branch=main")
	nested := filepath.Join(root, "sub")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	lookup := execGitBranchLookup{}
	_, err := lookup.CurrentBranch(nested)
	assert.Error(t, err)
}

// TestNewWorkspaceResolver_UsesCurrentWorkingDirectory proves
// NewWorkspaceResolver wires the real execGitBranchLookup and the process's
// actual cwd. go test's working directory (this package's source
// directory) is nested inside the repository's git working copy, not the
// working copy's own root, so it is not "inside a clone" for inference
// purposes — ResolveRepo/ResolveWorkBranch fail exactly as they would for
// any agent working from a directory that is not a clone root.
func TestNewWorkspaceResolver_UsesCurrentWorkingDirectory(t *testing.T) {
	t.Parallel()
	cfg := &ConfigMock{IdentifierFunc: func() string { return "ada-lovelace-7-reviewer" }}
	ws, err := NewWorkspaceResolver(cfg)
	require.NoError(t, err)
	_, err = ws.ResolveRepo()
	assert.Error(t, err, "go test's working directory is nested inside a repo, not a clone root")
}
