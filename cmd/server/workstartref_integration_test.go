//go:build integration

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/refnames"
)

// This file is loam-5iu's end-to-end proof, and it is deliberately an
// INTEGRATION test rather than a handler unit test: the claim is not "the
// handler calls a seam" (internal/handler/workbranch's own tests pin that)
// but "after `loam work start`, the ref is really in the mirror and `loam
// work diff` really works" -- which needs a real Postgres, the real booted
// server, the real gitref subprocess, and the compiled CLI, exactly the
// stack clonepush_integration_test.go already assembles and whose helpers
// this file reuses.
//
// Before loam-5iu, CreateWorkBranch inserted the work_branches row and
// nothing created the ref, so GetWorkBranchDiff answered ErrRefMissing ->
// FailedPrecondition for essentially every work branch: the error path was
// the common case, not an edge case.

// startedWorkBranch is the subset of `loam work start`'s JSON output this
// file reads.
type startedWorkBranch struct {
	Repo   string `json:"repo"`
	Name   string `json:"name"`
	Target string `json:"target"`
	State  string `json:"state"`
}

// mirrorRefSHAOrEmpty reads ref back from the bare mirror, returning "" when
// it does not exist -- never trusting the server's own report for the "did
// the ref actually land" proof.
func mirrorRefSHAOrEmpty(t *testing.T, mirrorDir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+mirrorDir, "rev-parse", "--verify", "--quiet", ref)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestWorkStart_CreatesTheMirrorRef_AndDiffWorksImmediately is loam-5iu.
//
// Three claims, in the order an agent meets them:
//
//  1. `work start` puts the ref in the mirror, at the target's tip, under
//     the reserved namespace and NOT at refs/heads/<name>.
//  2. `work diff` on that branch -- with no push, no clone, nothing else --
//     succeeds. It reports an empty diff, which is the CORRECT answer for a
//     branch that is exactly its target, and the assertion that matters is
//     the exit code: this call used to exit 2 with precondition_failed.
//  3. `loam clone` of that freshly started branch works, which it could not
//     when there was no ref to fetch.
func TestWorkStart_CreatesTheMirrorRef_AndDiffWorksImmediately(t *testing.T) {
	dsn := newPostgres(t)
	dataDir := shortDataDir(t)
	rs := startServerWithDataDir(t, dsn, dataDir)
	const repoName = "acme/widgets"
	const targetBranch = "main"
	seedEnrolledRepoWithMirror(t, dsn, dataDir, repoName, targetBranch)
	mirrorDir := mirrorpath.Dir(dataDir, repoName)
	targetTip := mirrorRefSHAOrEmpty(t, mirrorDir, refnames.TargetBranch(targetBranch))
	require.NotEmpty(t, targetTip, "precondition: the seeded mirror must carry the target branch")

	workspace := t.TempDir()
	env := loamAgentEnv("http://"+rs.addr, "ada-lovelace", "7", "author")
	started := runLoamCLI(t, workspace, env, "work", "start", repoName, targetBranch)
	require.Equal(t, 0, started.exitCode, "stdout: %s\nstderr: %s", started.stdout, started.stderr)
	var wb startedWorkBranch
	require.NoError(t, json.Unmarshal([]byte(started.stdout), &wb))
	require.Regexp(t, `^wb-[0-9a-f]{6}$`, wb.Name)

	assert.Equal(t, targetTip, mirrorRefSHAOrEmpty(t, mirrorDir, refnames.WorkBranch(wb.Name)),
		"`work start` must create the work branch's ref in the mirror, at its target's tip (loam-5iu)")
	assert.Empty(t, mirrorRefSHAOrEmpty(t, mirrorDir, "refs/heads/"+wb.Name),
		"the ref must live under the reserved namespace, never at the unreserved path a mirror fetch could prune")

	diff := runLoamCLI(t, workspace, env, "work", "diff", repoName, wb.Name)
	assert.Equal(t, 0, diff.exitCode,
		"`work diff` on a freshly started work branch must succeed -- it used to exit 2 with precondition_failed because the ref did not exist (loam-5iu). stdout: %s stderr: %s", diff.stdout, diff.stderr)
	assert.NotContains(t, diff.stdout, "precondition_failed")

	cloned := runLoamCLI(t, workspace, env, "clone", repoName, wb.Name)
	require.Equal(t, 0, cloned.exitCode, "cloning a freshly started work branch must work: there is now a ref to fetch. stdout: %s stderr: %s", cloned.stdout, cloned.stderr)
	clonePath := filepath.Join(workspace, "widgets")
	require.DirExists(t, clonePath)
	assert.Equal(t, wb.Name, runGitCmd(t, clonePath, "symbolic-ref", "--short", "HEAD"),
		"the clone must be checked out on the work branch under its BARE name, not the reserved ref path")
}

// TestWorkStart_TargetBranchMissingFromMirror_ReportsAPrecondition covers
// the one ref-creation failure an agent can act on, and proves it is not
// reported as an internal error: `from` is validated against
// repo_target_branches, so a repo enrolled before its first sync has the
// row and no ref in the mirror.
//
// The mirror here is a real, valid bare repository that simply does not
// carry the branch -- not a missing directory, which would be the
// operational ErrMirrorMissing fault instead.
func TestWorkStart_TargetBranchMissingFromMirror_ReportsAPrecondition(t *testing.T) {
	dsn := newPostgres(t)
	dataDir := shortDataDir(t)
	rs := startServerWithDataDir(t, dsn, dataDir)
	const repoName = "acme/widgets"
	const targetBranch = "main"
	seedEnrolledRepoWithMirror(t, dsn, dataDir, repoName, targetBranch)
	mirrorDir := mirrorpath.Dir(dataDir, repoName)
	// Remove the target branch from the mirror while leaving the repository
	// itself valid -- the pre-first-sync shape.
	runGitCmd(t, "", "--git-dir="+mirrorDir, "symbolic-ref", "HEAD", "refs/heads/placeholder")
	runGitCmd(t, "", "--git-dir="+mirrorDir, "update-ref", "-d", refnames.TargetBranch(targetBranch))
	require.Empty(t, mirrorRefSHAOrEmpty(t, mirrorDir, refnames.TargetBranch(targetBranch)))

	workspace := t.TempDir()
	env := loamAgentEnv("http://"+rs.addr, "ada-lovelace", "7", "author")
	started := runLoamCLI(t, workspace, env, "work", "start", repoName, targetBranch)

	assert.Equal(t, 2, started.exitCode, "stdout: %s stderr: %s", started.stdout, started.stderr)
	assert.Contains(t, started.stdout, "precondition_failed")
	entries, err := os.ReadDir(filepath.Join(mirrorDir, "refs", "heads"))
	if err == nil {
		for _, e := range entries {
			assert.NotEqual(t, "loam-reserved", e.Name(), "no work-branch ref may be left behind when the create could not resolve its base commit")
		}
	}
}
