package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- execGitRefs against a REAL git process ---
//
// These use clone_test.go's fixtures: newBareRepoWithTwoBranches (a bare
// repo with "main" and "wb-1", each holding a commit the other does not)
// and mustRunGit.
//
// A note on what these fixtures deliberately DO NOT collapse, because it is
// the whole point of loam-hwru. It is tempting to write one test that
// clones, fetches, and diffs, and to assert only that the diff came out
// right. That test cannot distinguish "the target ref is present in the
// clone" from "the diff happened to work" -- and those are exactly the two
// states the bead is about, since a diff computed against a GUESSED base
// also comes out looking right. So ref presence is asserted separately,
// against a SHA read from the upstream rather than from the clone under
// test, before any diff is run at all.

// singleBranchCloneOfWB1 clones only "wb-1" out of a two-branch upstream --
// the shape `loam clone` produces, and the shape in which origin/main does
// not exist. It returns the clone's path and the upstream's path.
func singleBranchCloneOfWB1(t *testing.T) (dest, upstream string) {
	t.Helper()
	upstream = newBareRepoWithTwoBranches(t)
	dest = filepath.Join(t.TempDir(), "doc-server")
	require.NoError(t, execGitCloner{}.Clone(t.Context(), upstream, "wb-1", dest, nil))
	return dest, upstream
}

// gitFails runs git and reports whether it FAILED, for asserting the
// before-state. It deliberately does not use mustRunGit, which requires
// success.
func gitFails(t *testing.T, dir string, args ...string) bool {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	return cmd.Run() != nil
}

// TestExecGitRefs_Fetch_MakesTheTargetRefPresentInASingleBranchClone is the
// direct proof of loam-hwru's primary fix, and it asserts REF PRESENCE
// independently of any diff:
//
//   - before: refs/remotes/origin/main does not resolve at all, and `git
//     diff origin/main...HEAD` fails -- the exact "unknown revision" three
//     reviewers hit.
//   - after: refs/remotes/origin/main resolves to the SHA the UPSTREAM
//     reports for main (read from the upstream, so the clone cannot satisfy
//     this assertion with a ref that merely exists and points anywhere).
//
// The diff is then run as a separate, subsequent assertion, so a
// regression that made the diff work by some other route could not disguise
// a missing ref.
func TestExecGitRefs_Fetch_MakesTheTargetRefPresentInASingleBranchClone(t *testing.T) {
	t.Parallel()
	dest, upstream := singleBranchCloneOfWB1(t)
	require.True(t, gitFails(t, dest, "rev-parse", "--verify", "refs/remotes/origin/main"), "precondition: a single-branch clone must NOT already have the target ref")
	require.True(t, gitFails(t, dest, "diff", "--quiet", "origin/main...HEAD"), "precondition: `git diff origin/main...HEAD` must fail before the fix")

	require.NoError(t, execGitRefs{}.Fetch(t.Context(), dest, "+refs/heads/main:refs/remotes/origin/main"))

	upstreamMain := mustRunGit(t, upstream, "rev-parse", "refs/heads/main")
	assert.Equal(t, upstreamMain, mustRunGit(t, dest, "rev-parse", "refs/remotes/origin/main"), "the fetched ref must point at the upstream's main, not merely exist")
}

// TestExecGitRefs_Fetch_MakesPlainGitDiffAndLogWorkAgainstTheBase is the
// second half, kept separate from the ref-presence assertion above on
// purpose (see this file's header). It pins the two commands the bead names
// -- and pins `git log base..HEAD` to the ONE commit that is genuinely on
// wb-1, which is what a wrongly guessed base would get wrong.
func TestExecGitRefs_Fetch_MakesPlainGitDiffAndLogWorkAgainstTheBase(t *testing.T) {
	t.Parallel()
	dest, _ := singleBranchCloneOfWB1(t)
	require.NoError(t, execGitRefs{}.Fetch(t.Context(), dest, "+refs/heads/main:refs/remotes/origin/main"))

	assert.False(t, gitFails(t, dest, "diff", "--quiet", "origin/main...HEAD"), "`git diff origin/main...HEAD` must work in the clone with no loam-specific command")
	log := mustRunGit(t, dest, "log", "--format=%s", "origin/main..HEAD")
	assert.Equal(t, "wb-1 commit", log, "the range must contain exactly the branch's own commit -- a base guessed too far back would include main's too")
}

// TestExecGitRefs_Fetch_DoesNotDragTheRemotesTagsAlong pins the --no-tags
// flag, which nothing else exercises. git's default is to follow tags
// reachable from what it fetches, so a clone that asked for ONE target ref
// would otherwise acquire the mirror's whole tag namespace -- the real
// mirror this runs against carries v0.0.1 through v0.0.8. That is not a
// correctness bug, but it is a silent cost on every clone, and a flag no
// test covers is a flag the next edit deletes.
func TestExecGitRefs_Fetch_DoesNotDragTheRemotesTagsAlong(t *testing.T) {
	t.Parallel()
	dest, upstream := singleBranchCloneOfWB1(t)
	mustRunGit(t, upstream, "tag", "v9.9.9", "refs/heads/main")
	require.NotEmpty(t, mustRunGit(t, upstream, "tag", "--list"), "precondition: the upstream must carry a tag for this to prove anything")

	require.NoError(t, execGitRefs{}.Fetch(t.Context(), dest, "+refs/heads/main:refs/remotes/origin/main"))

	assert.Empty(t, mustRunGit(t, dest, "tag", "--list"), "a fetch for one ref must not pull the remote's tags in behind it")
	assert.NotEmpty(t, mustRunGit(t, dest, "rev-parse", "refs/remotes/origin/main"), "and it must still have fetched the ref it was asked for")
}

// TestExecGitRefs_MergeBase_IsTheCommitTheBranchWasCutFrom pins that
// MergeBase answers with the fork point, which is what `clone` reports as
// base_sha -- NOT the target's tip, which is a different commit whenever
// main has moved on and is the subtler version of the same wrong answer.
func TestExecGitRefs_MergeBase_IsTheCommitTheBranchWasCutFrom(t *testing.T) {
	t.Parallel()
	dest, upstream := singleBranchCloneOfWB1(t)
	require.NoError(t, execGitRefs{}.Fetch(t.Context(), dest, "+refs/heads/main:refs/remotes/origin/main"))

	base, err := execGitRefs{}.MergeBase(t.Context(), dest, "refs/remotes/origin/main", "HEAD")
	require.NoError(t, err)
	head, err := execGitRefs{}.RevParse(t.Context(), dest, "HEAD")
	require.NoError(t, err)

	assert.Equal(t, mustRunGit(t, upstream, "rev-parse", "refs/heads/main"), base, "wb-1 was cut from main's only commit, so that IS the merge base here")
	assert.NotEqual(t, base, head, "base and head must be distinct commits, or this fixture proves nothing")
	assert.Len(t, head, 40, "RevParse must return a full SHA, not an abbreviation")
}

// TestExecGitRefs_RevParse_RejectsANonCommit pins the `^{commit}` peel:
// a ref naming something that is not a commit must be an error here, not a
// SHA that no later range operation can use.
func TestExecGitRefs_RevParse_RejectsANonCommit(t *testing.T) {
	t.Parallel()
	dest, _ := singleBranchCloneOfWB1(t)
	_, err := execGitRefs{}.RevParse(t.Context(), dest, "HEAD^{tree}")
	assert.Error(t, err)
}

// TestExecGitRefs_CountCommitsAhead_CountsWhatTheServerDoesNotHave is the
// check loam-hwru's third failure mode was one command away from. The
// "before" case matters as much as the "after": a fresh clone must report
// ZERO, or the refusal it drives would fire on every reviewer.
func TestExecGitRefs_CountCommitsAhead_CountsWhatTheServerDoesNotHave(t *testing.T) {
	t.Parallel()
	dest, _ := singleBranchCloneOfWB1(t)
	serverTip, err := execGitRefs{}.RevParse(t.Context(), dest, "HEAD")
	require.NoError(t, err)

	ahead, err := execGitRefs{}.CountCommitsAhead(t.Context(), dest, serverTip)
	require.NoError(t, err)
	assert.Equal(t, 0, ahead, "a clone that has pushed nothing is not ahead of anything")

	mustRunGit(t, dest, "-c", "user.name=fixture", "-c", "user.email=f@example.com", "commit", "--quiet", "--allow-empty", "-m", "local only")
	ahead, err = execGitRefs{}.CountCommitsAhead(t.Context(), dest, serverTip)
	require.NoError(t, err)
	assert.Equal(t, 1, ahead, "the commit made after the server's tip must be counted")
}

// TestExecGitRefs_CountCommitsAhead_UnknownBaseIsAnErrorNotZero pins the
// distinction the caller depends on: a base this repository does not have
// is INCONCLUSIVE, and flattening it to 0 would report "nothing unpushed"
// about a comparison that never happened.
func TestExecGitRefs_CountCommitsAhead_UnknownBaseIsAnErrorNotZero(t *testing.T) {
	t.Parallel()
	dest, _ := singleBranchCloneOfWB1(t)
	_, err := execGitRefs{}.CountCommitsAhead(t.Context(), dest, "0123456789012345678901234567890123456789")
	assert.Error(t, err)
}

// TestExecGitRefs_LsRemote_ReportsTheRemotesOwnTips proves LsRemote reads
// what the REMOTE holds, keyed by full ref name, and that a ref the remote
// does not advertise is simply absent rather than an error or an empty
// string masquerading as a SHA.
func TestExecGitRefs_LsRemote_ReportsTheRemotesOwnTips(t *testing.T) {
	t.Parallel()
	upstream := newBareRepoWithTwoBranches(t)

	shas, err := execGitRefs{}.LsRemote(t.Context(), upstream, nil, []string{"refs/heads/main", "refs/heads/wb-1", "refs/heads/nope"})
	require.NoError(t, err)

	assert.Equal(t, mustRunGit(t, upstream, "rev-parse", "refs/heads/main"), shas["refs/heads/main"])
	assert.Equal(t, mustRunGit(t, upstream, "rev-parse", "refs/heads/wb-1"), shas["refs/heads/wb-1"])
	_, present := shas["refs/heads/nope"]
	assert.False(t, present, "an unadvertised ref must be ABSENT, so a caller can tell it apart from one that exists")
}

// TestExecGitRefs_LsRemote_SendsOnlyTheHeadersItWasGiven proves two things
// at once, both of which have to hold or `work diff` silently loses its
// ability to name the commits it reports.
//
//  1. Repeated `-c http.extraHeader=` arguments ACCUMULATE rather than
//     overwrite. If they overwrote, only one header would arrive,
//     httpauth.Auth.GitIdentity would 403, and the diff would degrade to
//     the unidentified artifact this bead exists to remove.
//  2. Headers from an ENCLOSING repository's config do not leak in. This is
//     not hypothetical: `ls-remote` does ordinary repository discovery, and
//     the fixture deliberately runs it from inside a clone whose config
//     holds a DIFFERENT agent's three Loam-Agent-* headers. Without the
//     empty-string reset in LsRemote, that identity is sent first and the
//     request authenticates as someone else.
//
// Asserting on the wire, like
// TestExecGitCloner_Clone_SendsIdentityHeadersOnTheInitialFetch does, is
// the only way to see either; the stub is not a real smart-HTTP backend, so
// git itself fails, and what matters is what arrived before that.
//
// This is the one test in the package that is NOT parallel, and it cannot
// be: it uses t.Chdir to put the process inside the foreign clone, which is
// process-global state and which testing.T.Chdir panics on from a parallel
// test. Nothing else here reads the working directory.
func TestExecGitRefs_LsRemote_SendsOnlyTheHeadersItWasGiven(t *testing.T) {
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	// A clone standing in for "the agent's working directory", carrying
	// SOMEONE ELSE's identity headers in its own config.
	dest, _ := singleBranchCloneOfWB1(t)
	mustRunGit(t, dest, "config", "--add", "http.extraHeader", "Loam-Agent-Name: someone-else")
	mustRunGit(t, dest, "config", "--add", "http.extraHeader", "Loam-Agent-Id: 999")
	t.Chdir(dest)
	headers := []string{
		"Loam-Agent-Name: grace-hopper",
		"Loam-Agent-Id: 3",
		"Loam-Agent-Role: author",
	}

	_, err := execGitRefs{}.LsRemote(t.Context(), srv.URL+"/git/bobcob7/doc-server.git", headers, []string{"refs/heads/main"})
	require.Error(t, err, "the stub server is not a real smart-HTTP backend, so ls-remote itself must fail")
	require.NotNil(t, captured, "git must have sent a request before failing")
	assert.Equal(t, []string{"grace-hopper"}, captured.Values("Loam-Agent-Name"), "exactly the given identity, with the enclosing clone's reset away")
	assert.Equal(t, []string{"3"}, captured.Values("Loam-Agent-Id"))
	assert.Equal(t, []string{"author"}, captured.Values("Loam-Agent-Role"))
}

// TestRunGitOutput_KeepsStderrOutOfTheAnswer pins why runGitOutput is a
// separate helper from clone.go's runGitCommand: stdout IS the answer here,
// and git's chatter must not be interleaved into it. `git fetch` on a
// clone with a target refspec writes its progress to stderr; a helper that
// merged the two would return that text as if it were a SHA.
func TestRunGitOutput_KeepsStderrOutOfTheAnswer(t *testing.T) {
	t.Parallel()
	dest, _ := singleBranchCloneOfWB1(t)
	out, err := runGitOutput(t.Context(), dest, "fetch", "--verbose", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main")
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(out), "a fetch's progress belongs on stderr and must not reach the caller as output")
}
