//go:build acceptance

// features/sync.feature's step definitions, plus the upstream/mirror
// fixture plumbing they share. Everything here drives the SAME
// *mirrorsync.Scheduler newSyncHarness builds out of seven production
// collaborators (loam-a16) -- no scenario in this file stubs, decorates,
// or short-circuits a step of the Mirror Sync cycle; the only thing these
// steps do that production would not is SEED state (a repo on the fake
// forge, a bare mirror, a work_branches row, an upstream_pr_number),
// exactly as acceptance_seed_test.go already seeds enrollment.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cucumber/godog"
	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/fakeforge"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/mirrorpath"
)

// registerSyncSteps wires every step features/sync.feature's Background
// and its scenarios need. The "the next sync runs" / "the upstream PR
// merges" rows are NOT registered here -- they are core step-vocabulary
// rows already owned by registerVocabularySteps
// (acceptance_steps_test.go).
func (h *acceptanceHarness) registerSyncSteps(sc *godog.ScenarioContext) {
	sc.Step(`^I am signed in to the web interface as the admin$`, h.stepIAmSignedInAsAdmin)
	sc.Step(`^the mirror's "([^"]*)" has diverged from upstream$`, h.stepMirrorHasDiverged)
	sc.Step(`^the mirror's "([^"]*)" matches upstream exactly$`, h.stepMirrorMatchesUpstream)
	sc.Step(`^upstream has deleted its branch "([^"]*)"$`, h.stepUpstreamDeletedItsBranch)
	sc.Step(`^the mirror no longer has "([^"]*)"$`, h.stepMirrorNoLongerHas)
	sc.Step(`^a work branch "([^"]*)" exists$`, h.stepAWorkBranchExists)
	sc.Step(`^upstream has a branch named "([^"]*)"$`, h.stepUpstreamHasABranchNamed)
	sc.Step(`^the work branch "([^"]*)" is unchanged in the mirror$`, h.stepWorkBranchIsUnchangedInTheMirror)
	sc.Step(`^upstream has deleted the target branch "([^"]*)"$`, h.stepUpstreamDeletedTheTargetBranch)
	sc.Step(`^the repo's sync status shows an error naming "([^"]*)"$`, h.stepSyncStatusShowsAnErrorNaming)
	sc.Step(`^existing work branches are left untouched$`, h.stepExistingWorkBranchesAreLeftUntouched)
	sc.Step(`^the upstream forge is unreachable$`, h.stepUpstreamForgeIsUnreachable)
	sc.Step(`^the repo's sync status shows the error$`, h.stepSyncStatusShowsTheError)
	sc.Step(`^the forge is reachable again and the next sync runs$`, h.stepForgeReachableAgainAndSyncRuns)
	sc.Step(`^the repo's sync status is healthy$`, h.stepSyncStatusIsHealthy)
	sc.Step(`^an accepted work branch whose upstream PR has merged$`, h.stepAnAcceptedWorkBranchWhosePRHasMerged)
	sc.Step(`^the work branch is in state "([^"]*)"$`, h.stepTheWorkBranchIsInState)
	sc.Step(`^the "([^"]*)" branch is removed from the upstream forge$`, h.stepBranchIsRemovedFromUpstream)
}

// stepIAmSignedInAsAdmin is the Background row every admin-facing feature
// file opens with. It is a real, authenticated driver call, not a no-op
// flag: one RepoAdminService.ListRepos over the Admin actor's connect-go
// client (HTTP basic auth, acceptance_admin_test.go), which the router
// rejects with Unauthenticated if the credentials this suite configured
// are not the ones the server booted with. A step that merely recorded
// "signed in" in the world would pass against a server that accepts no
// admin at all.
func (h *acceptanceHarness) stepIAmSignedInAsAdmin(ctx context.Context) error {
	if _, err := h.listReposAsAdmin(ctx); err != nil {
		return fmt.Errorf("signing in to the admin API as %q: %w", h.adminUser, err)
	}
	return nil
}

// stepMirrorHasDiverged builds the scenario's mirror from its real
// upstream and then moves the mirror's own branch off that upstream tip
// with a genuine local commit, so the mirror and upstream have really
// diverged (the mirror is strictly ahead; a fetch that is not forced
// would leave it exactly where it is). Both SHAs are recorded so
// stepMirrorMatchesUpstream can prove the fetch actually rewound the
// mirror rather than assert an equality that held before the tick.
func (h *acceptanceHarness) stepMirrorHasDiverged(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	upstreamSHA, err := h.upstreamRefSHA(ctx, world, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	divergedSHA, err := commitIntoMirror(ctx, world.mirrorDir, branch, "", "MIRROR-DIVERGENCE.txt", "written into the mirror only\n", "acceptance: diverge the mirror")
	if err != nil {
		return err
	}
	if divergedSHA == upstreamSHA {
		return fmt.Errorf("mirror %q still points at the upstream tip %s after the divergence commit", branch, upstreamSHA)
	}
	world.divergedSHA = divergedSHA
	return nil
}

// stepMirrorMatchesUpstream asserts the mirror's branch is byte-for-byte
// the upstream tip, read back from the forge itself over an authenticated
// ls-remote rather than from any value this harness cached. It also fails
// if the mirror still holds the divergent commit the Given wrote, so a
// tick that silently did nothing cannot pass this step.
func (h *acceptanceHarness) stepMirrorMatchesUpstream(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	upstreamSHA, err := h.upstreamRefSHA(ctx, world, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	mirrorSHA, err := mirrorRefSHA(world.mirrorDir, "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("reading mirror ref refs/heads/%s: %w", branch, err)
	}
	if world.divergedSHA != "" && mirrorSHA == world.divergedSHA {
		return fmt.Errorf("mirror's %q is still the pre-sync divergent commit %s; the sync never rewound it", branch, mirrorSHA)
	}
	if mirrorSHA != upstreamSHA {
		return fmt.Errorf("mirror's %q is %s, upstream's is %s", branch, mirrorSHA, upstreamSHA)
	}
	return nil
}

// stepUpstreamDeletedItsBranch establishes the only history under which
// "upstream deleted a branch" is observable at all: the branch must have
// existed upstream, and the mirror must already hold it, before the
// deletion. So it pushes branch upstream from the mirror, re-clones the
// mirror so it genuinely fetched that branch, asserts the mirror has it,
// and only then deletes it upstream.
func (h *acceptanceHarness) stepUpstreamDeletedItsBranch(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	if _, err := h.transport.Push(ctx, h.forgeHost, world.mirrorDir, world.upstreamURL, "refs/heads/"+world.targetBranch+":refs/heads/"+branch); err != nil {
		return fmt.Errorf("creating upstream branch %s: %w", branch, err)
	}
	if err := h.cloneMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	if _, err := mirrorRefSHA(world.mirrorDir, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("mirror never fetched branch %s, so its later absence would prove nothing: %w", branch, err)
	}
	if err := h.forge.DeleteBranch(ctx, world.repo(), branch); err != nil {
		return fmt.Errorf("deleting upstream branch %s: %w", branch, err)
	}
	return nil
}

// stepMirrorNoLongerHas asserts branch is gone from the mirror.
func (h *acceptanceHarness) stepMirrorNoLongerHas(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	sha, err := mirrorRefSHA(world.mirrorDir, "refs/heads/"+branch)
	if err == nil {
		return fmt.Errorf("mirror still has refs/heads/%s at %s", branch, sha)
	}
	return nil
}

// stepAWorkBranchExists seeds one registered work branch: a work_branches
// row plus a real ref in the mirror at its own commit, distinct from the
// target branch's tip so a later comparison can tell them apart.
func (h *acceptanceHarness) stepAWorkBranchExists(ctx context.Context, name string) error {
	world := worldFrom(ctx)
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	world.workBranch = name
	if err := h.insertWorkBranchRow(ctx, world.repoID, name, world.targetBranch, "draft", world.agentName); err != nil {
		return err
	}
	sha, err := commitIntoMirror(ctx, world.mirrorDir, name, "", "WORK.txt", "work branch content\n", "acceptance: work branch commit")
	if err != nil {
		return err
	}
	world.workBranchSHA = sha
	return nil
}

// stepUpstreamHasABranchNamed grows a same-named branch on the forge --
// the collision docs/git-spec.md's "Work-branch refs" ref policy exists
// for. It asserts the upstream branch is at a DIFFERENT commit from the
// mirror's work branch, since a collision at the same SHA would make
// "unchanged" true no matter what the fetch did.
func (h *acceptanceHarness) stepUpstreamHasABranchNamed(ctx context.Context, name string) error {
	world := worldFrom(ctx)
	if err := h.forge.CreateCollidingBranch(ctx, world.repo(), name, ""); err != nil {
		return fmt.Errorf("creating colliding upstream branch %s: %w", name, err)
	}
	upstreamSHA, err := h.upstreamRefSHA(ctx, world, "refs/heads/"+name)
	if err != nil {
		return err
	}
	if upstreamSHA == world.workBranchSHA {
		return fmt.Errorf("upstream branch %s is at the same commit %s as the mirror's work branch, so this scenario could not observe a clobber", name, upstreamSHA)
	}
	return nil
}

// stepWorkBranchIsUnchangedInTheMirror asserts the mirror's work-branch
// ref still points at the exact commit stepAWorkBranchExists wrote, i.e.
// that the fetch's negative refspec kept the same-named upstream branch
// out entirely.
func (h *acceptanceHarness) stepWorkBranchIsUnchangedInTheMirror(ctx context.Context, name string) error {
	world := worldFrom(ctx)
	sha, err := mirrorRefSHA(world.mirrorDir, "refs/heads/"+name)
	if err != nil {
		return fmt.Errorf("reading mirror ref refs/heads/%s: %w", name, err)
	}
	if sha != world.workBranchSHA {
		return fmt.Errorf("mirror's work branch %s moved from %s to %s", name, world.workBranchSHA, sha)
	}
	return nil
}

// stepUpstreamDeletedTheTargetBranch deletes a LISTED target branch
// upstream -- docs/sync-spec.md's "If a target branch disappears
// upstream, the repo goes to sync_state = error naming the branch".
//
// It also seeds one open work branch first, deliberately: the scenario's
// closing assertion is "existing work branches are left untouched", and
// with no work branch in the repo that assertion is unfalsifiable. This
// feature file's Background enrolls a repo and nothing else, so the only
// place the world can grow the work branch that step talks about is here,
// in the Given that sets up the failure it must survive.
//
// That work branch is seeded REVIEWABLE and genuinely conflicting: it
// forks from the pre-edit target tip and rewrites the same file the
// target then rewrites, so a cycle that ever reached step 3 for this repo
// would merge-test it, find the conflict, and demote it to
// draft/conflict=reset (workbranchstore.Store.MarkConflicted). "Left
// untouched" is therefore a claim that can fail, not one the fixture
// makes true by having nothing a Mergeability Check could move.
//
// It sets expectSyncError so the shared "the next sync runs" step allows
// this tick to end in sync_state = error (see stepTheNextSyncRuns).
func (h *acceptanceHarness) stepUpstreamDeletedTheTargetBranch(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	if err := h.insertWorkBranchRow(ctx, world.repoID, world.workBranch, world.targetBranch, "reviewable", world.agentName); err != nil {
		return err
	}
	if _, err := commitIntoMirror(ctx, world.mirrorDir, world.workBranch, "refs/heads/"+world.targetBranch, "README.md", "work branch rewrite\n", "acceptance: work branch edit"); err != nil {
		return err
	}
	if _, err := commitIntoMirror(ctx, world.mirrorDir, world.targetBranch, "", "README.md", "target rewrite\n", "acceptance: conflicting target edit"); err != nil {
		return err
	}
	before, err := h.workBranchStates(ctx, world.repoID)
	if err != nil {
		return err
	}
	if len(before) == 0 {
		return fmt.Errorf("repo %s has no work branch, so 'existing work branches are left untouched' could not fail", world.repo())
	}
	world.workBranchesBefore = before
	world.expectSyncError = true
	if err := h.forge.DeleteBranch(ctx, world.repo(), branch); err != nil {
		return fmt.Errorf("deleting upstream target branch %s: %w", branch, err)
	}
	return nil
}

// stepSyncStatusShowsAnErrorNaming asserts, through the admin API rather
// than a direct SQL read, that the repo is in the error sync state and
// that the recorded message names branch.
func (h *acceptanceHarness) stepSyncStatusShowsAnErrorNaming(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	repo, err := h.getRepoAsAdmin(ctx, world.repo())
	if err != nil {
		return err
	}
	if repo.GetSync().GetState() != adminv1.SyncState_SYNC_STATE_ERROR {
		return fmt.Errorf("repo %s sync state is %s, want SYNC_STATE_ERROR", world.repo(), repo.GetSync().GetState())
	}
	if !strings.Contains(repo.GetSync().GetError(), branch) {
		return fmt.Errorf("repo %s sync error %q does not name %q", world.repo(), repo.GetSync().GetError(), branch)
	}
	return nil
}

// stepExistingWorkBranchesAreLeftUntouched asserts every work branch the
// repo had before the failing tick is still in exactly the state and
// conflict flag it had then -- the guarantee docs/sync-spec.md attaches to
// aborting the cycle at step 2 (mergeability never runs, so nothing is
// demoted or flagged over a target branch nobody could check against).
func (h *acceptanceHarness) stepExistingWorkBranchesAreLeftUntouched(ctx context.Context) error {
	world := worldFrom(ctx)
	if len(world.workBranchesBefore) == 0 {
		return fmt.Errorf("no work branch was recorded before the sync, so this step would assert nothing")
	}
	after, err := h.workBranchStates(ctx, world.repoID)
	if err != nil {
		return err
	}
	for name, before := range world.workBranchesBefore {
		got, ok := after[name]
		if !ok {
			return fmt.Errorf("work branch %s disappeared during the sync", name)
		}
		if got != before {
			return fmt.Errorf("work branch %s changed from %s to %s during the sync", name, before, got)
		}
	}
	return nil
}

// stepUpstreamForgeIsUnreachable points the repo's upstream_url at a
// reserved-then-closed local port, so the very next fetch fails at the
// transport with a connection refusal -- a real failing fetch, not an
// injected error. The reachable URL is kept in the world for
// stepForgeReachableAgainAndSyncRuns to restore.
func (h *acceptanceHarness) stepUpstreamForgeIsUnreachable(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	unreachable := "http://" + acceptanceFreeAddr(h.t) + "/unreachable.git"
	if err := h.setUpstreamURL(ctx, world.repoID, unreachable); err != nil {
		return err
	}
	world.expectSyncError = true
	return nil
}

// stepSyncStatusShowsTheError asserts the repo is in the error sync state
// with a non-empty message.
func (h *acceptanceHarness) stepSyncStatusShowsTheError(ctx context.Context) error {
	world := worldFrom(ctx)
	repo, err := h.getRepoAsAdmin(ctx, world.repo())
	if err != nil {
		return err
	}
	if repo.GetSync().GetState() != adminv1.SyncState_SYNC_STATE_ERROR {
		return fmt.Errorf("repo %s sync state is %s, want SYNC_STATE_ERROR", world.repo(), repo.GetSync().GetState())
	}
	if repo.GetSync().GetError() == "" {
		return fmt.Errorf("repo %s is in the error state with no recorded message", world.repo())
	}
	return nil
}

// stepForgeReachableAgainAndSyncRuns restores the real upstream URL and
// runs one more tick -- the retry docs/sync-spec.md describes ("the tick
// interval is the retry backoff"), driven here by an explicit tick rather
// than a wall clock.
func (h *acceptanceHarness) stepForgeReachableAgainAndSyncRuns(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := h.setUpstreamURL(ctx, world.repoID, world.upstreamURL); err != nil {
		return err
	}
	world.expectSyncError = false
	return h.stepTheNextSyncRuns(ctx)
}

// stepSyncStatusIsHealthy asserts the repo reached the idle sync state
// with its stale error cleared. It is deliberately the IDLE state, not
// merely "not error": a repo left mid-cycle at SYNCING has not
// demonstrated that a later cycle recovered.
func (h *acceptanceHarness) stepSyncStatusIsHealthy(ctx context.Context) error {
	world := worldFrom(ctx)
	repo, err := h.getRepoAsAdmin(ctx, world.repo())
	if err != nil {
		return err
	}
	if repo.GetSync().GetState() != adminv1.SyncState_SYNC_STATE_IDLE {
		return fmt.Errorf("repo %s sync state is %s (error %q), want SYNC_STATE_IDLE", world.repo(), repo.GetSync().GetState(), repo.GetSync().GetError())
	}
	if repo.GetSync().GetError() != "" {
		return fmt.Errorf("repo %s is idle but still carries the stale error %q", world.repo(), repo.GetSync().GetError())
	}
	return nil
}

// stepAnAcceptedWorkBranchWhosePRHasMerged reproduces, by seeding, the
// exact state Proposal Acceptance leaves behind (docs/sync-spec.md ->
// Proposal Acceptance): a work branch in the mirror, its tip pushed
// upstream as loam/<name>, an open PR from that branch into the target,
// and the PR's number recorded on the work_branches row -- then merges
// that PR on the forge.
//
// The upstream_pr_number write is a direct UPDATE because NOTHING in the
// tree writes that column yet: loam-giq.7 (Proposal Acceptance's own
// push+CreatePR+record leg) is still open, which is what leaves
// StorePRPoller's poll set permanently empty in production. Seeding the
// column is therefore not a shortcut past a code path that exists -- it
// is the only way to reach StorePRPoller at all until giq.7 lands, and it
// is the single fixture in this file that a production code path will
// later replace.
func (h *acceptanceHarness) stepAnAcceptedWorkBranchWhosePRHasMerged(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	if err := h.insertWorkBranchRow(ctx, world.repoID, world.workBranch, world.targetBranch, "reviewed", world.agentName); err != nil {
		return err
	}
	sha, err := commitIntoMirror(ctx, world.mirrorDir, world.workBranch, "", "PROPOSAL.txt", "proposed change\n", "acceptance: proposal commit")
	if err != nil {
		return err
	}
	world.workBranchSHA = sha
	upstreamBranch := "loam/" + world.workBranch
	if _, err := h.transport.Push(ctx, h.forgeHost, world.mirrorDir, world.upstreamURL, "refs/heads/"+world.workBranch+":refs/heads/"+upstreamBranch); err != nil {
		return fmt.Errorf("pushing %s upstream: %w", upstreamBranch, err)
	}
	if _, err := h.upstreamRefSHA(ctx, world, "refs/heads/"+upstreamBranch); err != nil {
		return fmt.Errorf("upstream branch %s was never created, so its later absence would prove nothing: %w", upstreamBranch, err)
	}
	_, number, err := h.forgeClient.CreatePR(ctx, world.repo(), upstreamBranch, world.targetBranch, "acceptance proposal", "acceptance proposal body")
	if err != nil {
		return fmt.Errorf("opening upstream PR for %s: %w", upstreamBranch, err)
	}
	world.upstreamPRNumber = number
	if err := h.recordUpstreamPRNumber(ctx, world.repoID, world.workBranch, number); err != nil {
		return err
	}
	if err := h.forge.MergePR(ctx, world.repo(), number); err != nil {
		return fmt.Errorf("merging upstream PR %s#%d: %w", world.repo(), number, err)
	}
	return nil
}

// stepTheWorkBranchIsInState asserts the scenario's work branch reached
// want. workBranchStates records "<state>/<conflict>" per branch (the two
// columns a cycle can move), so only the state half is compared here --
// this step is about work_branches.state alone.
func (h *acceptanceHarness) stepTheWorkBranchIsInState(ctx context.Context, want string) error {
	world := worldFrom(ctx)
	states, err := h.workBranchStates(ctx, world.repoID)
	if err != nil {
		return err
	}
	record, ok := states[world.workBranch]
	if !ok {
		return fmt.Errorf("work branch %s not found for repo %s", world.workBranch, world.repo())
	}
	got, _, _ := strings.Cut(record, "/")
	if got != want {
		return fmt.Errorf("work branch %s is in state %q, want %q", world.workBranch, got, want)
	}
	return nil
}

// stepBranchIsRemovedFromUpstream asserts no upstream ref remains under
// prefix (e.g. "loam/"), read straight off the forge with an
// authenticated ls-remote.
func (h *acceptanceHarness) stepBranchIsRemovedFromUpstream(ctx context.Context, prefix string) error {
	world := worldFrom(ctx)
	refs, err := h.upstreamRefs(ctx, world)
	if err != nil {
		return err
	}
	for ref := range refs {
		if strings.HasPrefix(ref, "refs/heads/"+prefix) {
			return fmt.Errorf("upstream still has %s", ref)
		}
	}
	return nil
}

// seedUpstreamRepo creates world's repo on the shared fake forge, with a
// single initial commit on its target branch, and records the clone URL
// every later fetch/push/ls-remote in this scenario uses. afterScenario
// removes it again (fakeforge.Server.RemoveRepo), so the next scenario
// naming the same repo starts from an empty forge rather than inheriting
// this one's branches and PR numbers.
func (h *acceptanceHarness) seedUpstreamRepo(ctx context.Context, world *acceptanceWorld) error {
	files := acceptanceUpstreamFiles(world.repo())
	if err := h.forge.SeedRepoFiles(ctx, world.repo(), files, fakeforge.SeedOptions{DefaultBranch: world.targetBranch}); err != nil {
		return fmt.Errorf("seeding upstream repo %s: %w", world.repo(), err)
	}
	world.upstreamSeeded = true
	world.upstreamURL = h.forge.GitURL(world.repo())
	return nil
}

// ensureMirrorFromUpstream clones world's bare mirror from its upstream
// unless this scenario already has one.
func (h *acceptanceHarness) ensureMirrorFromUpstream(ctx context.Context, world *acceptanceWorld) error {
	if world.mirrorDir != "" {
		return nil
	}
	return h.cloneMirrorFromUpstream(ctx, world)
}

// cloneMirrorFromUpstream runs the production enrollment clone
// (gittransport.Transport.Clone, the same call RepoAdminService.EnrollRepo
// makes) into the mirror path production derives, replacing any mirror
// already there.
func (h *acceptanceHarness) cloneMirrorFromUpstream(ctx context.Context, world *acceptanceWorld) error {
	mirrorDir := mirrorpath.Dir(h.server.dataDir, world.repo())
	if _, err := h.transport.Clone(ctx, h.forgeHost, mirrorDir, world.upstreamURL); err != nil {
		return fmt.Errorf("cloning mirror for %s: %w", world.repo(), err)
	}
	world.mirrorDir = mirrorDir
	return nil
}

// upstreamRefs lists world's upstream refs by name -> SHA, over the same
// authenticated transport production uses.
func (h *acceptanceHarness) upstreamRefs(ctx context.Context, world *acceptanceWorld) (map[string]string, error) {
	out, err := h.transport.LsRemote(ctx, h.forgeHost, world.upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("listing upstream refs for %s: %w", world.repo(), err)
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/") {
			continue
		}
		refs[fields[1]] = fields[0]
	}
	return refs, nil
}

// upstreamRefSHA returns ref's upstream SHA, erroring if the forge does
// not advertise it.
func (h *acceptanceHarness) upstreamRefSHA(ctx context.Context, world *acceptanceWorld, ref string) (string, error) {
	refs, err := h.upstreamRefs(ctx, world)
	if err != nil {
		return "", err
	}
	sha, ok := refs[ref]
	if !ok {
		return "", fmt.Errorf("upstream %s has no %s", world.repo(), ref)
	}
	return sha, nil
}

// setUpstreamURL rewrites the repo row's upstream_url, the one knob these
// scenarios use to make the forge unreachable and reachable again.
func (h *acceptanceHarness) setUpstreamURL(ctx context.Context, repoID uuid.UUID, url string) error {
	if _, err := h.server.pool.Exec(ctx, `UPDATE repos SET upstream_url = $1 WHERE id = $2`, url, repoID); err != nil {
		return fmt.Errorf("rewriting upstream_url for repo %s: %w", repoID, err)
	}
	return nil
}

// recordUpstreamPRNumber writes work_branches.upstream_pr_number -- the
// column loam-giq.7 will own; see
// stepAnAcceptedWorkBranchWhosePRHasMerged's doc comment.
func (h *acceptanceHarness) recordUpstreamPRNumber(ctx context.Context, repoID uuid.UUID, name string, number int) error {
	tag, err := h.server.pool.Exec(ctx,
		`UPDATE work_branches SET upstream_pr_number = $1 WHERE repo_id = $2 AND name = $3`, number, repoID, name)
	if err != nil {
		return fmt.Errorf("recording upstream PR number for work branch %s: %w", name, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("recording upstream PR number for work branch %s: %d rows updated, want 1", name, tag.RowsAffected())
	}
	return nil
}

// workBranchStates reads every work branch of repoID back as name ->
// "<state>/<conflict>", the two columns a Mirror Sync cycle can move.
func (h *acceptanceHarness) workBranchStates(ctx context.Context, repoID uuid.UUID) (map[string]string, error) {
	rows, err := h.server.pool.Query(ctx, `SELECT name, state, conflict FROM work_branches WHERE repo_id = $1`, repoID)
	if err != nil {
		return nil, fmt.Errorf("reading work branches for repo %s: %w", repoID, err)
	}
	defer rows.Close()
	states := make(map[string]string)
	for rows.Next() {
		var name, state, conflict string
		if err := rows.Scan(&name, &state, &conflict); err != nil {
			return nil, fmt.Errorf("scanning work branch row: %w", err)
		}
		states[name] = state + "/" + conflict
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading work branches for repo %s: %w", repoID, err)
	}
	return states, nil
}

// commitIntoMirror writes one commit onto branch inside the bare mirror
// at mirrorDir, by cloning it locally, committing, and pushing back --
// the only way to move a bare repo's ref to a NEW commit without
// hand-rolling git plumbing. branch is reset to fromRef first when
// fromRef is non-empty (how a scenario builds a work branch that forked
// from an older target tip, and so genuinely conflicts with a later one);
// an empty fromRef branches off whatever the clone's own HEAD is.
// Returns the new commit's SHA as the mirror now records it.
func commitIntoMirror(ctx context.Context, mirrorDir, branch, fromRef, filename, content, message string) (string, error) {
	work, err := os.MkdirTemp("", "loam-acceptance-mirror-commit-*")
	if err != nil {
		return "", fmt.Errorf("creating scratch clone dir: %w", err)
	}
	defer os.RemoveAll(work)
	clone := filepath.Join(work, "clone")
	if _, err := runIsolatedGit(ctx, work, "clone", "--quiet", mirrorDir, clone); err != nil {
		return "", fmt.Errorf("cloning mirror %s: %w", mirrorDir, err)
	}
	checkout := []string{"checkout", "--quiet", "-B", branch}
	if fromRef != "" {
		checkout = append(checkout, "origin/"+strings.TrimPrefix(fromRef, "refs/heads/"))
	}
	if _, err := runIsolatedGit(ctx, clone, checkout...); err != nil {
		return "", fmt.Errorf("checking out %s: %w", branch, err)
	}
	if err := os.WriteFile(filepath.Join(clone, filename), []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", filename, err)
	}
	if _, err := runIsolatedGit(ctx, clone, "add", filename); err != nil {
		return "", fmt.Errorf("staging %s: %w", filename, err)
	}
	if _, err := runIsolatedGit(ctx, clone, "commit", "--quiet", "-m", message); err != nil {
		return "", fmt.Errorf("committing %s: %w", filename, err)
	}
	if _, err := runIsolatedGit(ctx, clone, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return "", fmt.Errorf("pushing %s into the mirror: %w", branch, err)
	}
	return mirrorRefSHA(mirrorDir, "refs/heads/"+branch)
}

// runIsolatedGit runs one git subcommand with no ambient configuration
// and an explicit identity: GIT_CONFIG_NOSYSTEM plus a redirected HOME/
// XDG_CONFIG_HOME drop the system and user gitconfig (macOS's Command
// Line Tools ship a system credential.helper -- see
// internal/gittransport's gitEnv for the defect that caused), and the
// four GIT_AUTHOR_*/GIT_COMMITTER_* variables mean a commit never depends
// on git guessing user@hostname, which a developer laptop allows and CI
// refuses.
func runIsolatedGit(ctx context.Context, dir string, args ...string) (string, error) {
	home, err := os.MkdirTemp("", "loam-acceptance-githome-*")
	if err != nil {
		return "", fmt.Errorf("creating isolated git home: %w", err)
	}
	defer os.RemoveAll(home)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=loam-acceptance",
		"GIT_AUTHOR_EMAIL=acceptance@example.invalid",
		"GIT_COMMITTER_NAME=loam-acceptance",
		"GIT_COMMITTER_EMAIL=acceptance@example.invalid",
	}
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), runErr, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
