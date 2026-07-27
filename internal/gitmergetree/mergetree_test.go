package gitmergetree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireGit skips when the git binary is not on PATH, matching
// internal/gittransport's own convention for tests that shell out to a
// real git subprocess.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found on PATH; skipping merge-tree test")
	}
}

// testLogger is a discard logger, per this repo's Go testing standard.
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// runFixtureGit runs a real git command to BUILD or INSPECT a fixture --
// never through Checker -- isolated from the host's git config the same
// way internal/gittransport's runVerificationGit is, and for the same
// reasons: GIT_CONFIG_NOSYSTEM plus a temp HOME/XDG_CONFIG_HOME so the
// developer machine's system gitconfig (macOS Command Line Tools ships
// one) cannot mask a bug, and an explicit author/committer identity
// because those three isolation settings remove every config file that
// would otherwise supply one -- git then falls back to guessing
// user@hostname, which succeeds with a warning on a laptop and fails
// outright on a CI runner with "Please tell me who you are". --author on
// a commit does NOT cover this; it sets only the author, never the
// committer.
func runFixtureGit(t *testing.T, args ...string) string {
	t.Helper()
	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "unused-global-gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=loam-test", "GIT_AUTHOR_EMAIL=loam-test@example.invalid",
		"GIT_COMMITTER_NAME=loam-test", "GIT_COMMITTER_EMAIL=loam-test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// mirrorFixture is a bare mirror carrying four branches built from one
// root commit, covering every merge outcome this package classifies:
//
//   - main      -- the "target branch", advanced past the fork point.
//   - clean     -- edits a file main never touched: merges cleanly.
//   - conflict  -- edits the same line main did: conflicts.
//   - unrelated -- an orphan branch sharing no history with main.
type mirrorFixture struct {
	dir string
}

// newMirrorFixture materializes mirrorFixture in a temp directory: a
// working-tree repo is built with real commits, then cloned bare, which is
// exactly the shape internal/mirrorpath.Dir points production at.
func newMirrorFixture(t *testing.T) mirrorFixture {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	runFixtureGit(t, "init", "-q", "-b", "main", wt)
	write := func(name, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(wt, name), []byte(content), 0o644))
	}
	git := func(args ...string) { runFixtureGit(t, append([]string{"-C", wt}, args...)...) }
	write("shared.txt", "root\n")
	git("add", ".")
	git("commit", "-qm", "root")
	git("branch", "clean")
	git("branch", "conflict")
	write("shared.txt", "target edit\n")
	git("commit", "-qam", "target advances")
	git("checkout", "-q", "clean")
	write("untouched.txt", "clean side\n")
	git("add", ".")
	git("commit", "-qm", "clean work")
	git("checkout", "-q", "conflict")
	write("shared.txt", "work branch edit\n")
	git("commit", "-qam", "conflicting work")
	git("checkout", "-q", "--orphan", "unrelated")
	git("rm", "-rqf", ".")
	write("orphan.txt", "no shared history\n")
	git("add", ".")
	git("commit", "-qm", "orphan root")
	mirror := filepath.Join(root, "widgets.git")
	runFixtureGit(t, "clone", "-q", "--bare", wt, mirror)
	return mirrorFixture{dir: mirror}
}

// refs returns `git for-each-ref` output for the mirror, the exact string
// a no-ref-writes assertion compares before and after a check.
func (f mirrorFixture) refs(t *testing.T) string {
	t.Helper()
	return runFixtureGit(t, "--git-dir="+f.dir, "for-each-ref")
}

// sha resolves ref in the mirror to its full object ID.
func (f mirrorFixture) sha(t *testing.T, ref string) string {
	t.Helper()
	return runFixtureGit(t, "--git-dir="+f.dir, "rev-parse", ref)
}

func TestMergeTree_CleanMergeReportsNotConflicted(t *testing.T) {
	t.Parallel()
	fixture := newMirrorFixture(t)
	conflicted, err := New(testLogger()).MergeTree(t.Context(), fixture.dir, "refs/heads/clean", fixture.sha(t, "refs/heads/main"))
	require.NoError(t, err)
	assert.False(t, conflicted, "a work branch touching only files the target never touched must merge cleanly")
}

func TestMergeTree_ConflictingMergeReportsConflicted(t *testing.T) {
	t.Parallel()
	fixture := newMirrorFixture(t)
	conflicted, err := New(testLogger()).MergeTree(t.Context(), fixture.dir, "refs/heads/conflict", fixture.sha(t, "refs/heads/main"))
	require.NoError(t, err)
	assert.True(t, conflicted, "a work branch editing the same line the target edited must be reported as conflicting")
}

// TestMergeTree_TargetTipAcceptedAsARawSHA pins that the production call
// shape works: the target side is passed as the bare NewSHA an Advance
// carries, never as a ref path, so a target ref that has since moved again
// is still checked against the tip the sync actually observed.
func TestMergeTree_TargetTipAcceptedAsARawSHA(t *testing.T) {
	t.Parallel()
	fixture := newMirrorFixture(t)
	forkPoint := fixture.sha(t, "refs/heads/main~1")
	conflicted, err := New(testLogger()).MergeTree(t.Context(), fixture.dir, "refs/heads/conflict", forkPoint)
	require.NoError(t, err)
	assert.False(t, conflicted, "merging the fork point itself back in is a no-op merge and must be clean")
}

// TestMergeTree_MissingRefIsAnErrorNotAConflict is the load-bearing test
// of this package. Measured against git 2.50.1, `merge-tree --write-tree`
// reports an unresolvable ref with exit status 1 -- byte-identical to how
// it reports a genuine conflict -- and distinguishes the two only by
// leaving stdout empty. A classifier trusting exit status alone therefore
// reports a missing work-branch ref as "this branch conflicts", which in
// production demotes a reviewable branch to draft and voids its verdicts
// over a check that never ran.
func TestMergeTree_MissingRefIsAnErrorNotAConflict(t *testing.T) {
	t.Parallel()
	fixture := newMirrorFixture(t)
	conflicted, err := New(testLogger()).MergeTree(t.Context(), fixture.dir, "refs/heads/never-created", fixture.sha(t, "refs/heads/main"))
	require.Error(t, err, "a work-branch ref absent from the mirror must fail the check, not answer it")
	assert.ErrorIs(t, err, errCheckFailed)
	assert.False(t, conflicted, "a failed check must never come back as conflicted=true")
}

// TestMergeTree_UnknownTargetSHAIsAnErrorNotAConflict is the same hazard
// approached from the other argument: a well-formed but nonexistent object
// ID on the target side also exits 1 with empty stdout.
func TestMergeTree_UnknownTargetSHAIsAnErrorNotAConflict(t *testing.T) {
	t.Parallel()
	fixture := newMirrorFixture(t)
	conflicted, err := New(testLogger()).MergeTree(t.Context(), fixture.dir, "refs/heads/clean", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	require.Error(t, err)
	assert.ErrorIs(t, err, errCheckFailed)
	assert.False(t, conflicted)
}

// TestMergeTree_UnrelatedHistoriesIsAnErrorNotAConflict pins the exit-128
// branch of the classifier: git refuses to attempt the merge at all, so
// there is no verdict to report. Reporting "conflicted" here would be a
// guess dressed as an answer.
func TestMergeTree_UnrelatedHistoriesIsAnErrorNotAConflict(t *testing.T) {
	t.Parallel()
	fixture := newMirrorFixture(t)
	conflicted, err := New(testLogger()).MergeTree(t.Context(), fixture.dir, "refs/heads/unrelated", fixture.sha(t, "refs/heads/main"))
	require.Error(t, err)
	assert.ErrorIs(t, err, errCheckFailed)
	assert.False(t, conflicted)
}

// TestMergeTree_MissingMirrorIsAnError covers the operational fault of an
// enrolled repo whose mirror is absent or corrupt on disk.
func TestMergeTree_MissingMirrorIsAnError(t *testing.T) {
	t.Parallel()
	requireGit(t)
	conflicted, err := New(testLogger()).MergeTree(t.Context(), filepath.Join(t.TempDir(), "absent.git"), "refs/heads/wb", "refs/heads/main")
	require.Error(t, err)
	assert.ErrorIs(t, err, errCheckFailed)
	assert.False(t, conflicted)
}

// TestMergeTree_CanceledContextIsAnErrorNotAConflict pins that a killed
// subprocess is never mistaken for git's own answer: cancellation kills
// git, which surfaces as a nonzero exit that would otherwise reach the
// classifier.
func TestMergeTree_CanceledContextIsAnErrorNotAConflict(t *testing.T) {
	t.Parallel()
	fixture := newMirrorFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	conflicted, err := New(testLogger()).MergeTree(ctx, fixture.dir, "refs/heads/conflict", fixture.sha(t, "refs/heads/main"))
	require.Error(t, err)
	assert.ErrorIs(t, err, errCheckFailed)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, conflicted)
}

// realExitError returns a genuine *exec.ExitError with the given exit
// status, by running a command that exits with it. Fabricating one by
// hand is not possible -- it wraps an *os.ProcessState with no exported
// constructor -- and a stub error would not exercise the errors.As branch
// under test.
func realExitError(t *testing.T, status int) error {
	t.Helper()
	err := exec.CommandContext(t.Context(), "sh", "-c", fmt.Sprintf("exit %d", status)).Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "fixture must produce a real *exec.ExitError")
	require.Equal(t, status, exitErr.ExitCode())
	return err
}

// TestClassifyRunErr_CancellationWinsOverTheProcessExitStatus pins the
// ordering inside classifyRunErr. exec.CommandContext KILLS git when the
// context is done, and a killed process comes back as an ordinary
// *exec.ExitError -- so if the ExitError branch ran first, a check
// interrupted by shutdown would be classified as git's own answer about
// the merge. Driven through classifyRunErr directly rather than through a
// real subprocess because cancellation landing after git starts (as
// opposed to before) cannot be scheduled deterministically from a test.
func TestClassifyRunErr_CancellationWinsOverTheProcessExitStatus(t *testing.T) {
	t.Parallel()
	exitErr := realExitError(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, err := classifyRunErr(ctx, exitErr, []byte("30cee01b7b15478ab01dce5377bf07720e4b10b1\n"), "", []string{"merge-tree"})
	require.Error(t, err, "a canceled context must not be reported as git's exit-1 conflict answer")
	assert.ErrorIs(t, err, errCheckFailed)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, out.exitCode)
}

// TestClassifyRunErr_LiveContextReportsTheProcessExitStatus is the
// complement: with the context still live, a nonzero exit is data, not an
// error, so classify can decide what it means.
func TestClassifyRunErr_LiveContextReportsTheProcessExitStatus(t *testing.T) {
	t.Parallel()
	out, err := classifyRunErr(t.Context(), realExitError(t, 128), []byte("stdout"), "stderr", []string{"merge-tree"})
	require.NoError(t, err)
	assert.Equal(t, 128, out.exitCode)
	assert.Equal(t, []byte("stdout"), out.stdout)
	assert.Equal(t, "stderr", out.stderr)
}

// TestClassifyRunErr_NonExitFailureIsACheckFailure covers git failing to
// run at all (binary missing, fork failure): there is no exit status to
// classify, so it can only be a check failure.
func TestClassifyRunErr_NonExitFailureIsACheckFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("exec: \"git\": executable file not found in $PATH")
	_, err := classifyRunErr(t.Context(), boom, nil, "", []string{"merge-tree"})
	require.ErrorIs(t, err, errCheckFailed)
	assert.ErrorIs(t, err, boom)
}

// TestMergeTree_WritesNoRefs is the spec constraint itself
// (docs/sync-spec.md -> Mergeability Check: "no worktree, no writes to any
// ref"; the pre-receive ref policy, loam-ofg.18, exists to enforce exactly
// this class of rule). Asserted over the CONFLICTING case specifically,
// since that is the path that produces a tree plus conflicted blobs and so
// has the most to write.
func TestMergeTree_WritesNoRefs(t *testing.T) {
	t.Parallel()
	fixture := newMirrorFixture(t)
	before := fixture.refs(t)
	require.NotEmpty(t, before, "fixture must have refs for this comparison to mean anything")
	conflicted, err := New(testLogger()).MergeTree(t.Context(), fixture.dir, "refs/heads/conflict", fixture.sha(t, "refs/heads/main"))
	require.NoError(t, err)
	require.True(t, conflicted, "this test is only meaningful over the write-heaviest path")
	assert.Equal(t, before, fixture.refs(t), "merge-tree must create, move, or delete no ref")
}

// TestMergeTree_RepeatedChecksAreStable pins that the check is a pure
// read with respect to its own verdict: running it twice over the same
// inputs answers the same both times, so the level-triggered
// re-evaluation the mergeability checker performs on every advance cannot
// drift.
func TestMergeTree_RepeatedChecksAreStable(t *testing.T) {
	t.Parallel()
	fixture := newMirrorFixture(t)
	checker := New(testLogger())
	tip := fixture.sha(t, "refs/heads/main")
	first, err := checker.MergeTree(t.Context(), fixture.dir, "refs/heads/conflict", tip)
	require.NoError(t, err)
	second, err := checker.MergeTree(t.Context(), fixture.dir, "refs/heads/conflict", tip)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.True(t, second)
}

// TestClassify_TrustsNeitherExitStatusAloneNorOutputAlone drives the
// classifier directly over the exact stdout/stderr/exit triples measured
// against git 2.50.1 (see the package doc comment's table), including the
// two shapes a real git can produce that a naive implementation gets
// wrong: exit 1 with empty stdout (unresolvable ref, NOT a conflict) and
// exit 0 with non-OID stdout (the legacy trivial-merge form, or a git too
// old to know --write-tree).
func TestClassify_TrustsNeitherExitStatusAloneNorOutputAlone(t *testing.T) {
	t.Parallel()
	const oid = "30cee01b7b15478ab01dce5377bf07720e4b10b1"
	tests := []struct {
		name       string
		out        gitOutput
		conflicted bool
		wantErr    bool
	}{
		{
			name: "clean merge is exit 0 with a bare object id",
			out:  gitOutput{exitCode: 0, stdout: []byte(oid + "\n")},
		},
		{
			name:       "conflict is exit 1 with an object id then conflict detail",
			out:        gitOutput{exitCode: 1, stdout: []byte(oid + "\n100644 aaa 1\tf.txt\n\nCONFLICT (content): Merge conflict in f.txt\n")},
			conflicted: true,
		},
		{
			name:    "exit 1 with empty stdout is an unresolvable ref, not a conflict",
			out:     gitOutput{exitCode: 1, stderr: "merge-tree: refs/heads/nope - not something we can merge\n"},
			wantErr: true,
		},
		{
			name:    "exit 128 is a merge git refused to attempt",
			out:     gitOutput{exitCode: 128, stderr: "fatal: refusing to merge unrelated histories\n"},
			wantErr: true,
		},
		{
			name:    "exit 129 is a git without --write-tree",
			out:     gitOutput{exitCode: 129, stderr: "error: unknown option `write-tree'\nusage: git merge-tree ...\n"},
			wantErr: true,
		},
		{
			name:    "exit 0 with legacy trivial-merge output is not a clean answer",
			out:     gitOutput{exitCode: 0, stdout: []byte("changed in both\n  base   100644 aaa f.txt\n")},
			wantErr: true,
		},
		{
			name:    "a truncated object id is rejected",
			out:     gitOutput{exitCode: 0, stdout: []byte(oid[:39] + "\n")},
			wantErr: true,
		},
		{
			name:    "an object id with trailing text on the same line is rejected",
			out:     gitOutput{exitCode: 1, stdout: []byte(oid + " and then some\n")},
			wantErr: true,
		},
		{
			name:    "a non-hex string of object-id length is rejected",
			out:     gitOutput{exitCode: 0, stdout: []byte(strings.Repeat("z", 40) + "\n")},
			wantErr: true,
		},
		{
			name: "a sha256 mirror's 64-character object id is accepted",
			out:  gitOutput{exitCode: 0, stdout: []byte(strings.Repeat("ab", 32) + "\n")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conflicted, err := classify(tc.out)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, errCheckFailed)
				assert.False(t, conflicted, "a failed classification must never report a conflict")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.conflicted, conflicted)
		})
	}
}

// TestMergeTree_ErrorNamesBothRefsAndTheMirror pins that a failure is
// diagnosable: the operator reading repos.sync_state needs to know which
// branch, which tip, and which mirror could not be checked.
func TestMergeTree_ErrorNamesBothRefsAndTheMirror(t *testing.T) {
	t.Parallel()
	fixture := newMirrorFixture(t)
	tip := fixture.sha(t, "refs/heads/main")
	_, err := New(testLogger()).MergeTree(t.Context(), fixture.dir, "refs/heads/never-created", tip)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refs/heads/never-created")
	assert.Contains(t, err.Error(), tip)
	assert.Contains(t, err.Error(), fixture.dir)
	assert.Contains(t, err.Error(), "not something we can merge", "git's own explanation must survive into the error")
}

// TestGitEnv_IsolatesFromAmbientConfig pins the isolation list itself,
// which is load-bearing rather than hygiene: a merge driver,
// merge.conflictStyle, or core.autocrlf picked up from a system or
// user-global gitconfig could change this check's verdict from one machine
// to another, and macOS's Command Line Tools ship a system gitconfig by
// default. It is a whitelist, not os.Environ() plus additions, so nothing
// ambient reaches git at all.
func TestGitEnv_IsolatesFromAmbientConfig(t *testing.T) {
	t.Parallel()
	home := "/tmp/loam-gitmergetree-test-home"
	env := gitEnv(home)
	assert.Contains(t, env, "GIT_CONFIG_NOSYSTEM=1")
	assert.Contains(t, env, "HOME="+home)
	assert.Contains(t, env, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	assert.Contains(t, env, "GIT_CONFIG_GLOBAL="+filepath.Join(home, "unused-global-gitconfig"))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		assert.Contains(t, []string{
			"PATH", "GIT_CONFIG_NOSYSTEM", "HOME", "XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL",
			"GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_PAGER",
			"GIT_TRACE", "GIT_TRACE_CURL", "GIT_CURL_VERBOSE", "GIT_TRACE_PACKET",
			"GIT_TRACE_PACK_ACCESS", "GIT_TRACE_SETUP",
		}, name, "gitEnv must be an explicit whitelist; %s is not on it", name)
	}
}
