//go:build acceptance

// features/sync.feature's step definitions, plus the upstream/mirror
// fixture plumbing they share. Everything here drives the SAME
// *mirrorsync.Scheduler newSyncHarness builds out of seven production
// collaborators (loam-a16) -- no scenario in this file stubs, decorates,
// or short-circuits a step of the Mirror Sync cycle; the only thing these
// steps do that production would not is SEED state (a repo on the fake
// forge, a bare mirror, a work_branches row), exactly as
// acceptance_seed_test.go already seeds enrollment.
//
// work_branches.upstream_pr_number is no longer among that seeded state:
// the "an accepted work branch whose upstream PR has merged" fixture now
// runs the production accept engine (mirrorsync.StoreProposalAccepter,
// loam-giq.7) and lets it write the column, which is what makes the PR
// poller reachable through a real code path rather than a hand-written
// UPDATE.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/cucumber/godog"
	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/fakeforge"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/refnames"
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
	sc.Step(`^a branch prefixed "([^"]*)" is pushed to the upstream forge$`, h.stepABranchPrefixedIsPushedToTheUpstreamForge)
	sc.Step(`^the upstream PR is opened from that branch into "([^"]*)"$`, h.stepTheUpstreamPRIsOpenedFromThatBranchInto)
	sc.Step(`^the PR body is the work branch's description$`, h.stepThePRBodyIsTheWorkBranchsDescription)
	sc.Step(`^it ends with a footer attributing the PR to Loam$`, h.stepItEndsWithAFooterAttributingThePRToLoam)
	sc.Step(`^no agent identity appears in the body$`, h.stepNoAgentIdentityAppearsInTheBody)
	sc.Step(`^the server is configured without PR attribution$`, h.stepTheServerIsConfiguredWithoutPRAttribution)
	sc.Step(`^the PR body is the work branch's description alone$`, h.stepThePRBodyIsTheWorkBranchsDescriptionAlone)
	sc.Step(`^someone pushes a commit directly to the upstream "([^"]*)" branch$`, h.stepSomeonePushesDirectlyToTheUpstreamBranch)
	sc.Step(`^the upstream "([^"]*)" branch moves on while the work branch moves on separately$`, h.stepUpstreamAndWorkBranchMoveSeparately)
	sc.Step(`^the work branch advances to that commit$`, h.stepTheWorkBranchAdvancesToThatCommit)
	sc.Step(`^the work branch still holds the commit its author pushed$`, h.stepTheWorkBranchStillHoldsItsAuthorsCommit)
	sc.Step(`^a new review round is opened by the server$`, h.stepANewReviewRoundIsOpenedByTheServer)
	sc.Step(`^the accepted tip records that commit$`, h.stepTheAcceptedTipRecordsThatCommit)
	sc.Step(`^accepting it is refused until it is approved again$`, h.stepAcceptingIsRefusedUntilApprovedAgain)
	sc.Step(`^the admin sees the work branch flagged as diverged from upstream$`, h.stepTheAdminSeesTheBranchFlaggedAsDiverged)
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
	divergedSHA, err := commitIntoMirror(ctx, world.mirrorDir, refnames.TargetBranch(branch), "", "MIRROR-DIVERGENCE.txt", "written into the mirror only\n", "acceptance: diverge the mirror")
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
	world.setPrimaryWorkBranch(name)
	if err := h.insertWorkBranchRow(ctx, world.repoID, name, world.targetBranch, "draft", world.agentName); err != nil {
		return err
	}
	sha, err := commitIntoMirror(ctx, world.mirrorDir, refnames.WorkBranch(name), "", "WORK.txt", "work branch content\n", "acceptance: work branch commit")
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
	sha, err := mirrorRefSHA(world.mirrorDir, refnames.WorkBranch(name))
	if err != nil {
		return fmt.Errorf("reading mirror ref %s: %w", refnames.WorkBranch(name), err)
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
	if _, err := commitIntoMirror(ctx, world.mirrorDir, refnames.WorkBranch(world.workBranch), refnames.TargetBranch(world.targetBranch), "README.md", "work branch rewrite\n", "acceptance: work branch edit"); err != nil {
		return err
	}
	if _, err := commitIntoMirror(ctx, world.mirrorDir, refnames.TargetBranch(world.targetBranch), "", "README.md", "target rewrite\n", "acceptance: conflicting target edit"); err != nil {
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

// stepAnAcceptedWorkBranchWhosePRHasMerged reaches the state
// docs/sync-spec.md -> Proposal Acceptance leaves behind by RUNNING
// proposal acceptance: it seeds a reviewed work branch with a real commit
// in the mirror, then calls the production *mirrorsync.StoreProposalAccepter
// (loam-giq.7, wired in newAcceptanceAccepter exactly as
// cmd/server/sync.go's buildProposalAccepter wires it), which pushes
// loam/<name> upstream over the real transport, opens the PR on the fake
// forge, and writes upstream_pr_url/upstream_pr_number itself. Only then
// is that PR merged on the forge.
//
// It used to be three hand-written steps plus a direct UPDATE of
// upstream_pr_number, because nothing in the tree wrote that column and
// seeding it was the only way to reach StorePRPoller at all. That is no
// longer true, and the substitution matters beyond tidiness: this step is
// now the end-to-end proof that the accept engine's recorded column is the
// same column the PR poller's poll set is built from. A regression in
// either half breaks "The pushed branch is cleaned up after the PR ends".
//
// The precondition assertions are kept and tightened rather than dropped:
// a fixture whose upstream branch or PR number never materialised would
// make the scenario's later "the loam/ branch is removed" assertion pass
// vacuously.
func (h *acceptanceHarness) stepAnAcceptedWorkBranchWhosePRHasMerged(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	world.setPrimaryWorkBranch(world.workBranch)
	if err := h.insertWorkBranchRow(ctx, world.repoID, world.workBranch, world.targetBranch, "reviewed", world.agentName); err != nil {
		return err
	}
	if err := h.setWorkBranchTitleDescription(ctx, world.repoID, world.workBranch, "acceptance proposal", "acceptance proposal body"); err != nil {
		return err
	}
	sha, err := commitIntoMirror(ctx, world.mirrorDir, refnames.WorkBranch(world.workBranch), "", "PROPOSAL.txt", "proposed change\n", "acceptance: proposal commit")
	if err != nil {
		return err
	}
	world.workBranchSHA = sha
	result, err := h.accepter.AcceptProposal(ctx, mirrorsync.RepoID(world.repo()), world.workBranch)
	if err != nil {
		return fmt.Errorf("accepting proposal %s in repo %s: %w", world.workBranch, world.repo(), err)
	}
	if result.UpstreamBranch != "loam/"+world.workBranch {
		return fmt.Errorf("accept pushed %q, want %q", result.UpstreamBranch, "loam/"+world.workBranch)
	}
	upstreamSHA, err := h.upstreamRefSHA(ctx, world, "refs/heads/"+result.UpstreamBranch)
	if err != nil {
		return fmt.Errorf("upstream branch %s was never created, so its later absence would prove nothing: %w", result.UpstreamBranch, err)
	}
	if upstreamSHA != sha {
		return fmt.Errorf("upstream %s is at %s, want the work branch tip %s", result.UpstreamBranch, upstreamSHA, sha)
	}
	recorded, err := h.recordedUpstreamPRNumber(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if recorded != result.PRNumber {
		return fmt.Errorf("accept reported PR #%d but work_branches.upstream_pr_number holds #%d", result.PRNumber, recorded)
	}
	world.upstreamPRNumber = recorded
	if err := h.forge.MergePR(ctx, world.repo(), recorded); err != nil {
		return fmt.Errorf("merging upstream PR %s#%d: %w", world.repo(), recorded, err)
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

// stepABranchPrefixedIsPushedToTheUpstreamForge asserts loam-cgg's rewritten
// accept path really did push the work branch's own tip to a NAMESPACED
// upstream branch (docs/sync-spec.md's reserved "loam/" prefix), checked
// from two independent sources: the accept's own report
// (world.lastAcceptUpstreamBranch) and a live, authenticated ls-remote
// against the forge -- so a handler that only claimed to push, without the
// transport call actually landing the object upstream, fails here rather
// than passing on a value this harness merely cached.
//
// The upstream SHA is also compared against the mirror's OWN work-branch
// ref, read fresh rather than from a value latched before this accept: the
// tip StoreProposalAccepter records as accepted_tip is resolved
// immediately before the push (internal/mirrorsync/accepter.go, loam-cgg),
// so what landed upstream must be exactly what the mirror holds right now,
// not merely "some commit that used to be the tip".
func (h *acceptanceHarness) stepABranchPrefixedIsPushedToTheUpstreamForge(ctx context.Context, prefix string) error {
	world := worldFrom(ctx)
	want := prefix + world.workBranch
	if world.lastAcceptUpstreamBranch != want {
		return fmt.Errorf("the accept reported upstream branch %q, want the namespaced %q", world.lastAcceptUpstreamBranch, want)
	}
	upstreamSHA, err := h.upstreamRefSHA(ctx, world, "refs/heads/"+want)
	if err != nil {
		return fmt.Errorf("upstream branch %s was never created: %w", want, err)
	}
	mirrorSHA, err := mirrorRefSHA(world.mirrorDir, refnames.WorkBranch(world.workBranch))
	if err != nil {
		return fmt.Errorf("reading the mirror's %s: %w", refnames.WorkBranch(world.workBranch), err)
	}
	if upstreamSHA != mirrorSHA {
		return fmt.Errorf("upstream %s is at %s, want the work branch's own tip %s", want, upstreamSHA, mirrorSHA)
	}
	return nil
}

// stepTheUpstreamPRIsOpenedFromThatBranchInto asserts the forge holds
// exactly one open pull request (theOneUpstreamPR: the whole forge record,
// not merely what Loam believes it opened) from the namespaced branch this
// scenario's own prior step just proved was pushed, into target, and that
// its number matches the one recorded on the work branch row -- the same
// "read from the side that owns it" discipline
// stepAnUpstreamPRIsCreatedWithAGeneratedBranchName (admin-proposals.feature)
// applies, for this feature file's own wording.
func (h *acceptanceHarness) stepTheUpstreamPRIsOpenedFromThatBranchInto(ctx context.Context, target string) error {
	world := worldFrom(ctx)
	pr, err := h.theOneUpstreamPR(world)
	if err != nil {
		return err
	}
	want := "loam/" + world.workBranch
	if pr.HeadBranch != want || pr.TargetBranch != target || pr.State != "open" {
		return fmt.Errorf("the forge's pull request is %+v, want an open one from %s into %s", pr, want, target)
	}
	if pr.Number != world.upstreamPRNumber {
		return fmt.Errorf("the forge's pull request is #%d, but the work branch records #%d", pr.Number, world.upstreamPRNumber)
	}
	return nil
}

// stepThePRBodyIsTheWorkBranchsDescription asserts the upstream PR's body
// STARTS WITH the work branch's own description, read from the row rather
// than a harness literal. It is deliberately a prefix check, not equality:
// this feature file's next two steps ("it ends with a footer ..." and "no
// agent identity appears ...") are what pin the rest of the body down, so
// together the three steps make the same two-sided equality claim
// stepTheProposedTitleAndDescriptionAreTheBranchsOwn makes in one step for
// admin-proposals.feature -- split across sentences here because that is
// how this feature file's own Gherkin phrases it.
func (h *acceptanceHarness) stepThePRBodyIsTheWorkBranchsDescription(ctx context.Context) error {
	world := worldFrom(ctx)
	_, description, err := h.workBranchProposalText(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	pr, err := h.theOneUpstreamPR(world)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(pr.Description, description) {
		return fmt.Errorf("the upstream PR's body %q does not start with the work branch's own description %q", pr.Description, description)
	}
	return nil
}

// stepItEndsWithAFooterAttributingThePRToLoam asserts the body is EXACTLY
// the description plus a blank line plus acceptanceAttributionFooter --
// equality, not containment, so a footer buried mid-body or a body with
// trailing junk after it cannot pass.
func (h *acceptanceHarness) stepItEndsWithAFooterAttributingThePRToLoam(ctx context.Context) error {
	world := worldFrom(ctx)
	_, description, err := h.workBranchProposalText(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	pr, err := h.theOneUpstreamPR(world)
	if err != nil {
		return err
	}
	want := description + "\n\n" + acceptanceAttributionFooter
	if pr.Description != want {
		return fmt.Errorf("the upstream PR's body is %q, want %q", pr.Description, want)
	}
	return nil
}

// stepNoAgentIdentityAppearsInTheBody asserts the body names none of the
// identities this scenario actually used -- the author, the reviewer, the
// author's bare name, and the literal "admin" -- the same identity set
// stepTheProposedTitleAndDescriptionAreTheBranchsOwn checks, reproduced
// here rather than shared: docs/sync-spec.md's own claim ("agent
// attribution is already carried by the commit authors in git history") is
// this file's subject, not merely a side effect of the title/description
// check that file makes.
func (h *acceptanceHarness) stepNoAgentIdentityAppearsInTheBody(ctx context.Context) error {
	world := worldFrom(ctx)
	pr, err := h.theOneUpstreamPR(world)
	if err != nil {
		return err
	}
	for _, identity := range []string{world.author.identifier(), world.reviewer.identifier(), world.author.name, "admin"} {
		if strings.Contains(pr.Description, identity) {
			return fmt.Errorf("the upstream PR's body names %q; agent attribution belongs in the commit authors, not the PR body", identity)
		}
	}
	return nil
}

// stepTheServerIsConfiguredWithoutPRAttribution builds a scenario-scoped
// *mirrorsync.StoreProposalAccepter with attribution off and records it as
// world.accepterOverride, for acceptProposalForReal ("I accept it") to use
// INSTEAD of the harness's own whole-suite h.accepter.
//
// A per-scenario accepter, not a process-wide env flip, is the only safe
// way to reach this: TestFeatures builds ONE shared in-process server for
// the whole suite and reads LOAM_PR_ATTRIBUTION exactly once, before that
// server boots (acceptanceConfig) -- flipping it mid-suite would race every
// other scenario's own accept against whichever value happened to be set
// when its own tick ran. What this DOES exercise for real is the
// production knob itself: attribution is a plain bool captured at
// NewStoreProposalAccepter construction (see its own doc comment,
// "captured at construction rather than read per call"), and this
// constructs the exact same production type, over the same live pool and
// transport, differing only in that one bool -- precisely how
// cmd/server/sync.go's buildProposalAccepter would construct it if
// LOAM_PR_ATTRIBUTION were false.
func (h *acceptanceHarness) stepTheServerIsConfiguredWithoutPRAttribution(ctx context.Context) error {
	world := worldFrom(ctx)
	world.accepterOverride = newAcceptanceAccepterWithAttribution(h.server, h.transport, h.forgeClient, false)
	return nil
}

// stepThePRBodyIsTheWorkBranchsDescriptionAlone asserts the body is EXACTLY
// the work branch's own description, byte for byte -- no attribution
// footer, no trailing newline, nothing appended -- proving the
// accepterOverride built above (attribution off) actually reached
// mirrorsync.prBody rather than the footer merely being absent by
// coincidence.
func (h *acceptanceHarness) stepThePRBodyIsTheWorkBranchsDescriptionAlone(ctx context.Context) error {
	world := worldFrom(ctx)
	_, description, err := h.workBranchProposalText(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	pr, err := h.theOneUpstreamPR(world)
	if err != nil {
		return err
	}
	if pr.Description != description {
		return fmt.Errorf("the upstream PR's body is %q, want the work branch's own description %q alone (PR attribution is disabled)", pr.Description, description)
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

// setWorkBranchTitleDescription seeds the two columns proposal acceptance
// turns into the upstream PR's title and body. insertWorkBranchRow leaves
// both NULL (every other scenario in this suite is indifferent to them),
// but a proposal with no description would produce an empty PR body, so
// the accept fixture sets them explicitly.
func (h *acceptanceHarness) setWorkBranchTitleDescription(ctx context.Context, repoID uuid.UUID, name, title, description string) error {
	tag, err := h.server.pool.Exec(ctx,
		`UPDATE work_branches SET title = $1, description = $2 WHERE repo_id = $3 AND name = $4`, title, description, repoID, name)
	if err != nil {
		return fmt.Errorf("seeding title/description for work branch %s: %w", name, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("seeding title/description for work branch %s: %d rows updated, want 1", name, tag.RowsAffected())
	}
	return nil
}

// recordedUpstreamPRNumber reads work_branches.upstream_pr_number back
// off the row -- the column proposal acceptance writes and the PR poller's
// poll set is built from. It errors on SQL NULL rather than returning a
// zero, so an accept that pushed but never recorded cannot slip past as a
// PR "#0" the scenario would then fail on much further downstream.
func (h *acceptanceHarness) recordedUpstreamPRNumber(ctx context.Context, repoID uuid.UUID, name string) (int, error) {
	var number *int
	if err := h.server.pool.QueryRow(ctx,
		`SELECT upstream_pr_number FROM work_branches WHERE repo_id = $1 AND name = $2`, repoID, name).Scan(&number); err != nil {
		return 0, fmt.Errorf("reading upstream_pr_number for work branch %s: %w", name, err)
	}
	if number == nil {
		return 0, fmt.Errorf("work branch %s has no recorded upstream_pr_number after an accept", name)
	}
	return *number, nil
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
func commitIntoMirror(ctx context.Context, mirrorDir, ref, fromRef, filename, content, message string) (string, error) {
	work, err := os.MkdirTemp("", "loam-acceptance-mirror-commit-*")
	if err != nil {
		return "", fmt.Errorf("creating scratch clone dir: %w", err)
	}
	defer os.RemoveAll(work)
	clone := filepath.Join(work, "clone")
	if _, err := runIsolatedGit(ctx, work, "clone", "--quiet", mirrorDir, clone); err != nil {
		return "", fmt.Errorf("cloning mirror %s: %w", mirrorDir, err)
	}
	// A fixed local branch name, never ref's own: ref is a full path, and
	// a work branch's is refs/heads/loam-reserved/<name>, which is not a
	// legal local branch name to check out under. What the commit lands on
	// is decided by the push refspec below, not by what the scratch clone
	// happens to call its branch.
	checkout := []string{"checkout", "--quiet", "-B", "acceptance-scratch"}
	if fromRef != "" {
		checkout = append(checkout, "origin/"+strings.TrimPrefix(fromRef, "refs/heads/"))
	}
	if _, err := runIsolatedGit(ctx, clone, checkout...); err != nil {
		return "", fmt.Errorf("checking out %s: %w", ref, err)
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
	if _, err := runIsolatedGit(ctx, clone, "push", "--quiet", "--force", "origin", "HEAD:"+ref); err != nil {
		return "", fmt.Errorf("pushing %s into the mirror: %w", ref, err)
	}
	return mirrorRefSHA(mirrorDir, ref)
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

// --- Upstream drift on loam/<work-branch> (loam-giq.11) ---

// acceptanceDriftScratchRef is the mirror-local ref the drift fixtures
// build the "somebody else pushed" commit on before sending it upstream.
// It is a carrier, not a branch anything reads: the very next mirror fetch
// prunes it (nothing upstream matches it), while the commit itself stays
// reachable through refs/heads/loam/<name> once the forge has it.
const acceptanceDriftScratchRef = "refs/heads/acceptance-upstream-drift"

// stepSomeonePushesDirectlyToTheUpstreamBranch reproduces the reported
// incident (loam-giq.11) exactly: with the proposal accepted and
// loam/<name> sitting at the commit Loam pushed, a commit is added ON TOP
// of it, straight on the forge, without passing through Loam at all.
//
// The commit is built in the mirror and pushed over the SAME authenticated
// transport an admin's own git would use -- there is no back door into the
// fake forge here -- and the work branch's own ref is deliberately left
// where it is, which is what makes this a fast-forward rather than a
// divergence.
//
// Three preconditions are checked before the scenario continues, because
// each of them failing would make the later assertions pass vacuously: the
// forge really advertises the new commit, it really differs from what the
// accept recorded, and the work branch itself has NOT moved.
func (h *acceptanceHarness) stepSomeonePushesDirectlyToTheUpstreamBranch(ctx context.Context, prefix string) error {
	world := worldFrom(ctx)
	upstreamRef := "refs/heads/" + prefix + world.workBranch
	before, err := h.upstreamRefSHA(ctx, world, upstreamRef)
	if err != nil {
		return fmt.Errorf("this scenario needs an accepted proposal's upstream branch to push onto: %w", err)
	}
	pushed, err := h.pushCommitOntoUpstreamBranch(ctx, world, "UPSTREAM-EDIT.txt", "edited on the forge, behind loam's back\n", "acceptance: a commit pushed straight to loam/")
	if err != nil {
		return err
	}
	if pushed == before {
		return fmt.Errorf("upstream %s is still at %s, so nothing drifted", upstreamRef, before)
	}
	accepted, err := h.recordedAcceptedTip(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if pushed == accepted {
		return fmt.Errorf("the commit pushed upstream (%s) is the one loam recorded as accepted, so there is no drift to detect", pushed)
	}
	tip, err := mirrorRefSHA(world.mirrorDir, refnames.WorkBranch(world.workBranch))
	if err != nil {
		return err
	}
	if tip != accepted {
		return fmt.Errorf("the work branch is at %s but loam recorded %s as accepted; this fixture means to move ONLY the upstream branch", tip, accepted)
	}
	world.driftUpstreamSHA = pushed
	world.driftWorkTipSHA = tip
	return h.latchRoundBeforeTheTick(ctx, world)
}

// stepUpstreamAndWorkBranchMoveSeparately builds the genuinely diverged
// case: two DIFFERENT children of the commit Loam pushed, one landing on
// the forge's loam/<name> and one landing on the work branch, so neither
// tip contains the other and no fast-forward reconciles them.
//
// Both commits touch the same file for a reason: it makes the divergence a
// real one a human would have to resolve, not a pair of edits git could
// merge without a decision. Loam does not attempt either, but a fixture
// whose two sides trivially merged would be arguing about a case the spec
// is not about.
func (h *acceptanceHarness) stepUpstreamAndWorkBranchMoveSeparately(ctx context.Context, prefix string) error {
	world := worldFrom(ctx)
	accepted, err := h.recordedAcceptedTip(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	upstreamSHA, err := h.pushCommitOntoUpstreamBranch(ctx, world, "DIVERGENCE.txt", "the forge's version\n", "acceptance: the forge's side of the divergence")
	if err != nil {
		return err
	}
	workSHA, err := commitIntoMirror(ctx, world.mirrorDir, refnames.WorkBranch(world.workBranch), refnames.WorkBranch(world.workBranch),
		"DIVERGENCE.txt", "the author's version\n", "acceptance: the author's side of the divergence")
	if err != nil {
		return err
	}
	if upstreamSHA == workSHA {
		return fmt.Errorf("both sides landed on %s, so nothing diverged", upstreamSHA)
	}
	if upstreamSHA == accepted || workSHA == accepted {
		return fmt.Errorf("one side is still at the accepted tip %s (upstream %s, work branch %s); a diverged fixture needs both to have moved", accepted, upstreamSHA, workSHA)
	}
	world.driftUpstreamSHA = upstreamSHA
	world.driftWorkTipSHA = workSHA
	return h.latchRoundBeforeTheTick(ctx, world)
}

// stepTheWorkBranchAdvancesToThatCommit is the adoption itself, read off
// the mirror rather than off any value this harness cached: the work
// branch's ref must now BE the commit that arrived upstream, and it must
// have moved to get there (the pre-tick tip is asserted separately, so a
// tick that did nothing cannot pass this step).
func (h *acceptanceHarness) stepTheWorkBranchAdvancesToThatCommit(ctx context.Context) error {
	world := worldFrom(ctx)
	tip, err := mirrorRefSHA(world.mirrorDir, refnames.WorkBranch(world.workBranch))
	if err != nil {
		return err
	}
	if tip == world.driftWorkTipSHA {
		return fmt.Errorf("the work branch is still at %s; the upstream commit %s was not adopted", tip, world.driftUpstreamSHA)
	}
	if tip != world.driftUpstreamSHA {
		return fmt.Errorf("the work branch is at %s, want the adopted upstream commit %s", tip, world.driftUpstreamSHA)
	}
	return nil
}

// stepTheWorkBranchStillHoldsItsAuthorsCommit is the diverged case's
// no-op proof. Loam changes nothing: it cannot know which side is
// intended, and the push that would reconcile it is precisely the
// destructive one Proposal Acceptance refuses to make.
func (h *acceptanceHarness) stepTheWorkBranchStillHoldsItsAuthorsCommit(ctx context.Context) error {
	world := worldFrom(ctx)
	tip, err := mirrorRefSHA(world.mirrorDir, refnames.WorkBranch(world.workBranch))
	if err != nil {
		return err
	}
	if tip != world.driftWorkTipSHA {
		return fmt.Errorf("the work branch moved to %s, want the author's own commit %s left exactly where it was", tip, world.driftWorkTipSHA)
	}
	return nil
}

// stepANewReviewRoundIsOpenedByTheServer proves the approvals reset, in
// the terms this system already expresses staleness in: a new numbered
// round, attributed to "server" -- the same attribution a catch-up restore
// uses, and one no agent and no admin can produce.
func (h *acceptanceHarness) stepANewReviewRoundIsOpenedByTheServer(ctx context.Context) error {
	world := worldFrom(ctx)
	number, requestedBy, err := h.latestRound(ctx, world, world.workBranch)
	if err != nil {
		return err
	}
	if number <= world.driftRoundBefore {
		return fmt.Errorf("the work branch is still on review round %d; adopting an unreviewed commit must open a new one", number)
	}
	if requestedBy != "server" {
		return fmt.Errorf("review round %d was requested by %q, want %q -- nobody asked for this round, the server opened it", number, requestedBy, "server")
	}
	return nil
}

// stepTheAcceptedTipRecordsThatCommit pins the settled answer to "what
// should accepted_tip become after an adoption": the adopted commit, so
// the column keeps meaning "the upstream tip Loam last pushed OR adopted"
// and ListProposals stays exact -- nothing remains to push.
func (h *acceptanceHarness) stepTheAcceptedTipRecordsThatCommit(ctx context.Context) error {
	world := worldFrom(ctx)
	accepted, err := h.recordedAcceptedTip(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if accepted != world.driftUpstreamSHA {
		return fmt.Errorf("work_branches.accepted_tip holds %s, want the adopted commit %s", accepted, world.driftUpstreamSHA)
	}
	return nil
}

// stepAcceptingIsRefusedUntilApprovedAgain is the whole point of the
// reset, asserted as a difference rather than as a state: the SAME accept,
// by the same admin, on the same branch, is refused before a fresh
// approval and succeeds after one.
//
// Without both halves this proves much less than it appears to. The refusal
// alone would also pass if adoption had simply broken the branch, and the
// success alone would also pass if nothing had been reset.
func (h *acceptanceHarness) stepAcceptingIsRefusedUntilApprovedAgain(ctx context.Context) error {
	world := worldFrom(ctx)
	client := h.newProposalServiceClient()
	_, err := acceptProposal(ctx, client, world.repo(), world.workBranch)
	if rejectErr := requireRPCRejected(err, "accepting a branch whose approvals were just reset", connect.CodeFailedPrecondition); rejectErr != nil {
		return rejectErr
	}
	if _, err := h.submitVerdict(world, world.reviewer, world.workBranch, "approve"); err != nil {
		return fmt.Errorf("re-approving the adopted commit: %w", err)
	}
	if _, err := acceptProposal(ctx, client, world.repo(), world.workBranch); err != nil {
		return fmt.Errorf("accepting after a fresh approval must succeed, or the reset is a dead end: %w", err)
	}
	return nil
}

// stepTheAdminSeesTheBranchFlaggedAsDiverged reads the flag back the way
// the admin console does -- one GetWorkBranch over loam.v1 as a superuser,
// the same call the proposal detail page makes -- not out of the database.
// Surfacing this state through the protos IS half of loam-giq.11; a
// scenario that asserted the column would pass just as happily with the
// field left off the wire.
//
// The conflict field is asserted too, and asserted to be NONE: the two are
// separate fields precisely because they mean different things and call for
// different remedies, so a console must be able to see drift on a branch
// that merges into its target perfectly well.
func (h *acceptanceHarness) stepTheAdminSeesTheBranchFlaggedAsDiverged(ctx context.Context) error {
	world := worldFrom(ctx)
	resp, err := h.newWorkBranchServiceClient().GetWorkBranch(ctx, connect.NewRequest(&loamv1.GetWorkBranchRequest{
		Repo:       world.repo(),
		WorkBranch: world.workBranch,
	}))
	if err != nil {
		return fmt.Errorf("reading work branch %s as the admin: %w", world.workBranch, err)
	}
	wb := resp.Msg.GetWorkBranch()
	if wb.GetUpstreamDrift() != loamv1.UpstreamDrift_UPSTREAM_DRIFT_DIVERGED {
		return fmt.Errorf("the admin sees upstream_drift %s on %s, want UPSTREAM_DRIFT_DIVERGED", wb.GetUpstreamDrift(), world.workBranch)
	}
	if wb.GetConflict() != loamv1.WorkBranchConflict_WORK_BRANCH_CONFLICT_NONE {
		return fmt.Errorf("the admin sees conflict %s on %s, want WORK_BRANCH_CONFLICT_NONE -- this branch merges into its target fine; what moved is the branch loam pushed", wb.GetConflict(), world.workBranch)
	}
	return nil
}

// pushCommitOntoUpstreamBranch builds one commit on top of the work
// branch's CURRENT upstream branch content and pushes it to the forge's
// refs/heads/loam/<name>, over the harness's authenticated transport.
//
// The commit is forked from the mirror's own work-branch ref rather than
// from the mirrored loam/<name>: at this point in a scenario the mirror has
// not fetched since the accept, so it holds no loam/<name> ref at all --
// the accept pushed that branch UP, it does not come back down until the
// next sync. The two are the same commit at that moment, which is what
// makes forking from the reachable one correct rather than merely
// convenient.
func (h *acceptanceHarness) pushCommitOntoUpstreamBranch(ctx context.Context, world *acceptanceWorld, filename, content, message string) (string, error) {
	sha, err := commitIntoMirror(ctx, world.mirrorDir, acceptanceDriftScratchRef, refnames.WorkBranch(world.workBranch), filename, content, message)
	if err != nil {
		return "", err
	}
	refspec := acceptanceDriftScratchRef + ":" + refnames.UpstreamProposalBranch(world.workBranch)
	if _, err := h.transport.Push(ctx, h.forgeHost, world.mirrorDir, world.upstreamURL, refspec); err != nil {
		return "", fmt.Errorf("pushing %s to the forge: %w", refspec, err)
	}
	upstreamSHA, err := h.upstreamRefSHA(ctx, world, refnames.UpstreamProposalBranch(world.workBranch))
	if err != nil {
		return "", err
	}
	if upstreamSHA != sha {
		return "", fmt.Errorf("the forge advertises %s for %s after the push, want the pushed commit %s", upstreamSHA, refnames.UpstreamProposalBranch(world.workBranch), sha)
	}
	return sha, nil
}

// latchRoundBeforeTheTick records the branch's current review round so the
// Then that asserts a NEW one has something earlier to compare against --
// the same discipline stepMirrorHasDiverged applies to SHAs (loam-8vg).
func (h *acceptanceHarness) latchRoundBeforeTheTick(ctx context.Context, world *acceptanceWorld) error {
	number, _, err := h.latestRound(ctx, world, world.workBranch)
	if err != nil {
		return err
	}
	world.driftRoundBefore = number
	return nil
}

// recordedAcceptedTip reads work_branches.accepted_tip back off the row --
// what proposal acceptance pushed, or what the drift reconciler adopted.
// A SQL NULL is an error rather than an empty string: every caller here is
// asserting about a branch that has definitely been accepted, so a NULL
// means the fixture never got that far.
func (h *acceptanceHarness) recordedAcceptedTip(ctx context.Context, repoID uuid.UUID, name string) (string, error) {
	var tip *string
	if err := h.server.pool.QueryRow(ctx,
		`SELECT accepted_tip FROM work_branches WHERE repo_id = $1 AND name = $2`, repoID, name).Scan(&tip); err != nil {
		return "", fmt.Errorf("reading accepted_tip for work branch %s: %w", name, err)
	}
	if tip == nil {
		return "", fmt.Errorf("work branch %s has no recorded accepted_tip", name)
	}
	return *tip, nil
}
