package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedLookup is a gitLookup returning the same cloneRoot/origin/branch for
// any input directory, the shape most of the tests below need: they only
// vary the directory workspace inference is asked to start from, not the
// lookup's underlying git facts.
func fixedLookup(cloneRoot, origin, branch string) *gitLookupMock {
	return &gitLookupMock{
		CloneRootFunc:     func(string) (string, error) { return cloneRoot, nil },
		OriginURLFunc:     func(string) (string, error) { return origin, nil },
		CurrentBranchFunc: func(string) (string, error) { return branch, nil },
	}
}

// TestWorkspace_AtCloneRoot_InfersRepoFromOriginAndWorkBranchFromHEAD proves
// the loam-0pj.5 acceptance criterion: from the clone root, ResolveRepo
// returns the enrolled "<group>/<repo_name>" identifier derived from the
// origin remote (never the bare directory name — see FIX 1), and
// ResolveWorkBranch returns the current git branch.
func TestWorkspace_AtCloneRoot_InfersRepoFromOriginAndWorkBranchFromHEAD(t *testing.T) {
	t.Parallel()
	cloneRoot := filepath.FromSlash("/workspace/doc-server")
	lookup := fixedLookup(cloneRoot, "https://loam.example/git/bobcob7/doc-server.git", "wb-9c2f1a")
	ws := newWorkspace(cloneRoot, "ada-lovelace-7-reviewer", lookup)
	repo, err := ws.ResolveRepo()
	require.NoError(t, err)
	assert.Equal(t, "bobcob7/doc-server", repo, "repo must be the enrolled <group>/<repo_name> identifier, not the bare directory name")
	branch, err := ws.ResolveWorkBranch()
	require.NoError(t, err)
	assert.Equal(t, "wb-9c2f1a", branch)
}

// TestWorkspace_InsideCloneSubdirectory_StillInfersAndStagesOutsideClone
// proves the two things FIX 2 requires together: inference works from any
// depth inside a clone (cli-spec: "run from inside a repo directory" is not
// limited to the root), and the staging path it computes is NOT under the
// clone root — it lives under the clone's parent (the workspace root),
// exactly as it would from the clone root itself.
func TestWorkspace_InsideCloneSubdirectory_StillInfersAndStagesOutsideClone(t *testing.T) {
	t.Parallel()
	cloneRoot := filepath.FromSlash("/workspace/doc-server")
	nested := filepath.FromSlash("/workspace/doc-server/internal/foo")
	lookup := fixedLookup(cloneRoot, "https://loam.example/git/bobcob7/doc-server.git", "wb-9c2f1a")
	ws := newWorkspace(nested, "ada-lovelace-7-reviewer", lookup)
	repo, err := ws.ResolveRepo()
	require.NoError(t, err)
	assert.Equal(t, "bobcob7/doc-server", repo)
	branch, err := ws.ResolveWorkBranch()
	require.NoError(t, err)
	assert.Equal(t, "wb-9c2f1a", branch)
	staging, err := ws.stagingPath("bobcob7/doc-server", "wb-9c2f1a")
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(staging, cloneRoot), "staging path %q must not be under the clone root %q (cli-spec: staging lives OUTSIDE any clone)", staging, cloneRoot)
	assert.Equal(t, filepath.Join(filepath.Dir(cloneRoot), ".loam", "staging", "bobcob7", "doc-server", "wb-9c2f1a", "ada-lovelace-7-reviewer"), staging)
}

// TestWorkspace_OutsideAnyClone_ResolveFails proves the other half of the
// acceptance criterion: when the caller is not inside any git working
// copy, both ResolveRepo and ResolveWorkBranch fail (the resolution layer
// above turns this into exit 2 when no explicit argument covers the gap).
func TestWorkspace_OutsideAnyClone_ResolveFails(t *testing.T) {
	t.Parallel()
	lookup := &gitLookupMock{CloneRootFunc: func(string) (string, error) { return "", errors.New("not a git repository") }}
	ws := newWorkspace(filepath.FromSlash("/workspace"), "ada-lovelace-7-reviewer", lookup)
	_, err := ws.ResolveRepo()
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotInClone)
	_, err = ws.ResolveWorkBranch()
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotInClone)
}

// TestWorkspace_UnparseableOriginRemote_RepoFailsButWorkBranchStillResolves
// proves repo and work-branch inference are independent: a clone whose
// origin remote is not shaped like a Loam clone URL (e.g. a plain GitHub
// remote) fails ResolveRepo without breaking ResolveWorkBranch, and never
// falls back to a bare directory name.
func TestWorkspace_UnparseableOriginRemote_RepoFailsButWorkBranchStillResolves(t *testing.T) {
	t.Parallel()
	cloneRoot := filepath.FromSlash("/workspace/doc-server")
	lookup := fixedLookup(cloneRoot, "https://github.com/bobcob7/doc-server", "wb-9c2f1a")
	ws := newWorkspace(cloneRoot, "ada-lovelace-7-reviewer", lookup)
	_, err := ws.ResolveRepo()
	require.Error(t, err)
	assert.NotErrorIs(t, err, errNotInClone, "this is a parse failure inside a real clone, not 'not inside a clone'")
	branch, err := ws.ResolveWorkBranch()
	require.NoError(t, err)
	assert.Equal(t, "wb-9c2f1a", branch)
}

// TestWorkspace_NoOriginRemote_RepoFails proves a clone with no "origin"
// remote at all also fails ResolveRepo cleanly (via OriginURL erroring),
// rather than panicking or silently resolving something bogus.
func TestWorkspace_NoOriginRemote_RepoFails(t *testing.T) {
	t.Parallel()
	cloneRoot := filepath.FromSlash("/workspace/doc-server")
	lookup := &gitLookupMock{
		CloneRootFunc:     func(string) (string, error) { return cloneRoot, nil },
		OriginURLFunc:     func(string) (string, error) { return "", errors.New("no such remote 'origin'") },
		CurrentBranchFunc: func(string) (string, error) { return "wb-9c2f1a", nil },
	}
	ws := newWorkspace(cloneRoot, "ada-lovelace-7-reviewer", lookup)
	_, err := ws.ResolveRepo()
	assert.Error(t, err)
}

// TestRepoFromOriginURL covers the origin-URL parser directly across the
// shapes it must accept and reject.
func TestRepoFromOriginURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		origin   string
		wantRepo string
		wantOK   bool
	}{
		{"https loam clone URL", "https://loam.example/git/bobcob7/doc-server.git", "bobcob7/doc-server", true},
		{"http loam clone URL, no .git suffix", "http://loam.example:8080/git/bobcob7/doc-server", "bobcob7/doc-server", true},
		{"plain github URL, no /git/ marker", "https://github.com/bobcob7/doc-server", "", false},
		{"empty", "", "", false},
		{"marker with no group", "https://loam.example/git/doc-server.git", "", false},
		{"marker with trailing slash only", "https://loam.example/git/", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo, ok := repoFromOriginURL(tt.origin)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantRepo, repo)
		})
	}
}

// TestWorkspace_StagingPath_DiffersPerRepoWorkBranchAndAgent proves the
// other loam-0pj.5 acceptance criterion: the staging path is keyed by all
// three of repo, work branch, and agent.
func TestWorkspace_StagingPath_DiffersPerRepoWorkBranchAndAgent(t *testing.T) {
	t.Parallel()
	lookup := &gitLookupMock{CloneRootFunc: func(string) (string, error) { return "", errors.New("outside a clone") }}
	base := newWorkspace(filepath.FromSlash("/workspace"), "ada-lovelace-7-reviewer", lookup)
	other := newWorkspace(filepath.FromSlash("/workspace"), "grace-hopper-3-author", lookup)
	mustStagingPath := func(t *testing.T, ws *workspace, repo, workBranch string) string {
		t.Helper()
		path, err := ws.stagingPath(repo, workBranch)
		require.NoError(t, err)
		return path
	}
	paths := map[string]string{
		"repo-a":       mustStagingPath(t, base, "repo-a", "wb-1"),
		"repo-b":       mustStagingPath(t, base, "repo-b", "wb-1"),
		"branch-b":     mustStagingPath(t, base, "repo-a", "wb-2"),
		"other-agent":  mustStagingPath(t, other, "repo-a", "wb-1"),
		"repo-a-again": mustStagingPath(t, base, "repo-a", "wb-1"),
	}
	assert.NotEqual(t, paths["repo-a"], paths["repo-b"], "staging path must vary by repo")
	assert.NotEqual(t, paths["repo-a"], paths["branch-b"], "staging path must vary by work branch")
	assert.NotEqual(t, paths["repo-a"], paths["other-agent"], "staging path must vary by agent")
	assert.Equal(t, paths["repo-a"], paths["repo-a-again"], "staging path must be stable for the same key")
	assert.True(t, filepath.IsAbs(paths["repo-a"]))
}

// TestWorkspace_StagingPath_AcceptsLegitimateNestedRepoAndStaysContained
// proves the positive case the guard must not break: a repo identifier
// legitimately shaped "<group>/<repo_name>" (docs/cli-spec.md -> clone)
// still nests correctly under the staging root, and the resulting path is
// genuinely contained under it — not merely "no error was returned".
func TestWorkspace_StagingPath_AcceptsLegitimateNestedRepoAndStaysContained(t *testing.T) {
	t.Parallel()
	lookup := &gitLookupMock{CloneRootFunc: func(string) (string, error) { return "", errors.New("outside a clone") }}
	ws := newWorkspace(filepath.FromSlash("/workspace"), "ada-lovelace-7-reviewer", lookup)
	root := filepath.Join(filepath.FromSlash("/workspace"), ".loam", "staging")
	path, err := ws.stagingPath("bobcob7/doc-server", "wb-9c2f1a")
	require.NoError(t, err)
	assertPathContained(t, root, path)
	assert.Equal(t, filepath.Join(root, "bobcob7", "doc-server", "wb-9c2f1a", "ada-lovelace-7-reviewer"), path)
}

// assertPathContained proves path is genuinely under root by computing
// their relative path and requiring it not escape upward (a leading "..")
// — a structural check, not merely the absence of an error. It also
// requires the path be a proper descendant: filepath.Rel(root, root)
// returns "." and filepath.IsLocal(".") is true, so IsLocal alone would
// accept path == root, which is the staging root rather than a per-agent
// subtree within it.
func assertPathContained(t *testing.T, root, path string) {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	require.NoError(t, err)
	assert.Truef(t, filepath.IsLocal(rel), "path %q must resolve to a local (contained) path relative to root %q, got %q", path, root, rel)
	assert.NotEqualf(t, ".", rel, "path %q must be a proper descendant of root %q, not root itself", path, root)
}

// stagingPathAttack is one row of the traversal attack table below: a
// repo or work-branch key an attacker (or a careless script) might supply,
// which StagingPath must reject rather than silently join into an escaping
// path.
type stagingPathAttack struct {
	name string
	key  string
}

// stagingPathAttacks is the shared attack corpus for both the repo and
// work-branch positions: parent-directory traversal in every shape, an
// absolute path, a mixed legitimate-looking traversal, "." alone, the
// empty string, a key of only separators, an oversized key, a key
// containing a NUL byte, and a percent-encoded traversal attempt (inert
// here since nothing upstream of StagingPath ever URL-decodes a CLI
// positional, but still must not slip past the allowlist as literal text).
var stagingPathAttacks = []stagingPathAttack{
	{"dot-dot", ".."},
	{"dot-dot-dot-dot", "../.."},
	{"leading-slash", "/etc/passwd"},
	{"mixed-legitimate-then-traversal", "a/../../b"},
	{"single-dot", "."},
	{"empty", ""},
	{"only-separators", "///"},
	{"very-long-segment", strings.Repeat("a", maxStagingKeySegmentLength+1)},
	{"nul-byte", "wb-1\x00../../etc"},
	{"percent-encoded-traversal", "%2e%2e%2f%2e%2e"},
}

// TestWorkspace_StagingPath_RejectsTraversalInRepoKey proves every attack
// in stagingPathAttacks is rejected with a usage error when supplied as
// the repo key, and never produces a staging path at all — repo's
// legitimate "/" nesting (proven separately above) must not open a gap
// for these.
func TestWorkspace_StagingPath_RejectsTraversalInRepoKey(t *testing.T) {
	t.Parallel()
	lookup := &gitLookupMock{CloneRootFunc: func(string) (string, error) { return "", errors.New("outside a clone") }}
	ws := newWorkspace(filepath.FromSlash("/workspace"), "ada-lovelace-7-reviewer", lookup)
	for _, tt := range stagingPathAttacks {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, err := ws.stagingPath(tt.key, "wb-9c2f1a")
			require.Errorf(t, err, "repo key %q must be rejected", tt.key)
			assert.ErrorIs(t, err, errInvalidStagingKey)
			assert.ErrorIs(t, err, errUsage, "rejection must classify as a usage error (exit 2)")
			assert.Emptyf(t, path, "a rejected key must never produce a staging path, got %q", path)
		})
	}
}

// TestWorkspace_StagingPath_RejectsTraversalInWorkBranchKey mirrors
// TestWorkspace_StagingPath_RejectsTraversalInRepoKey for the work-branch
// position, plus a case repo's allowlist alone would not catch: a
// work-branch key containing a legitimate-looking "/" must still be
// rejected outright, since — unlike repo — a work branch never
// legitimately nests.
func TestWorkspace_StagingPath_RejectsTraversalInWorkBranchKey(t *testing.T) {
	t.Parallel()
	lookup := &gitLookupMock{CloneRootFunc: func(string) (string, error) { return "", errors.New("outside a clone") }}
	ws := newWorkspace(filepath.FromSlash("/workspace"), "ada-lovelace-7-reviewer", lookup)
	attacks := append([]stagingPathAttack{{"legitimate-looking-nested-slash", "bobcob7/doc-server"}}, stagingPathAttacks...)
	for _, tt := range attacks {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, err := ws.stagingPath("bobcob7/doc-server", tt.key)
			require.Errorf(t, err, "work-branch key %q must be rejected", tt.key)
			assert.ErrorIs(t, err, errInvalidStagingKey)
			assert.ErrorIs(t, err, errUsage, "rejection must classify as a usage error (exit 2)")
			assert.Emptyf(t, path, "a rejected key must never produce a staging path, got %q", path)
		})
	}
}

// TestValidateStagingKey_TableOfAttacks unit-tests validateStagingKey
// directly (independent of StagingPath's containment check), proving the
// allowlist itself — not just the containment fallback — is what rejects
// every attack shape. allowNested=true covers repo's shape; the "/"
// entries under allowNested=false additionally prove workBranch's
// stricter no-nesting rule.
func TestValidateStagingKey_TableOfAttacks(t *testing.T) {
	t.Parallel()
	for _, tt := range stagingPathAttacks {
		t.Run("nested/"+tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateStagingKey("repo", tt.key, true)
			require.Errorf(t, err, "key %q must be rejected even when nesting is allowed", tt.key)
			assert.ErrorIs(t, err, errInvalidStagingKey)
		})
		t.Run("flat/"+tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateStagingKey("work branch", tt.key, false)
			require.Errorf(t, err, "key %q must be rejected when nesting is disallowed", tt.key)
			assert.ErrorIs(t, err, errInvalidStagingKey)
		})
	}
	t.Run("legitimate nested repo key is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateStagingKey("repo", "bobcob7/doc-server", true))
	})
	t.Run("flat key with a slash is rejected even though every segment is individually legal", func(t *testing.T) {
		t.Parallel()
		err := validateStagingKey("work branch", "bobcob7/doc-server", false)
		require.Error(t, err)
		assert.ErrorIs(t, err, errInvalidStagingKey)
	})
}

// TestWorkspace_StagingPath_ContainmentCheckCatchesTraversalNotCoveredByAllowlist
// proves the containment check (filepath.IsLocal) is load-bearing on its
// own, not merely redundant with the allowlist: agentIdentifier is the
// third segment StagingPath joins, but it is never run through
// validateStagingKey (it is not attacker-supplied on this path — see
// StagingPath's doc comment — it comes from local LOAM_AGENT_* environment
// configuration). If that changes, or a future caller builds a workspace
// with an unvalidated identifier some other way, the containment check
// alone stands between it and an escaping path. repo ("bobcob7/doc-server",
// 2 segments) plus workBranch ("wb-9c2f1a", 1 segment) give the composed
// relative path 3 segments to climb before an identifier's own ".."
// components can carry it past root; 4 climbs — one more than that — is
// the minimum that actually escapes (filepath.Clean only cancels ".."
// against segments already present in the path, so 3 climbs would merely
// collapse back to root, not escape it).
func TestWorkspace_StagingPath_ContainmentCheckCatchesTraversalNotCoveredByAllowlist(t *testing.T) {
	t.Parallel()
	lookup := &gitLookupMock{CloneRootFunc: func(string) (string, error) { return "", errors.New("outside a clone") }}
	ws := newWorkspace(filepath.FromSlash("/workspace"), "../../../../etc", lookup)
	path, err := ws.stagingPath("bobcob7/doc-server", "wb-9c2f1a")
	require.Error(t, err, "an agent identifier that would escape the staging root must be rejected even though it bypassed key validation")
	assert.ErrorIs(t, err, errInvalidStagingKey)
	assert.Empty(t, path)
}

// TestResolveWorkBranchIdentity_ExplicitArgsWinOverInference proves an
// explicit positional argument is used even when inference would resolve
// to something different — explicit always wins.
func TestResolveWorkBranchIdentity_ExplicitArgsWinOverInference(t *testing.T) {
	t.Parallel()
	ws := &WorkspaceResolverMock{
		ResolveRepoFunc:       func() (string, error) { return "inferred/repo", nil },
		ResolveWorkBranchFunc: func() (string, error) { return "inferred-branch", nil },
	}
	repo, branch, err := resolveWorkBranchIdentity(ws, []string{"explicit/repo", "explicit-branch"})
	require.NoError(t, err)
	assert.Equal(t, "explicit/repo", repo)
	assert.Equal(t, "explicit-branch", branch)
}

// TestResolveWorkBranchIdentity_InfersOmittedArgs proves both positionals
// fall back to inference when omitted entirely.
func TestResolveWorkBranchIdentity_InfersOmittedArgs(t *testing.T) {
	t.Parallel()
	ws := &WorkspaceResolverMock{
		ResolveRepoFunc:       func() (string, error) { return "bobcob7/doc-server", nil },
		ResolveWorkBranchFunc: func() (string, error) { return "wb-9c2f1a", nil },
	}
	repo, branch, err := resolveWorkBranchIdentity(ws, nil)
	require.NoError(t, err)
	assert.Equal(t, "bobcob7/doc-server", repo)
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
	repo, branch, err := resolveWorkBranchIdentity(ws, []string{"explicit/repo"})
	require.NoError(t, err)
	assert.Equal(t, "explicit/repo", repo)
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
	_, _, err := resolveWorkBranchIdentity(ws, []string{"explicit/repo"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errUsage)
}

// runGit runs git with args in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

// TestExecGitLookup_CloneRoot_FromRootAndSubdirectory drives execGitLookup
// — the real gitLookup implementation — against an actual git repository
// in a temp dir, from both the clone root and a nested subdirectory, so the
// mocked-lookup tests above are backed by at least one end-to-end proof
// that the real adapter finds the clone root at any depth (FIX 2).
func TestExecGitLookup_CloneRoot_FromRootAndSubdirectory(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	runGit(t, root, "init", "--initial-branch=main")
	runGit(t, root, "commit", "--allow-empty", "-m", "init")
	nested := filepath.Join(root, "internal", "foo")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	lookup := execGitLookup{}
	for _, dir := range []string{root, nested} {
		got, err := lookup.CloneRoot(dir)
		require.NoError(t, err)
		wantInfo, statErr := os.Stat(root)
		require.NoError(t, statErr)
		gotInfo, statErr := os.Stat(got)
		require.NoError(t, statErr)
		assert.True(t, os.SameFile(wantInfo, gotInfo), "CloneRoot(%q) = %q, want the same directory as %q", dir, got, root)
	}
}

// TestExecGitLookup_CloneRoot_NotAGitDirectory_Errors proves execGitLookup
// rejects a plain (non-clone) directory, the signal workspace inference
// relies on to know it is not inside a clone at all.
func TestExecGitLookup_CloneRoot_NotAGitDirectory_Errors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lookup := execGitLookup{}
	_, err := lookup.CloneRoot(dir)
	assert.Error(t, err)
}

// TestExecGitLookup_OriginURL_ReturnsConfiguredRemote proves execGitLookup
// reads the real "origin" remote URL.
func TestExecGitLookup_OriginURL_ReturnsConfiguredRemote(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	runGit(t, root, "init", "--initial-branch=main")
	runGit(t, root, "remote", "add", "origin", "https://loam.example/git/bobcob7/doc-server.git")
	lookup := execGitLookup{}
	url, err := lookup.OriginURL(root)
	require.NoError(t, err)
	assert.Equal(t, "https://loam.example/git/bobcob7/doc-server.git", url)
}

// TestExecGitLookup_OriginURL_NoRemote_Errors proves execGitLookup fails
// cleanly when there is no "origin" remote configured.
func TestExecGitLookup_OriginURL_NoRemote_Errors(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	runGit(t, root, "init", "--initial-branch=main")
	lookup := execGitLookup{}
	_, err := lookup.OriginURL(root)
	assert.Error(t, err)
}

// TestExecGitLookup_CurrentBranch_ReturnsCheckedOutBranch proves
// execGitLookup reads the real checked-out branch name.
func TestExecGitLookup_CurrentBranch_ReturnsCheckedOutBranch(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	runGit(t, root, "init", "--initial-branch=main")
	runGit(t, root, "commit", "--allow-empty", "-m", "init")
	runGit(t, root, "checkout", "-b", "wb-9c2f1a")
	lookup := execGitLookup{}
	branch, err := lookup.CurrentBranch(root)
	require.NoError(t, err)
	assert.Equal(t, "wb-9c2f1a", branch)
}

// TestNewWorkspaceResolver_InfersFromNestedWorkingDirectory proves
// newWorkspaceResolver wires the real execGitLookup and the process's
// actual cwd, and that inference works from go test's working directory —
// this package's source directory, nested inside the repository's git
// working copy rather than its root — locking in the FIX 2 behavior that
// inference is not limited to the clone root.
func TestNewWorkspaceResolver_InfersFromNestedWorkingDirectory(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if err := exec.Command("git", "rev-parse", "--show-toplevel").Run(); err != nil {
		t.Skip("not running inside a git working copy")
	}
	cfg := &ConfigMock{IdentifierFunc: func() string { return "ada-lovelace-7-reviewer" }}
	ws, err := newWorkspaceResolver(cfg)
	require.NoError(t, err)
	branch, branchErr := ws.ResolveWorkBranch()
	if branchErr != nil {
		t.Skipf("ambient checkout has no resolvable branch (e.g. detached HEAD): %v", branchErr)
	}
	assert.NotEmpty(t, branch, "go test's working directory is nested inside this repo's clone; branch inference must work from any depth")
}
