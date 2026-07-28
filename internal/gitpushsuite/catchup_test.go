package gitpushsuite

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/catchup"
	"github.com/bobcob7/loam/internal/gitancestry"
	"github.com/bobcob7/loam/internal/hooksocket"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// isolatedGit runs a real git command outside the stack under test --
// building fixture state, or verifying it -- cut off from the host's own
// git config and carrying an explicit committer identity.
//
// Copied in shape and reasoning from internal/gittransport/helpers_test.go's
// runVerificationGit: without GIT_AUTHOR_*/GIT_COMMITTER_*, `git commit`
// falls back to guessing username@hostname, which succeeds with a warning
// on a developer laptop and fails outright on a CI runner ("Please tell me
// who you are"), so the gap is invisible locally.
func isolatedGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=loam-test", "GIT_AUTHOR_EMAIL=loam-test@example.invalid",
		"GIT_COMMITTER_NAME=loam-test", "GIT_COMMITTER_EMAIL=loam-test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// recordingClearer is internal/catchup's conflictClearer seam, recording
// every work branch whose conflict was cleared. Hand-written rather than
// moq-generated for the same reason every other fixture in this package
// is: moq mocks live in their own package's moq_test.go and are not
// importable from here (see fakeRepoStore's and stubRoleStore's own doc
// comments). catchup's own decision table is covered by moq mocks in
// internal/catchup/detector_test.go; what THIS suite adds is the real
// git/hook/socket/mirror path underneath it.
type recordingClearer struct {
	mu     sync.Mutex
	calls  []uuid.UUID
	result workbranchstore.WorkBranch
}

func (r *recordingClearer) ClearConflict(_ context.Context, id uuid.UUID) (workbranchstore.WorkBranch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, id)
	return r.result, nil
}

func (r *recordingClearer) Calls() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uuid.UUID(nil), r.calls...)
}

// recordingOpener is internal/catchup's roundOpener seam, recording the
// requested_by attribution of every round opened.
type recordingOpener struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingOpener) OpenRound(_ context.Context, workBranchID uuid.UUID, requestedBy string) (reviewstore.Round, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, requestedBy)
	return reviewstore.Round{WorkBranchID: workBranchID, Number: int32(len(r.calls) + 1), RequestedBy: requestedBy}, nil
}

func (r *recordingOpener) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// catchupBranches is the registry for the test below: one branch that was
// DEMOTED by a conflicting advance (conflict 'reset') and one that was
// merely FLAGGED while draft, both owned by alice and both targeting main.
func catchupBranches() (map[string]workbranchstore.WorkBranch, uuid.UUID, uuid.UUID) {
	demoted := uuid.MustParse("018f0000-0000-7000-8000-00000000000a")
	flagged := uuid.MustParse("018f0000-0000-7000-8000-00000000000b")
	return map[string]workbranchstore.WorkBranch{
		"wb-demoted": {
			ID: demoted, Name: "wb-demoted", Author: aliceIdentifier, Target: "main",
			State: workbranchstore.StateDraft, Conflict: workbranchstore.ConflictReset,
		},
		"wb-flagged": {
			ID: flagged, Name: "wb-flagged", Author: aliceIdentifier, Target: "main",
			State: workbranchstore.StateDraft, Conflict: workbranchstore.ConflictFlagged,
		},
	}, demoted, flagged
}

// TestCatchUp_RealPushThroughTheRealHook is loam-giq.6's Definition of
// Done proven end to end, against the real composition: a real `git push`
// from a real clone, over real HTTP, through the real compiled cmd/loamhook
// binary, over a real unix socket, into a real hooksocket.Server, whose
// post-accept seam drives internal/catchup.Detector with a REAL
// gitancestry.Checker against a REAL bare mirror.
//
// The one thing only this composition can prove -- and the reason the
// unit tests are not enough -- is the object QUARANTINE. While pre-receive
// runs, the pushed commits exist only in receive-pack's temporary
// tmp_objdir-incoming-* directory, not in the mirror's own object store, so
// a `git --git-dir=<mirror>` process cannot resolve the pushed tip at all
// (measured directly against git 2.50.1). Every layer between the hook's
// GIT_QUARANTINE_PATH and gitancestry's GIT_ALTERNATE_OBJECT_DIRECTORIES
// has to be intact for this test to see "caught up"; break any one of them
// and the ancestry check fails instead, the flag stays, and the assertions
// below go red.
//
// It also proves the conditional rule in both directions over the same
// real path: the demoted branch gets a round, the merely-flagged one does
// not.
//
// NOTE on loam-ppb: work_branches.author stores identity.Identifier()
// ("name-id-role") in production while refpolicy.evaluateOne compares it
// against the bare LOAM_AGENT_NAME, so an author cannot currently push to
// their own branch in production at all. This suite's registry sets Author
// to the bare agent name, matching what evaluateOne actually compares --
// the same thing every other test in this package already does. The push
// path being reachable here is therefore an assumption about the FIXED
// world; that P0 is resolved separately and is deliberately neither fixed
// nor worked around here.
func TestCatchUp_RealPushThroughTheRealHook(t *testing.T) {
	t.Parallel()
	branches, demotedID, flaggedID := catchupBranches()
	clearer := &recordingClearer{result: workbranchstore.WorkBranch{
		Name: "wb-demoted", State: workbranchstore.StateReviewable, Conflict: workbranchstore.ConflictNone,
	}}
	opener := &recordingOpener{}
	var env stackEnv
	detectorReady := make(chan struct{})
	// The detector needs env.dataDir, which newStackWithAccept only
	// returns once it has been given the hook -- so the hook closes over
	// the detector through this channel rather than the other way round.
	// It is closed before the server can possibly receive a push, since
	// nothing dials the socket until the first `git push` below.
	var detector *catchup.Detector
	onAccept := func(ctx context.Context, accepted hooksocket.AcceptedPush) {
		<-detectorReady
		detector.OnAcceptedPush(ctx, accepted)
	}
	env = newStackWithAccept(t, branches, loamhookBinary, true, onAccept)
	detector = catchup.New(env.dataDir, gitancestry.New(testLogger()), clearer, opener, testLogger())
	close(detectorReady)

	clone := cloneWithIdentity(t, env, "alice", "1", "author")
	// Advance the mirror's target branch behind the clone's back, exactly
	// as upstream sync would: a plain `git fetch` INTO the bare mirror
	// runs no pre-receive hook, so this models "the target moved" without
	// pretending an agent could push to a read-only ref.
	isolatedGit(t, clone, "checkout", "--quiet", "-b", "advance")
	commitFile(t, clone, "advance.txt", "target advances")
	isolatedGit(t, env.mirrorDir, "--git-dir="+env.mirrorDir, "fetch", "--quiet", clone, "advance:refs/heads/main")
	advancedTip := isolatedGit(t, env.mirrorDir, "--git-dir="+env.mirrorDir, "rev-parse", "refs/heads/main")

	// A work branch forked BEFORE the advance: its history does not
	// contain the new target tip.
	isolatedGit(t, clone, "checkout", "--quiet", "-b", "wb-demoted", "origin/main")
	commitFile(t, clone, "wb.txt", "work in progress")
	out, err := pushRef(t, clone, "refs/heads/wb-demoted")
	require.NoError(t, err, "the push itself must be ACCEPTED -- catch-up detection only runs post-accept: %s", out)
	require.NotEmpty(t, mirrorRefSHA(t, env.mirrorDir, "refs/heads/wb-demoted"), "the ref must actually have landed")
	assert.Empty(t, clearer.Calls(), "a push that does not contain the current target tip must leave the conflict flag alone (docs/git-spec.md: 'the flag simply stays until a push catches up')")
	assert.Empty(t, opener.Calls())

	// Catching up is ordinary git: fetch the target, merge it, push.
	runGit(t, clone, "fetch", "--quiet", "origin", "main")
	isolatedGit(t, clone, "merge", "--quiet", "--no-edit", "FETCH_HEAD")
	require.Equal(t, "", isolatedGit(t, clone, "merge-base", "--is-ancestor", advancedTip, "HEAD"), "fixture precondition: the caught-up branch must now contain the advanced target tip")
	out, err = pushRef(t, clone, "refs/heads/wb-demoted")
	require.NoError(t, err, "push: %s", out)
	require.Equal(t, []uuid.UUID{demotedID}, clearer.Calls(), "a push whose history contains the current target tip must clear the conflict")
	assert.Equal(t, []string{catchup.RoundRequestedBy}, opener.Calls(), "a DEMOTED branch flips back to reviewable, and that transition opens a fresh round attributed to the server")

	// The same caught-up history pushed to a MERELY FLAGGED branch: it
	// loses the flag and nothing else. A round here would invent a review
	// nobody asked for.
	out, err = pushRef(t, clone, "refs/heads/wb-flagged")
	require.NoError(t, err, "push: %s", out)
	assert.Equal(t, []uuid.UUID{demotedID, flaggedID}, clearer.Calls(), "a merely flagged branch still loses its flag on catch-up")
	assert.Equal(t, []string{catchup.RoundRequestedBy}, opener.Calls(), "the merely-flagged branch never transitioned into reviewable, so it must NOT have opened a second round")
}
