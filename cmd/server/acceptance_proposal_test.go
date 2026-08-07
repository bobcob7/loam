//go:build acceptance

// Step definitions for features/admin-proposals.feature (loam-tb8h): the
// admin's decision loop -- the proposal queue, AcceptProposal, the close,
// and the conflict/catch-up/re-accept arc that ends in ONE upstream pull
// request.
//
// # Which forge this exercises, and why that is a choice
//
// The whole suite substitutes a *fakeforge.Client for the entire
// forge.Provider seam (acceptance_harness_test.go), so the production
// *forge.Forgejo REST client is not on any path here. That is deliberate
// and is the right seam for THIS layer: every claim these scenarios make
// -- "an upstream PR is created with a generated branch name", "the
// existing upstream PR is updated in place", "no new upstream PR is
// created" -- is a claim about what internal/mirrorsync's accept engine
// DECIDES to do, not about how that decision is encoded on the wire.
// Provider-level parity between the fake and a real Forgejo is pinned
// separately, by ONE shared contract suite run against both legs
// (internal/forgesuite), and Forgejo's actual request/response encoding is
// exercised end to end by Taskfile.yml's demo:m5, which drives the shipped
// binary through cmd/demoenv's Forgejo-REST translator. Neither of those
// belongs in a Gherkin scenario; duplicating them here would only make
// these steps slower and no more truthful.
//
// The consequence of that seam for the AcceptProposal RPC specifically --
// which half of it these scenarios reach, which half they cannot, and
// which scenario is still @wip because of it -- is set out in full on
// acceptProposalForReal below. Read that before adding a step here that
// means to observe anything Loam sends a forge.
//
// # Everything below reaches its fixture state through production code
//
// No step in this file writes work_branches.state, .conflict,
// .upstream_pr_number or .upstream_pr_url. A branch becomes reviewed by
// being sent for review and receiving a real verdict through the CLI; it
// becomes conflicted because a genuine upstream advance rewrote the same
// file the branch did and the production mergeability checker found it on
// a real sync tick; it gains a PR because the production accept engine
// pushed and opened one; it is caught up because the author cloned,
// merged, resolved and PUSHED through /git/* and the real pre-receive
// hook, which is the only thing that ever runs internal/catchup at all.
// The one direct SQL this file does is READ-only, plus the plain row
// INSERT acceptance_seed_test.go already establishes as this suite's
// enrollment fixture technique.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"github.com/cucumber/godog"
	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/forge"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/refnames"
)

// acceptanceProposalReviewerID is the reviewer identity every proposal
// fixture in this file votes as. admin-proposals.feature never names one
// -- its Background establishes only the admin and the enrolled repo --
// but a proposal is by definition a branch somebody approved, so the
// verdicts have to come from somewhere. Naming the identity here, and
// recording it as world.reviewer, is also what makes the shared "the work
// branch stays in state ..." assertion (acceptance_review_test.go, which
// reads back as world.reviewer) reachable from this feature file at all.
const acceptanceProposalReviewerID = "katherine-johnson-5-reviewer"

// acceptanceProposalFile is the one path every fixture in this file
// rewrites: the proposal's own commit rewrites it on the work branch, the
// conflicting target advance rewrites it upstream, and the catch-up merge
// reconciles the two. Using a single file is what makes the conflict
// certain rather than incidental -- the two sides edit the same lines of
// the same path, from a shared merge base.
const acceptanceProposalFile = "README.md"

// The three contents that file takes. They are distinct, non-overlapping
// whole-file rewrites so `git merge` cannot resolve them automatically:
// a fixture edit that made them auto-mergeable would silently turn every
// conflict scenario in this file into a no-op, which is why the catch-up
// step asserts the merge really did fail before resolving it.
const (
	acceptanceAdvancedTargetReadme = "# The target's README, rewritten upstream by a conflicting advance\n"
	acceptanceCaughtUpReadme       = "# The README, reconciled by the author's catch-up merge\n"
)

// acceptanceProposalReadme is the work branch's own rewrite of that file.
// It names the branch so two branches in one scenario (the queue
// scenario's approved and disapproved pair) never write identical trees.
func acceptanceProposalReadme(workBranch string) string {
	return "# The proposal's README, rewritten on " + workBranch + "\n"
}

// acceptanceProposalTitle/Description are the work branch's own title and
// description -- the two values proposal acceptance turns into the
// upstream pull request's title and body. They are branch-specific so
// "the proposed title and description are the work branch's own" is
// falsifiable: a PR carrying some other branch's text, or a harness
// literal, reads as a mismatch rather than as a pass.
func acceptanceProposalTitle(workBranch string) string {
	return "Acceptance proposal on " + workBranch
}

func acceptanceProposalDescription(workBranch string) string {
	return "The proposed change on " + workBranch + ", described by its author so the upstream pull request has a body of its own."
}

// acceptanceAttributionFooter is the footer docs/sync-spec.md specifies
// for an upstream PR body, reproduced verbatim rather than imported:
// internal/mirrorsync's own constant is unexported, and a test that read
// the production value would assert the body equals itself. demo:m5's
// check-prs carries the same literal for the same reason.
const acceptanceAttributionFooter = "---\nProposed via Loam."

// acceptanceCloseReason is the body the close RPC sends, read back by
// "the reason is recorded on the work branch".
const acceptanceCloseReason = "Closing this from the acceptance suite: superseded by other work."

// registerProposalSteps wires every step features/admin-proposals.feature
// needs beyond the ones it shares with the rest of the suite. Six of its
// sentences are NOT registered here, because they already exist and mean
// exactly the same thing: "I am signed in to the web interface as the
// admin" and "the work branch is in state ..." (acceptance_sync_test.go);
// "I accept it", the core step-vocabulary row (acceptance_steps_test.go);
// and "a work branch in state ...", "the work branch stays in state ..."
// and "the attempt is rejected as a failed precondition"
// (acceptance_review_test.go -- the last of which grew a second arm for
// this file's two refused RPCs).
func (h *acceptanceHarness) registerProposalSteps(sc *godog.ScenarioContext) {
	sc.Step(`^a work branch in state "([^"]*)" with one "([^"]*)" verdict$`, h.stepAWorkBranchInStateWithOneVerdict)
	sc.Step(`^a work branch in state "([^"]*)" with only a "([^"]*)" verdict$`, h.stepAWorkBranchInStateWithOneVerdict)
	sc.Step(`^a proposal in state "([^"]*)" with one "([^"]*)" verdict$`, h.stepAProposalInStateWithOneVerdict)
	sc.Step(`^a work branch flagged as conflicted with its target$`, h.stepAWorkBranchFlaggedAsConflicted)
	sc.Step(`^it is in state "([^"]*)" with one "([^"]*)" verdict$`, h.stepItIsInStateWithOneVerdict)
	sc.Step(`^a work branch in state "([^"]*)" whose upstream PR has been created$`, h.stepAWorkBranchInStateWhoseUpstreamPRExists)
	sc.Step(`^a work branch whose upstream PR has been created$`, h.stepAWorkBranchWhoseUpstreamPRExists)
	sc.Step(`^a conflicting target advance reset it to "([^"]*)"$`, h.stepAConflictingTargetAdvanceResetIt)
	sc.Step(`^it was caught up, re-reviewed, and approved again$`, h.stepItWasCaughtUpReReviewedAndApprovedAgain)
	sc.Step(`^I open the proposal queue$`, h.stepIOpenTheProposalQueue)
	sc.Step(`^I try to accept it$`, h.stepITryToAcceptIt)
	sc.Step(`^I request a re-review$`, h.stepIRequestAReReview)
	sc.Step(`^I close it with a reason$`, h.stepICloseItWithAReason)
	sc.Step(`^the upstream PR is closed without merging$`, h.stepTheUpstreamPRIsClosedWithoutMerging)
	sc.Step(`^its target branch advances with conflicting changes$`, h.stepItsTargetBranchAdvancesWithConflictingChanges)
	sc.Step(`^the approved work branch is listed$`, h.stepTheApprovedWorkBranchIsListed)
	sc.Step(`^the disapproved work branch is not listed$`, h.stepTheDisapprovedWorkBranchIsNotListed)
	sc.Step(`^each listed proposal shows its verdicts$`, h.stepEachListedProposalShowsItsVerdicts)
	sc.Step(`^an upstream PR is created with a generated branch name$`, h.stepAnUpstreamPRIsCreatedWithAGeneratedBranchName)
	sc.Step(`^the proposed title and description are the work branch's own$`, h.stepTheProposedTitleAndDescriptionAreTheBranchsOwn)
	sc.Step(`^the upstream PR URL is recorded on the work branch$`, h.stepTheUpstreamPRURLIsRecorded)
	sc.Step(`^a new review round is opened$`, h.stepANewReviewRoundIsOpened)
	sc.Step(`^the prior verdicts are marked stale$`, h.stepThePriorVerdictsAreMarkedStale)
	sc.Step(`^its prior verdicts are not yet stale, because no new round has opened$`, h.stepPriorVerdictsAreNotYetStale)
	sc.Step(`^it is flagged as conflicted$`, h.stepItIsFlaggedAsConflicted)
	sc.Step(`^the target branch advances with conflicting changes$`, h.stepTheTargetAdvancesWithConflictingChanges)
	sc.Step(`^it no longer appears in the proposal queue$`, h.stepItNoLongerAppearsInTheProposalQueue)
	sc.Step(`^it is still listed in the proposal queue, marked as not acceptable$`, h.stepItIsStillListedMarkedNotAcceptable)
	sc.Step(`^accepting it is still rejected as a failed precondition$`, h.stepAcceptingItIsStillRejected)
	sc.Step(`^the reason is recorded on the work branch$`, h.stepTheReasonIsRecordedOnTheWorkBranch)
	sc.Step(`^the upstream PR is closed$`, h.stepTheUpstreamPRIsClosed)
	sc.Step(`^the next sync sets the work branch to state "([^"]*)"$`, h.stepTheNextSyncSetsTheWorkBranchToState)
	sc.Step(`^the existing upstream PR is updated in place$`, h.stepTheExistingUpstreamPRIsUpdatedInPlace)
	sc.Step(`^no new upstream PR is created$`, h.stepNoNewUpstreamPRIsCreated)
	h.registerWorkBranchLifecycleSteps(sc)
}

// registerWorkBranchLifecycleSteps wires every step
// features/work-branch-lifecycle.feature needs that is not already shared
// with the rest of this suite -- all 8 of its scenarios that this file's
// own NOTES on loam-ofg.8 records were provisionally closed under a
// DEFERRED-WIP claim ("functionally covered by tests") that held for their
// UNIT coverage and not for this acceptance layer: the loam-9wr @wip sweep
// found every one of them failing at godog.ErrPending, because no step
// definition for them existed anywhere in cmd/server. This is that missing
// wiring: starting a work branch, refusing review without a title and
// description, opening review for real, editing title/description in
// place, the first verdict's reviewable -> reviewed flip, the absence of
// any author-facing completion action, a terminal branch refusing an edit,
// and a clean (non-conflicting) target advance leaving a reviewable branch
// untouched -- plus the four scenarios this file already covered before
// them, whose subject is the proposal loop this file implements: a
// reviewed+approved branch becoming a proposal, a re-review sending it
// back, completion happening only on an upstream merge, and a catch-up
// push returning a conflict-reset branch to review.
//
// Several of its sentences are near-twins of admin-proposals.feature's (or
// of this file's own) and are deliberately registered separately rather
// than by widening a regex: "the target branch advances ..." vs "ITS
// target branch advances ..." vs "... advances with changes that merge
// CLEANLY", "it no longer appears in the ADMIN'S proposal queue" vs "... in
// the proposal queue", "the work branch KEEPS its state ..." vs "IT keeps
// its state ...". godog dispatches on the whole sentence, so a shared
// definition would mean rewriting one feature file's prose to match
// another's -- which is the tail wagging the dog.
func (h *acceptanceHarness) registerWorkBranchLifecycleSteps(sc *godog.ScenarioContext) {
	sc.Step(`^it appears in the admin's proposal queue$`, h.stepItAppearsInTheAdminsProposalQueue)
	sc.Step(`^I request review again$`, h.stepIRequestReviewAgain)
	sc.Step(`^it no longer appears in the admin's proposal queue$`, h.stepItNoLongerAppearsInTheProposalQueue)
	sc.Step(`^a work branch reset to "([^"]*)" by a conflicting target advance$`, h.stepAWorkBranchResetByAConflictingTargetAdvance)
	sc.Step(`^I push commits that bring it up to date with its target$`, h.stepIPushCommitsThatBringItUpToDate)
	sc.Step(`^no request for review was needed$`, h.stepNoRequestForReviewWasNeeded)
	sc.Step(`^I start a work branch from "([^"]*)"$`, h.stepIStartAWorkBranchFrom)
	sc.Step(`^a work branch is created in state "([^"]*)"$`, h.stepAWorkBranchIsCreatedInState)
	sc.Step(`^its name is randomly generated$`, h.stepItsNameIsRandomlyGenerated)
	sc.Step(`^I have started a work branch with no title or description$`, h.stepIHaveStartedAWorkBranchWithNoTitleOrDescription)
	sc.Step(`^the request is rejected with a precondition error$`, h.stepTheRequestIsRejectedWithAPreconditionError)
	sc.Step(`^I have started a work branch with a title and description$`, h.stepIHaveStartedAWorkBranchWithATitleAndDescription)
	sc.Step(`^I update its title and description$`, h.stepIUpdateItsTitleAndDescription)
	sc.Step(`^the work branch keeps its state "([^"]*)"$`, h.stepTheWorkBranchKeepsItsState)
	sc.Step(`^the new title and description are shown$`, h.stepTheNewTitleAndDescriptionAreShown)
	sc.Step(`^the reviewer "([^"]*)" submits an "([^"]*)" verdict$`, h.stepTheReviewerSubmitsAVerdict)
	sc.Step(`^there is no author action that sets it "([^"]*)"$`, h.stepThereIsNoAuthorActionThatSetsIt)
	sc.Step(`^I try to update its title$`, h.stepITryToUpdateItsTitle)
	sc.Step(`^the target branch advances with changes that merge cleanly$`, h.stepTheTargetBranchAdvancesWithChangesThatMergeCleanly)
	sc.Step(`^the work branch's commits are unchanged$`, h.stepTheWorkBranchsCommitsAreUnchanged)
	sc.Step(`^it keeps its state "([^"]*)"$`, h.stepItKeepsItsState)
}

// --- features/work-branch-lifecycle.feature ---

// stepItAppearsInTheAdminsProposalQueue is that file's own wording for
// "this reviewed, approved branch satisfies ProposalService's predicate".
func (h *acceptanceHarness) stepItAppearsInTheAdminsProposalQueue(ctx context.Context) error {
	return h.requireQueued(ctx, worldFrom(ctx))
}

// stepIRequestReviewAgain is the AUTHOR sending their own branch back for
// another round -- the same transition admin-proposals.feature reaches
// through the admin (stepIRequestAReReview), driven here through the CLI
// as the author, which is who that feature file's Background establishes.
//
// It snapshots the same two things that step does, and for the same
// reasons, plus the queue membership the scenario's closing assertion
// depends on: "it no longer appears in the admin's proposal queue" can
// only be a removal if the branch was queued to begin with.
func (h *acceptanceHarness) stepIRequestReviewAgain(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := h.requireQueued(ctx, world); err != nil {
		return err
	}
	before, _, err := h.latestRound(ctx, world, world.workBranch)
	if err != nil {
		return err
	}
	world.roundBefore = before
	verdicts, err := h.listVerdicts(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if len(verdicts) == 0 {
		return fmt.Errorf("work branch %s carries no verdicts before the new round, so \"the prior verdicts are marked stale\" would hold vacuously", world.workBranch)
	}
	world.verdictsBefore = verdicts
	return h.requestReview(world, world.author, world.workBranch)
}

// stepAWorkBranchResetByAConflictingTargetAdvance seeds a reviewed,
// approved branch and then lets a genuine conflicting advance demote it,
// asserting both halves of the demotion. It also records how many
// request-review calls this suite had made by that point, which is what
// lets "no request for review was needed" be a claim about this harness's
// own behaviour rather than only about the row it produced.
func (h *acceptanceHarness) stepAWorkBranchResetByAConflictingTargetAdvance(ctx context.Context, want string) error {
	world := worldFrom(ctx)
	if _, err := h.seedReviewedWorkBranch(ctx, world, "reviewed", "approve"); err != nil {
		return err
	}
	if err := h.stepAConflictingTargetAdvanceResetIt(ctx, want); err != nil {
		return err
	}
	world.requestReviewsAtReset = world.requestReviews
	return nil
}

// stepIPushCommitsThatBringItUpToDate is the author's real catch-up push.
func (h *acceptanceHarness) stepIPushCommitsThatBringItUpToDate(ctx context.Context) error {
	return h.catchUpWorkBranch(ctx, worldFrom(ctx))
}

// stepNoRequestForReviewWasNeeded asserts the restore to reviewable was
// the SERVER's doing, from both directions at once:
//
//   - the new round records requested_by "server", internal/catchup's own
//     sentinel, which is deliberately distinct from every agent identifier
//     and from the admin's "admin" -- so a round an author or an admin
//     asked for cannot satisfy this;
//   - and no request-review call was made after the reset, counted by this
//     harness itself, so the assertion is not merely about what the row
//     says but about what was actually done to reach it.
//
// Either half alone would be weak: the first would pass if the harness had
// called request-review and the server had happened to overwrite the
// actor, the second would pass if no round had been opened at all.
func (h *acceptanceHarness) stepNoRequestForReviewWasNeeded(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.requestReviews != world.requestReviewsAtReset {
		return fmt.Errorf("%d request-review call(s) were made after the reset; the catch-up was supposed to need none",
			world.requestReviews-world.requestReviewsAtReset)
	}
	number, requestedBy, err := h.latestRound(ctx, world, world.workBranch)
	if err != nil {
		return err
	}
	if number != 2 {
		return fmt.Errorf("the restored branch is on round %d, want the second round the catch-up opened", number)
	}
	if requestedBy != "server" {
		return fmt.Errorf("round %d was requested by %q, want the server's own \"server\" -- a round opened by anyone else means a review WAS requested", number, requestedBy)
	}
	return nil
}

// acceptanceLifecycleUpdatedTitle/Description are the values "I update its
// title and description" sets on an already-titled, already-described
// reviewable branch (its Given reaches "reviewable" through
// acceptance_review_test.go's seedWorkBranchToState, which sets its own
// fixture title/description via setTitleAndDescription). They are
// deliberately distinct from that fixture text, so "the new title and
// description are shown" is a claim about a genuine REPLACEMENT, not merely
// that some title happens to exist.
const acceptanceLifecycleUpdatedTitle = "Updated while work progresses"
const acceptanceLifecycleUpdatedDescription = "The description as it reads after the author updated it mid-review, replacing the fixture text seedWorkBranchToState used to reach reviewable."

// acceptanceCleanAdvanceFile/Content is the target-side edit "the target
// branch advances with changes that merge cleanly" makes; acceptanceCleanWorkBranchFile
// is the work branch's OWN file for that same scenario. The two paths are
// deliberately distinct, so the two sides' whole-file additions from a
// shared base cannot help but merge cleanly -- a future edit that made them
// overlap would turn this scenario into the conflicting-advance one it
// exists to be distinguished from, not silently keep passing.
const acceptanceCleanAdvanceFile = "CLEAN-ADVANCE.md"
const acceptanceCleanAdvanceContent = "# A clean, non-conflicting target advance\n"
const acceptanceCleanWorkBranchFile = "WORK-BRANCH-OWN-FILE.md"

// acceptanceRandomWorkBranchNamePattern is the exact shape
// internal/handler/workbranch.randomWorkBranchName produces: "wb-" plus 3
// random bytes, hex-encoded.
var acceptanceRandomWorkBranchNamePattern = regexp.MustCompile(`^wb-[0-9a-f]{6}$`)

// stepIStartAWorkBranchFrom is "When I start a work branch from ...":
// CreateWorkBranch for real, as the scenario's own author, against a
// mirror this step builds first -- unlike every OTHER scenario in this
// file, this one's Given is nothing (its Background only enrolls the repo),
// so no earlier step has given it one.
func (h *acceptanceHarness) stepIStartAWorkBranchFrom(ctx context.Context, from string) error {
	world := worldFrom(ctx)
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	before := world.workBranch
	res := h.runLoamAs(world, world.author, "", "work", "start", world.repo(), from)
	world.lastCLI = res
	if err := requireLoamOK(res, fmt.Sprintf("loam work start (as %s)", world.author.identifier())); err != nil {
		return err
	}
	var out acceptanceWorkBranchOutput
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		return fmt.Errorf("decoding loam work start JSON output: %w\nstdout: %s", err, res.stdout)
	}
	if out.Name == "" || out.Name == before {
		return fmt.Errorf("loam work start reported name %q (this scenario's own unstarted default %q); want a freshly generated one", out.Name, before)
	}
	world.lastWorkBranch = out
	world.setPrimaryWorkBranch(out.Name)
	return nil
}

// stepAWorkBranchIsCreatedInState checks both halves of CreateWorkBranch's
// own response: what the RPC just echoed back, and what actually landed in
// the row -- so a handler that reported the right state without persisting
// it (or vice versa) fails here rather than passing on whichever half this
// step happened to trust.
func (h *acceptanceHarness) stepAWorkBranchIsCreatedInState(ctx context.Context, want string) error {
	world := worldFrom(ctx)
	if world.lastWorkBranch.Name == "" {
		return fmt.Errorf("no work branch was started in this scenario, so \"a work branch is created in state %q\" has nothing to check", want)
	}
	if world.lastWorkBranch.State != want {
		return fmt.Errorf("loam work start reported state %q, want %q", world.lastWorkBranch.State, want)
	}
	return h.requireWorkBranchState(ctx, world, world.workBranch, want)
}

// stepItsNameIsRandomlyGenerated asserts the name CreateWorkBranch reported
// matches the server's own random-name shape AND differs from this
// scenario's own pre-assigned default (acceptanceWorld's deterministic
// "wb-<n>") -- shape alone would also accept a handler that always
// returned some fixed, correctly-shaped literal.
func (h *acceptanceHarness) stepItsNameIsRandomlyGenerated(ctx context.Context) error {
	world := worldFrom(ctx)
	if !acceptanceRandomWorkBranchNamePattern.MatchString(world.workBranch) {
		return fmt.Errorf("work branch name %q does not match the server's randomly generated shape wb-<6 hex characters>", world.workBranch)
	}
	return nil
}

// stepIHaveStartedAWorkBranchWithNoTitleOrDescription seeds a draft work
// branch with neither field set -- RequestReview's own precondition
// (docs/cli-spec.md -> "request-review": requires title + description to
// be set) -- through the same direct-row technique every Given in this
// suite that needs no real git presence uses.
func (h *acceptanceHarness) stepIHaveStartedAWorkBranchWithNoTitleOrDescription(ctx context.Context) error {
	world := worldFrom(ctx)
	name := world.claimWorkBranch()
	return h.insertWorkBranchRow(ctx, world.repoID, name, world.targetBranch, "draft", world.author.identifier())
}

// stepTheRequestIsRejectedWithAPreconditionError is this feature file's own
// wording for a request-review refusal -- the CLI-driven twin of
// acceptance_review_test.go's RPC-agnostic
// stepTheAttemptIsRejectedAsFailedPrecondition, reading the same
// world.lastCLI stepIRequestReview (acceptance_steps_test.go) leaves behind
// on a non-zero exit.
func (h *acceptanceHarness) stepTheRequestIsRejectedWithAPreconditionError(ctx context.Context) error {
	return requireLoamRejected(worldFrom(ctx).lastCLI, "loam work request-review", "precondition_failed", 2)
}

// stepIHaveStartedAWorkBranchWithATitleAndDescription is the Given
// request-review actually needs to succeed: a draft branch carrying both
// fields, set through `loam work set` for real (setTitleAndDescription),
// not by widening insertWorkBranchRow's own INSERT.
func (h *acceptanceHarness) stepIHaveStartedAWorkBranchWithATitleAndDescription(ctx context.Context) error {
	world := worldFrom(ctx)
	name := world.claimWorkBranch()
	if err := h.insertWorkBranchRow(ctx, world.repoID, name, world.targetBranch, "draft", world.author.identifier()); err != nil {
		return err
	}
	return h.setTitleAndDescription(world, world.author, name)
}

// stepIUpdateItsTitleAndDescription is "When I update its title and
// description": `loam work set` with values distinct from whatever fixture
// text the Given used to reach "reviewable" -- see the
// acceptanceLifecycleUpdated* constants' own doc comment for why
// distinctness matters here.
func (h *acceptanceHarness) stepIUpdateItsTitleAndDescription(ctx context.Context) error {
	world := worldFrom(ctx)
	res := h.runLoamAs(world, world.author, acceptanceLifecycleUpdatedDescription,
		"work", "set", world.repo(), world.workBranch, "--title", acceptanceLifecycleUpdatedTitle)
	world.lastCLI = res
	return requireLoamOK(res, fmt.Sprintf("loam work set (as %s)", world.author.identifier()))
}

// stepTheWorkBranchKeepsItsState is this scenario's own wording for "the
// state did not move" -- a bare title/description edit is not a lifecycle
// transition, and this is the assertion that would catch one that
// accidentally became one.
func (h *acceptanceHarness) stepTheWorkBranchKeepsItsState(ctx context.Context, want string) error {
	world := worldFrom(ctx)
	return h.requireWorkBranchState(ctx, world, world.workBranch, want)
}

// stepTheNewTitleAndDescriptionAreShown reads the update back through
// `loam work show` -- the read path the Gherkin's "are SHOWN" names, rather
// than a direct row read -- and asserts both fields equal exactly what
// stepIUpdateItsTitleAndDescription just sent, not merely that they are
// non-empty.
func (h *acceptanceHarness) stepTheNewTitleAndDescriptionAreShown(ctx context.Context) error {
	world := worldFrom(ctx)
	out, err := h.showWorkBranch(world, world.author, world.workBranch)
	if err != nil {
		return err
	}
	if out.Title != acceptanceLifecycleUpdatedTitle || out.Description != acceptanceLifecycleUpdatedDescription {
		return fmt.Errorf("loam work show reports title %q / description %q, want %q / %q", out.Title, out.Description, acceptanceLifecycleUpdatedTitle, acceptanceLifecycleUpdatedDescription)
	}
	return nil
}

// stepTheReviewerSubmitsAVerdict is "When the reviewer \"...\" submits an
// \"...\" verdict": a literal reviewer identity, parsed and recorded as
// world.reviewer for a later step to read, then one real SubmitVerdict.
func (h *acceptanceHarness) stepTheReviewerSubmitsAVerdict(ctx context.Context, reviewerID, outcome string) error {
	world := worldFrom(ctx)
	reviewer, err := parseAcceptanceActor(reviewerID)
	if err != nil {
		return err
	}
	if reviewer.role != "reviewer" {
		return fmt.Errorf("agent %q has role %q, but this step names a REVIEWER", reviewerID, reviewer.role)
	}
	world.reviewer = reviewer
	_, err = h.submitVerdict(world, reviewer, world.workBranch, outcome)
	return err
}

// stepThereIsNoAuthorActionThatSetsIt is "Then there is no author action
// that sets it \"complete\"": the closest thing to a completion command an
// author could reach for, `loam work complete`, does not exist at all --
// internal/cli/router.go's own work subcommand table has no such entry,
// per proto/loam/v1/workbranch.proto's own SubmitVerdict comment ("There is
// no agent-facing completion RPC ... COMPLETE is set by the server when the
// upstream PR merges"). Asserting the CLI's own usage rejection, not merely
// "the state is still X", is what makes this catch a REGRESSION that added
// such a route: if `work complete` ever started succeeding, this step would
// see exit 0 where it demands the "usage" rejection and fail there, before
// the state check below even runs. That state check is the Gherkin's own
// claim made literal: the row's state must still differ from state after
// the rejected attempt.
func (h *acceptanceHarness) stepThereIsNoAuthorActionThatSetsIt(ctx context.Context, state string) error {
	world := worldFrom(ctx)
	res := h.runLoamAs(world, world.author, "", "work", "complete", world.repo(), world.workBranch)
	if err := requireLoamRejected(res, "loam work complete (as the author)", "usage", 2); err != nil {
		return fmt.Errorf("%w -- no author-facing action may set a work branch to %q", err, state)
	}
	current, _, err := h.workBranchStateConflict(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if current == state {
		return fmt.Errorf("work branch %s is in state %q after the rejected attempt -- an author action set it to %q after all", world.workBranch, current, state)
	}
	return nil
}

// stepITryToUpdateItsTitle is "When I try to update its title": `loam work
// set --title ...` against a work branch this scenario expects to already
// be terminal. Unlike stepIUpdateItsTitleAndDescription it does not fail
// the step on a non-zero exit -- the refusal is the following Then's job
// (the shared stepTheAttemptIsRejectedAsFailedPrecondition,
// acceptance_review_test.go), reached through world.lastCLI exactly as
// every other CLI-driven refusal in this suite is.
func (h *acceptanceHarness) stepITryToUpdateItsTitle(ctx context.Context) error {
	world := worldFrom(ctx)
	world.lastCLI = h.runLoamAs(world, world.author, "", "work", "set", world.repo(), world.workBranch, "--title", "An edit a terminal work branch must refuse")
	return nil
}

// stepTheTargetBranchAdvancesWithChangesThatMergeCleanly is "When the
// target branch advances with changes that merge cleanly": unlike every
// other target-advance scenario in this suite, this one's own Given ("a
// work branch in state \"reviewable\"", acceptance_review_test.go's
// stepAWorkBranchInState) gives the branch a real title, description and
// review round but no git presence at all. The production mergeability
// checker (internal/mirrorsync.StoreMergeabilityChecker) runs git
// merge-tree against the work branch's OWN ref, so this step builds that
// ref itself: a mirror cloned for real from upstream, then one commit on
// the work branch's ref, forked from the target's CURRENT tip, touching a
// path the target's own advance (below) never does -- see
// acceptanceCleanAdvanceFile/acceptanceCleanWorkBranchFile's own doc
// comment for why that distinctness is load-bearing.
func (h *acceptanceHarness) stepTheTargetBranchAdvancesWithChangesThatMergeCleanly(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	sha, err := commitIntoMirror(ctx, world.mirrorDir, refnames.WorkBranch(world.workBranch), refnames.TargetBranch(world.targetBranch),
		acceptanceCleanWorkBranchFile, "# This work branch's own, untouched file\n", "acceptance: the work branch's own commit")
	if err != nil {
		return err
	}
	world.workBranchSHA = sha
	if err := h.forge.AdvanceBranch(ctx, world.repo(), world.targetBranch, fakeforge.AdvanceOptions{
		Path:    acceptanceCleanAdvanceFile,
		Content: []byte(acceptanceCleanAdvanceContent),
		Message: "acceptance: a clean, non-conflicting target advance",
	}); err != nil {
		return fmt.Errorf("advancing upstream %s on %s: %w", world.targetBranch, world.repo(), err)
	}
	return h.stepTheNextSyncRuns(ctx)
}

// stepTheWorkBranchsCommitsAreUnchanged asserts the work branch's own ref in
// the mirror still points at the commit
// stepTheTargetBranchAdvancesWithChangesThatMergeCleanly made, proving the
// mergeability check -- correctly, for a clean merge -- never touched it:
// "a broker, not an author" (StoreMergeabilityChecker's own doc comment).
func (h *acceptanceHarness) stepTheWorkBranchsCommitsAreUnchanged(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.workBranchSHA == "" {
		return fmt.Errorf("no baseline commit was recorded for work branch %s, so \"unchanged\" would compare against nothing", world.workBranch)
	}
	sha, err := mirrorRefSHA(world.mirrorDir, refnames.WorkBranch(world.workBranch))
	if err != nil {
		return fmt.Errorf("reading the mirror's %s: %w", refnames.WorkBranch(world.workBranch), err)
	}
	if sha != world.workBranchSHA {
		return fmt.Errorf("work branch %s moved from %s to %s after a clean target advance; a mergeability check must never write to it", world.workBranch, world.workBranchSHA, sha)
	}
	return nil
}

// stepItKeepsItsState is this scenario's own third-person wording for "the
// state did not move" -- see stepTheWorkBranchKeepsItsState for the other
// scenario's near-twin and why the two are registered separately.
func (h *acceptanceHarness) stepItKeepsItsState(ctx context.Context, want string) error {
	world := worldFrom(ctx)
	return h.requireWorkBranchState(ctx, world, world.workBranch, want)
}

// --- driver plumbing ---

// requireRPCRejected asserts err is a connect error carrying exactly want.
// Asserting the CODE, not merely that the call failed, is what stops a
// "rejected as a failed precondition" claim from being satisfied by an
// unauthenticated client, a typo'd repo name (NotFound), or a transport
// error -- each of which fails the RPC and none of which is the refusal
// the scenario is about.
func requireRPCRejected(err error, what string, want connect.Code) error {
	if err == nil {
		return fmt.Errorf("%s succeeded, want a %s rejection", what, want)
	}
	if got := connect.CodeOf(err); got != want {
		return fmt.Errorf("%s was rejected as %s (%v), want %s", what, got, err, want)
	}
	return nil
}

// newWorkBranchServiceClient builds an ADMIN-authenticated connect client
// for loam.v1.WorkBranchService. That service is the CLI's, but the admin
// reaches it as a superuser -- internal/httpauth.Auth.CLI accepts valid
// admin basic auth and marks the context admin, and
// handler.CapabilityChecker bypasses every capability gate for one -- which
// is precisely how proposal.proto tells an admin to send a branch back:
// "To send a branch back for another round, the admin calls
// loam.v1.WorkBranchService.RequestReview."
func (h *acceptanceHarness) newWorkBranchServiceClient() loamv1connect.WorkBranchServiceClient {
	return loamv1connect.NewWorkBranchServiceClient(h.adminHTTPClient(), h.server.baseURL)
}

// listProposals reads the admin's proposal queue.
func (h *acceptanceHarness) listProposals(ctx context.Context) ([]*adminv1.Proposal, error) {
	resp, err := h.newProposalServiceClient().ListProposals(ctx, connect.NewRequest(&adminv1.ListProposalsRequest{
		Page: &loamv1.Page{Limit: 100},
	}))
	if err != nil {
		return nil, fmt.Errorf("listing the proposal queue: %w", err)
	}
	return resp.Msg.GetProposals(), nil
}

// proposalFor finds repo/name among proposals.
func proposalFor(proposals []*adminv1.Proposal, repo, name string) (*adminv1.Proposal, bool) {
	for _, p := range proposals {
		if p.GetWorkBranch().GetRepo() == repo && p.GetWorkBranch().GetName() == name {
			return p, true
		}
	}
	return nil, false
}

// acceptProposalForReal runs one proposal acceptance through the
// PRODUCTION accept engine -- the harness's own
// *mirrorsync.StoreProposalAccepter, constructed by newAcceptanceAccepter
// exactly as cmd/server/sync.go's buildProposalAccepter constructs the
// server's, over the same live pool, the same authenticated
// gittransport.Transport, and the shared fake forge's provider surface.
//
// # Why not the AcceptProposal RPC, when this suite plainly has one
//
// The RPC's PRECONDITION half is exercised for real, by "When I try to
// accept it" (stepITryToAcceptIt): both refusal scenarios in this feature
// file go through the live handler, which is where the admin gate and the
// reviewed / unconflicted / >= 1-current-round-approve checks live and
// where they are answered before any delegation happens.
//
// Its ENGINE half cannot be reached from this suite, and the reason is
// structural rather than incidental. The server's accepter binds a REAL
// *forge.Forgejo per repo (sync.go's forgePRTracker.provider), from
// repos.forge_host plus an encrypted credentials row -- and this suite has
// neither half of that pair. It seeds no credentials row at all (the
// harness resolves upstream git auth through staticTokenCredentialSource,
// not the encrypted store), and even with one, *forge.Forgejo speaks
// Forgejo's own pulls endpoints, which internal/fakeforge deliberately
// does not serve: its /api/v1/... surface implements only ValidateToken's
// scope probe and answers a PR creation 501 on purpose rather than invent
// wire behaviour nothing has verified against a real Forgejo (loam-c8v).
// Making the RPC work end to end here would mean building a second
// Forgejo-REST translator inside this package -- which is exactly what
// cmd/demoenv/forgejoapi.go already is, and what Taskfile.yml's demo:m5
// already drives the shipped binary through.
//
// So the layers divide the work rather than duplicating it: the real
// Forgejo client's encoding and error classification are demo:m5's and
// internal/forgesuite's, provider-level parity between the fake and the
// real client is the shared contract suite's, and what an accept DECIDES
// to do -- push loam/<name>, open or reuse a pull request, record it --
// is this file's, asserted against the same production type the RPC
// delegates to. The one thing no layer covers is the handler's own
// delegation line, which is why "Closing a work branch closes its upstream
// PR" is still @wip: that scenario's whole subject IS a delegation of this
// shape, and it would pass here whether or not the handler made it.
//
// The PR number is read back off the ROW rather than trusted from the
// return value, the same discipline stepAnAcceptedWorkBranchWhosePRHasMerged
// applies and for the same reason: that column is the whole of
// StorePRPoller's poll set, so an accept that opened a PR and failed to
// record it must fail HERE rather than surface much later as a poller that
// polled nothing.
//
// The first accept's URL and upstream-branch SHA are latched separately
// (world.upstreamPRURL, world.firstUpstreamBranchSHA) so a re-accept has
// something to be compared against that is not simply its own output.
//
// world.accepterOverride, when a scenario has set one, is used INSTEAD of
// the harness's own h.accepter -- see its doc comment on acceptanceWorld
// for why the PRAttribution knob needs a scenario-scoped accepter rather
// than a process-wide env var flip.
func (h *acceptanceHarness) acceptProposalForReal(ctx context.Context, world *acceptanceWorld) error {
	accepter := h.accepter
	if world.accepterOverride != nil {
		accepter = world.accepterOverride
	}
	result, err := accepter.AcceptProposal(ctx, mirrorsync.RepoID(world.repo()), world.workBranch)
	if err != nil {
		return fmt.Errorf("accepting proposal %s in repo %s: %w", world.workBranch, world.repo(), err)
	}
	world.lastAcceptPRURL = result.PRURL
	world.lastAcceptUpstreamBranch = result.UpstreamBranch
	number, err := h.recordedUpstreamPRNumber(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if number != result.PRNumber {
		return fmt.Errorf("the accept reported PR #%d but work_branches.upstream_pr_number holds #%d", result.PRNumber, number)
	}
	if world.upstreamPRNumber == 0 {
		world.upstreamPRNumber = number
		world.upstreamPRURL = result.PRURL
		sha, err := h.upstreamRefSHA(ctx, world, "refs/heads/"+result.UpstreamBranch)
		if err != nil {
			return fmt.Errorf("the accept reported pushing %s, but the forge does not advertise it: %w", result.UpstreamBranch, err)
		}
		world.firstUpstreamBranchSHA = sha
	}
	return nil
}

// --- fixture helpers ---

// setProposalTitleDescription gives a work branch its own title and
// description through `loam work set`, as its author. It is not
// decoration: RequestReview refuses a branch carrying neither, and these
// two values are what the accept engine turns into the upstream pull
// request's title and body.
func (h *acceptanceHarness) setProposalTitleDescription(world *acceptanceWorld, name string) error {
	res := h.runLoamAs(world, world.author, acceptanceProposalDescription(name),
		"work", "set", world.repo(), name, "--title", acceptanceProposalTitle(name))
	return requireLoamOK(res, fmt.Sprintf("loam work set (as %s)", world.author.identifier()))
}

// seedReviewedWorkBranch seeds ONE work branch and drives it all the way
// to reviewed with exactly one verdict of outcome, entirely through
// production paths:
//
//  1. a work_branches row (draft), the enrollment fixture technique
//     acceptance_seed_test.go already establishes;
//  2. a real commit in the bare mirror, forked from the TARGET tip and
//     rewriting acceptanceProposalFile -- the branch's actual content, and
//     the half of the later conflict that belongs to the work branch;
//  3. `loam work set` then `loam work request-review`, as the author,
//     which opens review round 1 and moves the branch to reviewable;
//  4. one real verdict from acceptanceProposalReviewerID, which is what
//     flips reviewable -> reviewed (internal/reviewpublish: the FIRST
//     verdict does that, whatever its outcome, so "reviewed with only a
//     neutral verdict" is a genuinely reachable row and not a forced one).
//
// It returns the branch's name, which is this scenario's own branch the
// first time it is called and an additional one afterwards (see
// acceptanceWorld.claimWorkBranch).
func (h *acceptanceHarness) seedReviewedWorkBranch(ctx context.Context, world *acceptanceWorld, state, outcome string) (string, error) {
	if state != "reviewed" {
		return "", fmt.Errorf("this fixture reaches a work branch's REVIEWED state through a real verdict; it cannot seed state %q", state)
	}
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return "", err
	}
	name := world.claimWorkBranch()
	if err := h.insertWorkBranchRow(ctx, world.repoID, name, world.targetBranch, "draft", world.author.identifier()); err != nil {
		return "", err
	}
	sha, err := commitIntoMirror(ctx, world.mirrorDir, refnames.WorkBranch(name), refnames.TargetBranch(world.targetBranch),
		acceptanceProposalFile, acceptanceProposalReadme(name), "acceptance: the proposal's own commit on "+name)
	if err != nil {
		return "", err
	}
	if name == world.workBranch {
		world.workBranchSHA = sha
	}
	if err := h.reviewWorkBranchTo(ctx, world, name, outcome); err != nil {
		return "", err
	}
	return name, nil
}

// reviewWorkBranchTo takes an already-seeded DRAFT work branch through the
// real review path to reviewed with one verdict of outcome, and verifies
// both halves of that claim afterwards: the row really is reviewed, and it
// carries exactly ONE verdict, with that outcome. The verdict count is
// what makes "with ONLY a disapprove verdict" and "with ONE approve
// verdict" mean what they say rather than "with at least one".
func (h *acceptanceHarness) reviewWorkBranchTo(ctx context.Context, world *acceptanceWorld, name, outcome string) error {
	if err := h.setProposalTitleDescription(world, name); err != nil {
		return err
	}
	if err := h.requestReview(world, world.author, name); err != nil {
		return err
	}
	world.reviewer = mustAcceptanceActor(acceptanceProposalReviewerID)
	if _, err := h.submitVerdict(world, world.reviewer, name, outcome); err != nil {
		return err
	}
	if err := h.requireWorkBranchState(ctx, world, name, "reviewed"); err != nil {
		return err
	}
	verdicts, err := h.listVerdicts(world, world.reviewer, name)
	if err != nil {
		return err
	}
	if len(verdicts) != 1 || verdicts[0].Outcome != outcome {
		return fmt.Errorf("work branch %s carries %+v, want exactly one %q verdict", name, verdicts, outcome)
	}
	if name == world.workBranch {
		world.verdictsBefore = verdicts
	}
	return nil
}

// advanceTargetConflictingly rewrites acceptanceProposalFile upstream on
// the target branch and runs one sync tick, so the mirror fetches the new
// tip and the PRODUCTION mergeability checker re-tests every open work
// branch against it. Nothing here touches work_branches: whatever the
// branches end up in, the sync cycle put them there.
func (h *acceptanceHarness) advanceTargetConflictingly(ctx context.Context, world *acceptanceWorld) error {
	err := h.forge.AdvanceBranch(ctx, world.repo(), world.targetBranch, fakeforge.AdvanceOptions{
		Path:    acceptanceProposalFile,
		Content: []byte(acceptanceAdvancedTargetReadme),
		Message: "acceptance: a conflicting target advance",
	})
	if err != nil {
		return fmt.Errorf("advancing upstream %s on %s: %w", world.targetBranch, world.repo(), err)
	}
	return h.stepTheNextSyncRuns(ctx)
}

// catchUpWorkBranch does what a real agent does after a conflicting
// advance, and nothing else: clone as the AUTHOR through the compiled CLI,
// fetch the target, merge it, resolve the one conflicting file, commit,
// and push back through /git/*.
//
// The push is the whole point. internal/catchup is wired as the policy
// socket's post-accept hook (cmd/server/main.go), so it only ever runs on
// a push that reached and passed the real pre-receive hook -- a fetch, or
// a commit written straight into the mirror, would move nothing and the
// conflict flag would never clear. loam-ppb is what makes that push legal
// at all: until it was fixed, internal/refpolicy compared the full
// work_branches.author identifier against a bare agent name and no author
// could push to their own branch.
//
// Installing that hook is therefore the first thing this does, and its
// PLACEMENT is load-bearing in both directions. A mirror cloned by
// ensureMirrorFromUpstream carries no hook at all: the server's one-time
// startup reconciliation ran long before any scenario's fixture existed.
// Without the hook the catch-up push is simply accepted by git, with no
// policy socket round trip and so no post-accept hook, and the conflict
// flag can never clear -- silently, because the push itself SUCCEEDS and
// the scenario only fails three assertions later. But the hook cannot be
// installed any earlier either: commitIntoMirror seeds a work branch by
// pushing straight into the bare mirror over its filesystem path, which
// carries none of the repo and agent context the smart-HTTP endpoint
// supplies, so a hooked mirror rejects it outright ("is not a work
// branch"). Every fixture write into this mirror is finished by the time
// this runs, and every push after it is a real agent's through /git/*.
//
// The merge is ASSERTED to conflict first. If a future fixture edit made
// the two rewrites auto-mergeable, every conflict scenario in this file
// would still "pass" while proving nothing, so a clean merge is a hard
// failure here rather than a silently easier path.
func (h *acceptanceHarness) catchUpWorkBranch(ctx context.Context, world *acceptanceWorld) error {
	if err := h.reconcileSeededMirror(ctx, world.mirrorDir); err != nil {
		return fmt.Errorf("installing the pre-receive hook on %s: %w", world.mirrorDir, err)
	}
	res := h.runLoamAs(world, world.author, "", "clone", world.repo(), world.workBranch)
	if err := requireLoamOK(res, fmt.Sprintf("loam clone (as %s)", world.author.identifier())); err != nil {
		return err
	}
	clone := filepath.Join(world.workspace, world.repoName)
	world.clonePath = clone
	if _, err := runIsolatedGit(ctx, clone, "fetch", "--quiet", "origin", world.targetBranch); err != nil {
		return fmt.Errorf("fetching %s into the author's clone: %w", world.targetBranch, err)
	}
	if _, err := runIsolatedGit(ctx, clone, "merge", "--no-commit", "--no-ff", "FETCH_HEAD"); err == nil {
		return fmt.Errorf("merging %s into %s succeeded cleanly, so there was no conflict to catch up from", world.targetBranch, world.workBranch)
	}
	unmerged, err := runIsolatedGit(ctx, clone, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return fmt.Errorf("listing the conflicted paths in the author's clone: %w", err)
	}
	if got := strings.TrimSpace(unmerged); got != acceptanceProposalFile {
		return fmt.Errorf("git reported conflicts in [%s], want exactly [%s]", got, acceptanceProposalFile)
	}
	if err := os.WriteFile(filepath.Join(clone, acceptanceProposalFile), []byte(acceptanceCaughtUpReadme), 0o644); err != nil {
		return fmt.Errorf("writing the author's conflict resolution: %w", err)
	}
	if _, err := runIsolatedGit(ctx, clone, "add", acceptanceProposalFile); err != nil {
		return fmt.Errorf("staging the author's conflict resolution: %w", err)
	}
	if _, err := runIsolatedGit(ctx, clone, "commit", "--quiet", "-m", "acceptance: merge "+world.targetBranch+" into "+world.workBranch); err != nil {
		return fmt.Errorf("committing the author's catch-up merge: %w", err)
	}
	if out, err := runIsolatedGit(ctx, clone, "push", "origin", world.workBranch); err != nil {
		return fmt.Errorf("the author's catch-up push was rejected, so nothing downstream of it can be true (see loam-ppb): %w\n%s", err, out)
	}
	return nil
}

// --- read helpers ---

// requireWorkBranchState asserts name's recorded state, reading the column
// straight back rather than through any value this harness cached.
func (h *acceptanceHarness) requireWorkBranchState(ctx context.Context, world *acceptanceWorld, name, want string) error {
	state, _, err := h.workBranchStateConflict(ctx, world.repoID, name)
	if err != nil {
		return err
	}
	if state != want {
		return fmt.Errorf("work branch %s/%s is in state %q, want %q", world.repo(), name, state, want)
	}
	return nil
}

// workBranchStateConflict reads one work branch's state and conflict flag.
func (h *acceptanceHarness) workBranchStateConflict(ctx context.Context, repoID uuid.UUID, name string) (string, string, error) {
	var state, conflict string
	err := h.server.pool.QueryRow(ctx,
		`SELECT state, conflict FROM work_branches WHERE repo_id = $1 AND name = $2`, repoID, name).Scan(&state, &conflict)
	if err != nil {
		return "", "", fmt.Errorf("reading state/conflict for work branch %s: %w", name, err)
	}
	return state, conflict, nil
}

// workBranchProposalText reads the title and description a work branch
// carries -- the source of truth "the proposed title and description are
// the work branch's own" compares the upstream PR against.
func (h *acceptanceHarness) workBranchProposalText(ctx context.Context, repoID uuid.UUID, name string) (string, string, error) {
	var title, description *string
	err := h.server.pool.QueryRow(ctx,
		`SELECT title, description FROM work_branches WHERE repo_id = $1 AND name = $2`, repoID, name).Scan(&title, &description)
	if err != nil {
		return "", "", fmt.Errorf("reading title/description for work branch %s: %w", name, err)
	}
	if title == nil || *title == "" || description == nil || *description == "" {
		return "", "", fmt.Errorf("work branch %s carries no title/description, so comparing a PR against them would prove nothing", name)
	}
	return *title, *description, nil
}

// recordedUpstreamPRURL reads work_branches.upstream_pr_url back, erroring
// on SQL NULL rather than answering with an empty string an assertion
// could then compare against another empty string.
func (h *acceptanceHarness) recordedUpstreamPRURL(ctx context.Context, repoID uuid.UUID, name string) (string, error) {
	var url *string
	err := h.server.pool.QueryRow(ctx,
		`SELECT upstream_pr_url FROM work_branches WHERE repo_id = $1 AND name = $2`, repoID, name).Scan(&url)
	if err != nil {
		return "", fmt.Errorf("reading upstream_pr_url for work branch %s: %w", name, err)
	}
	if url == nil || *url == "" {
		return "", fmt.Errorf("work branch %s has no recorded upstream_pr_url", name)
	}
	return *url, nil
}

// latestRound reads the branch's highest review round as (number,
// requested_by) -- who asked for it is what distinguishes an admin
// send-back from an author's own re-request and from the server's
// catch-up restore, and no CLI command reports a round at all (loam-8d2).
func (h *acceptanceHarness) latestRound(ctx context.Context, world *acceptanceWorld, name string) (int, string, error) {
	id, err := h.workBranchID(ctx, world, name)
	if err != nil {
		return 0, "", err
	}
	var number int
	var requestedBy string
	err = h.server.pool.QueryRow(ctx,
		`SELECT number, requested_by FROM review_rounds WHERE work_branch_id = $1 ORDER BY number DESC LIMIT 1`, id).Scan(&number, &requestedBy)
	if err != nil {
		return 0, "", fmt.Errorf("reading the latest review round for %s: %w", name, err)
	}
	return number, requestedBy, nil
}

// theOneUpstreamPR asserts the forge holds exactly one pull request for
// this scenario's repo and returns it.
//
// "Exactly one" is not incidental strictness -- it IS the "no new upstream
// PR is created" claim, and it is asserted against the forge's own whole
// record (open, closed and merged alike), never against what Loam believes
// it opened, which would make the assertion circular.
func (h *acceptanceHarness) theOneUpstreamPR(world *acceptanceWorld) (fakeforge.PullRequest, error) {
	prs := h.forge.PullRequests(world.repo())
	if len(prs) != 1 {
		return fakeforge.PullRequest{}, fmt.Errorf("the forge holds %d pull requests for %s (%+v), want exactly 1", len(prs), world.repo(), prs)
	}
	return prs[0], nil
}

// --- Givens ---

// stepAWorkBranchInStateWithOneVerdict backs both "a work branch in state
// ... with one ... verdict" and "a work branch in state ... with only a
// ... verdict". The two sentences describe the same fixture: reviewWorkBranchTo
// verifies the branch carries EXACTLY one verdict, with that outcome, so
// "one" and "only a" are the same claim and the shorter wording was never
// the weaker one.
func (h *acceptanceHarness) stepAWorkBranchInStateWithOneVerdict(ctx context.Context, state, outcome string) error {
	_, err := h.seedReviewedWorkBranch(ctx, worldFrom(ctx), state, outcome)
	return err
}

// stepAProposalInStateWithOneVerdict is "a proposal in state ... with one
// ... verdict": the same fixture, plus the assertion that makes the word
// "proposal" true -- the branch really is in the admin's queue, i.e. it
// satisfies ProposalService's own reviewed + non-stale-approve +
// unconflicted predicate rather than merely looking like it should.
//
// Recording that (world.queued) is also what keeps the later "it no longer
// appears in the proposal queue" honest: absence can only be a REMOVAL if
// presence was observed first.
func (h *acceptanceHarness) stepAProposalInStateWithOneVerdict(ctx context.Context, state, outcome string) error {
	world := worldFrom(ctx)
	name, err := h.seedReviewedWorkBranch(ctx, world, state, outcome)
	if err != nil {
		return err
	}
	if name != world.workBranch {
		return fmt.Errorf("this step seeds the scenario's own proposal, but claimed the name %s", name)
	}
	return h.requireQueued(ctx, world)
}

// requireQueued asserts this scenario's work branch is in the admin's
// proposal queue right now, and records that it was seen there.
func (h *acceptanceHarness) requireQueued(ctx context.Context, world *acceptanceWorld) error {
	proposals, err := h.listProposals(ctx)
	if err != nil {
		return err
	}
	if _, ok := proposalFor(proposals, world.repo(), world.workBranch); !ok {
		return fmt.Errorf("work branch %s/%s is not in the proposal queue (%d queued)", world.repo(), world.workBranch, len(proposals))
	}
	world.queued = true
	return nil
}

// stepAWorkBranchFlaggedAsConflicted seeds a DRAFT work branch that
// genuinely conflicts with its target, and lets the production mergeability
// check be the thing that flags it: the branch forks from the target tip
// and rewrites acceptanceProposalFile, a real upstream advance rewrites the
// same file differently, and one sync tick re-tests it.
//
// A draft branch is merely FLAGGED, never demoted (the conflict CASE in
// MarkWorkBranchConflicted), which is what leaves the following Given free
// to take it to reviewed. That ordering is the only way a
// reviewed-AND-conflicted row is reachable at all: a target advance
// demotes a reviewed branch in the same statement that flags it, so the
// handler's conflict precondition can only ever be met by a branch that
// was flagged first and reviewed afterwards.
func (h *acceptanceHarness) stepAWorkBranchFlaggedAsConflicted(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	name := world.claimWorkBranch()
	if err := h.insertWorkBranchRow(ctx, world.repoID, name, world.targetBranch, "draft", world.author.identifier()); err != nil {
		return err
	}
	sha, err := commitIntoMirror(ctx, world.mirrorDir, refnames.WorkBranch(name), refnames.TargetBranch(world.targetBranch),
		acceptanceProposalFile, acceptanceProposalReadme(name), "acceptance: the proposal's own commit on "+name)
	if err != nil {
		return err
	}
	if name == world.workBranch {
		world.workBranchSHA = sha
	}
	if err := h.advanceTargetConflictingly(ctx, world); err != nil {
		return err
	}
	state, conflict, err := h.workBranchStateConflict(ctx, world.repoID, name)
	if err != nil {
		return err
	}
	if conflict != "flagged" || state != "draft" {
		return fmt.Errorf("after a conflicting advance, draft work branch %s is %s/%s, want draft/flagged", name, state, conflict)
	}
	return nil
}

// stepItIsInStateWithOneVerdict is "it is in state ... with one ...
// verdict": the preceding Given's branch, taken to reviewed through the
// real review path.
//
// It also asserts the conflict flag SURVIVED that path. Nothing in
// request-review or verdict publication touches work_branches.conflict, so
// this is a guard rather than a behaviour under test -- but it is the
// guard that keeps "A conflicted work branch cannot be accepted" from
// going vacuous: were the flag ever cleared here, the accept would be
// refused for having the wrong state or no approval, and the scenario
// would pass while never reaching the precondition it exists to check.
func (h *acceptanceHarness) stepItIsInStateWithOneVerdict(ctx context.Context, state, outcome string) error {
	world := worldFrom(ctx)
	if state != "reviewed" {
		return fmt.Errorf("this fixture reaches a work branch's REVIEWED state through a real verdict; it cannot seed state %q", state)
	}
	if err := h.reviewWorkBranchTo(ctx, world, world.workBranch, outcome); err != nil {
		return err
	}
	_, conflict, err := h.workBranchStateConflict(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if conflict == "none" {
		return fmt.Errorf("work branch %s lost its conflict flag on the way to reviewed, so the accept could not be refused for being conflicted", world.workBranch)
	}
	return nil
}

// stepAWorkBranchInStateWhoseUpstreamPRExists is "a work branch in state
// ... whose upstream PR has been created": a real proposal, accepted
// through the admin's own AcceptProposal RPC.
func (h *acceptanceHarness) stepAWorkBranchInStateWhoseUpstreamPRExists(ctx context.Context, state string) error {
	world := worldFrom(ctx)
	if _, err := h.seedReviewedWorkBranch(ctx, world, state, "approve"); err != nil {
		return err
	}
	return h.acceptProposalForReal(ctx, world)
}

// stepAWorkBranchWhoseUpstreamPRExists is the unqualified form of the
// step above: the state is not named in the Gherkin because the scenario
// goes on to move it, but a PR can only exist on a branch that was
// reviewed and accepted, so the fixture is identical.
func (h *acceptanceHarness) stepAWorkBranchWhoseUpstreamPRExists(ctx context.Context) error {
	return h.stepAWorkBranchInStateWhoseUpstreamPRExists(ctx, "reviewed")
}

// stepAConflictingTargetAdvanceResetIt is "a conflicting target advance
// reset it to ...": one real upstream advance plus one sync tick, and then
// the assertion that the production mergeability check did BOTH halves of
// what MarkWorkBranchConflicted promises for a reviewed branch -- demoted
// to draft AND flagged 'reset', the value that later tells the catch-up
// detector to restore it to reviewable rather than merely unflag it.
func (h *acceptanceHarness) stepAConflictingTargetAdvanceResetIt(ctx context.Context, want string) error {
	world := worldFrom(ctx)
	if err := h.advanceTargetConflictingly(ctx, world); err != nil {
		return err
	}
	state, conflict, err := h.workBranchStateConflict(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if state != want || conflict != "reset" {
		return fmt.Errorf("after a conflicting advance, work branch %s is %s/%s, want %s/reset", world.workBranch, state, conflict, want)
	}
	return nil
}

// stepItWasCaughtUpReReviewedAndApprovedAgain is the whole recovery arc in
// one Given: a real catch-up push, the server's own restore, and a fresh
// approve in the new round.
//
// Every assertion in between is load-bearing for the scenario that follows
// ("Re-accepting a caught-up work branch updates the existing PR"), which
// could not otherwise tell a genuine re-accept from an accept that simply
// never lost its preconditions:
//
//   - conflict 'none' and state reviewable prove the push was what
//     restored the branch;
//   - round 2, requested_by 'server', proves NO request-review was called
//     or needed -- and this step never calls one, which is checkable by
//     reading it;
//   - the round-1 approve going stale is what makes the re-approve
//     necessary rather than redundant: AcceptProposal counts approvals in
//     the CURRENT round only, so without it the second accept would be
//     legal on the strength of the first review.
func (h *acceptanceHarness) stepItWasCaughtUpReReviewedAndApprovedAgain(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := h.catchUpWorkBranch(ctx, world); err != nil {
		return err
	}
	state, conflict, err := h.workBranchStateConflict(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if state != "reviewable" || conflict != "none" {
		return fmt.Errorf("after the author's catch-up push, work branch %s is %s/%s, want reviewable/none", world.workBranch, state, conflict)
	}
	number, requestedBy, err := h.latestRound(ctx, world, world.workBranch)
	if err != nil {
		return err
	}
	if number != 2 || requestedBy != "server" {
		return fmt.Errorf("the catch-up left the branch on round %d requested by %q, want round 2 requested by \"server\"", number, requestedBy)
	}
	stale, err := h.listVerdicts(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if len(stale) != 1 || !stale[0].Stale {
		return fmt.Errorf("the round-1 verdicts read back as %+v, want exactly one, marked stale by the new round", stale)
	}
	if _, err := h.submitVerdict(world, world.reviewer, world.workBranch, "approve"); err != nil {
		return err
	}
	return h.requireWorkBranchState(ctx, world, world.workBranch, "reviewed")
}

// --- Whens ---

// stepIOpenTheProposalQueue reads the admin's proposal queue through the
// real RPC and keeps the page for the Then steps.
func (h *acceptanceHarness) stepIOpenTheProposalQueue(ctx context.Context) error {
	world := worldFrom(ctx)
	proposals, err := h.listProposals(ctx)
	if err != nil {
		return err
	}
	world.lastProposals = proposals
	return nil
}

// stepITryToAcceptIt attempts an accept that the scenario expects to be
// refused. Unlike "I accept it" it does not fail the step on an error --
// the refusal is the following Then's business -- but it does record that
// an admin RPC was attempted, which is what routes that Then to its RPC
// arm (stepTheAttemptIsRejectedAsFailedPrecondition).
func (h *acceptanceHarness) stepITryToAcceptIt(ctx context.Context) error {
	world := worldFrom(ctx)
	_, err := acceptProposal(ctx, h.newProposalServiceClient(), world.repo(), world.workBranch)
	world.lastRPCErr = err
	world.rpcAttempted = true
	return nil
}

// stepIRequestAReReview sends the branch back for another round as the
// ADMIN, through loam.v1.WorkBranchService.RequestReview -- the surface
// proposal.proto names for exactly this ("To send a branch back for
// another round, the admin calls ..."), reached as a superuser over basic
// auth.
//
// The round number and the verdicts as they stood BEFORE the call are
// snapshotted here so the following Thens compare against a real earlier
// state rather than against a constant.
func (h *acceptanceHarness) stepIRequestAReReview(ctx context.Context) error {
	world := worldFrom(ctx)
	before, _, err := h.latestRound(ctx, world, world.workBranch)
	if err != nil {
		return err
	}
	world.roundBefore = before
	verdicts, err := h.listVerdicts(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if len(verdicts) == 0 {
		return fmt.Errorf("work branch %s carries no verdicts before the re-review, so \"the prior verdicts are marked stale\" would hold vacuously", world.workBranch)
	}
	world.verdictsBefore = verdicts
	_, err = h.newWorkBranchServiceClient().RequestReview(ctx, connect.NewRequest(&loamv1.RequestReviewRequest{
		Repo:       world.repo(),
		WorkBranch: world.workBranch,
	}))
	if err != nil {
		return fmt.Errorf("requesting a re-review of %s/%s as the admin: %w", world.repo(), world.workBranch, err)
	}
	return nil
}

// stepICloseItWithAReason closes the work branch through the admin's own
// CloseWorkBranch RPC, with a non-empty reason (the handler refuses an
// empty one outright).
func (h *acceptanceHarness) stepICloseItWithAReason(ctx context.Context) error {
	world := worldFrom(ctx)
	world.closeReason = acceptanceCloseReason
	_, err := h.newProposalServiceClient().CloseWorkBranch(ctx, connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo:       world.repo(),
		WorkBranch: world.workBranch,
		Body:       world.closeReason,
	}))
	if err != nil {
		return fmt.Errorf("closing work branch %s/%s: %w", world.repo(), world.workBranch, err)
	}
	return nil
}

// stepTheUpstreamPRIsClosedWithoutMerging closes the pull request ON THE
// FORGE, as someone doing it in the forge's own UI would -- the control
// API, not Loam's ClosePR -- so what the next sync observes is a genuine
// third-party close rather than a state Loam itself wrote.
func (h *acceptanceHarness) stepTheUpstreamPRIsClosedWithoutMerging(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.upstreamPRNumber == 0 {
		return fmt.Errorf("no upstream PR was recorded for %s/%s, so there is nothing to close", world.repo(), world.workBranch)
	}
	if err := h.forge.ClosePR(ctx, world.repo(), world.upstreamPRNumber); err != nil {
		return fmt.Errorf("closing upstream PR %s#%d on the forge: %w", world.repo(), world.upstreamPRNumber, err)
	}
	return nil
}

// stepItsTargetBranchAdvancesWithConflictingChanges is the When form of
// the conflicting advance. It first asserts the branch IS in the proposal
// queue: "removes a proposal from the queue" is a claim about a removal,
// and a branch that was never queued would satisfy the following Then no
// matter what the advance did.
func (h *acceptanceHarness) stepItsTargetBranchAdvancesWithConflictingChanges(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := h.requireQueued(ctx, world); err != nil {
		return err
	}
	return h.advanceTargetConflictingly(ctx, world)
}

// --- Thens ---

// stepTheApprovedWorkBranchIsListed asserts this scenario's approved
// branch is in the queue page the When just read.
func (h *acceptanceHarness) stepTheApprovedWorkBranchIsListed(ctx context.Context) error {
	world := worldFrom(ctx)
	proposal, ok := proposalFor(world.lastProposals, world.repo(), world.workBranch)
	if !ok {
		return fmt.Errorf("approved work branch %s/%s is not in the proposal queue (%d queued)", world.repo(), world.workBranch, len(world.lastProposals))
	}
	if proposal.GetWorkBranch().GetState() != loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED {
		return fmt.Errorf("queued proposal %s is listed as %s, want REVIEWED", world.workBranch, proposal.GetWorkBranch().GetState())
	}
	world.queued = true
	return nil
}

// stepTheDisapprovedWorkBranchIsNotListed asserts the second, disapproved
// branch is absent -- and, first, that it EXISTS and is itself reviewed,
// so its absence is the approval predicate at work rather than a row that
// was never seeded or never reached a state the queue considers.
func (h *acceptanceHarness) stepTheDisapprovedWorkBranchIsNotListed(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.secondWorkBranch == "" {
		return fmt.Errorf("this scenario seeded no second work branch, so \"the disapproved work branch is not listed\" would hold vacuously")
	}
	if err := h.requireWorkBranchState(ctx, world, world.secondWorkBranch, "reviewed"); err != nil {
		return err
	}
	if _, ok := proposalFor(world.lastProposals, world.repo(), world.secondWorkBranch); ok {
		return fmt.Errorf("disapproved work branch %s/%s is in the proposal queue", world.repo(), world.secondWorkBranch)
	}
	return nil
}

// stepEachListedProposalShowsItsVerdicts asserts every queued proposal
// carries the verdicts that put it there -- the proto field's own contract
// ("This round's verdicts ..., so the admin sees who approved without a
// second call") -- and that this scenario's own proposal carries the exact
// reviewer and outcome it was approved with.
func (h *acceptanceHarness) stepEachListedProposalShowsItsVerdicts(ctx context.Context) error {
	world := worldFrom(ctx)
	if len(world.lastProposals) == 0 {
		return fmt.Errorf("the proposal queue is empty, so \"each listed proposal shows its verdicts\" would hold vacuously")
	}
	for _, proposal := range world.lastProposals {
		name := proposal.GetWorkBranch().GetName()
		verdicts := proposal.GetVerdicts()
		if len(verdicts) == 0 {
			return fmt.Errorf("queued proposal %s carries no verdicts", name)
		}
		for _, v := range verdicts {
			if v.GetReviewer() == "" {
				return fmt.Errorf("a verdict on queued proposal %s names no reviewer (%+v)", name, v)
			}
			if v.GetOutcome() == loamv1.VerdictOutcome_VERDICT_OUTCOME_UNSPECIFIED {
				return fmt.Errorf("a verdict on queued proposal %s has no outcome (%+v)", name, v)
			}
		}
	}
	mine, ok := proposalFor(world.lastProposals, world.repo(), world.workBranch)
	if !ok {
		return fmt.Errorf("this scenario's own proposal %s is not queued", world.workBranch)
	}
	for _, v := range mine.GetVerdicts() {
		if v.GetReviewer() == world.reviewer.identifier() && v.GetOutcome() == loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE {
			return nil
		}
	}
	return fmt.Errorf("queued proposal %s does not show %s's approve verdict (%+v)", world.workBranch, world.reviewer.identifier(), mine.GetVerdicts())
}

// stepAnUpstreamPRIsCreatedWithAGeneratedBranchName asserts all three
// halves of what an accept claims to have done, each read from the side
// that owns it: the RPC reported the namespaced branch it generated, the
// FORGE advertises that branch at the work branch's own tip, and the forge
// holds one open pull request from it into the target.
//
// The branch name is checked to be GENERATED -- loam/<name>, the namespace
// docs/sync-spec.md specifies and StorePRPoller is the only deleter of --
// not merely non-empty, and not the work branch's own name.
func (h *acceptanceHarness) stepAnUpstreamPRIsCreatedWithAGeneratedBranchName(ctx context.Context) error {
	world := worldFrom(ctx)
	want := "loam/" + world.workBranch
	if world.lastAcceptUpstreamBranch != want {
		return fmt.Errorf("the accept reported upstream branch %q, want the generated %q", world.lastAcceptUpstreamBranch, want)
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
	pr, err := h.theOneUpstreamPR(world)
	if err != nil {
		return err
	}
	if pr.HeadBranch != want || pr.TargetBranch != world.targetBranch || pr.State != "open" {
		return fmt.Errorf("the forge's pull request is %+v, want an open one from %s into %s", pr, want, world.targetBranch)
	}
	if pr.Number != world.upstreamPRNumber {
		return fmt.Errorf("the forge's pull request is #%d, but the work branch records #%d", pr.Number, world.upstreamPRNumber)
	}
	return nil
}

// stepTheProposedTitleAndDescriptionAreTheBranchsOwn asserts the upstream
// pull request carries the work branch's title verbatim and its
// description as the body, read from the row rather than from a harness
// literal so the claim is genuinely "the work branch's own".
//
// The body assertion is EQUALITY, not containment, and it is two-sided:
// docs/sync-spec.md specifies the body as the description followed by the
// Loam attribution footer and says agent attribution belongs in the commit
// authors rather than here, so the body must also carry no agent identity
// at all. A "contains" check would be satisfied by a footer buried
// mid-body, and an attribution-blind check would not notice a body that
// named the author or the reviewer.
func (h *acceptanceHarness) stepTheProposedTitleAndDescriptionAreTheBranchsOwn(ctx context.Context) error {
	world := worldFrom(ctx)
	title, description, err := h.workBranchProposalText(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	pr, err := h.theOneUpstreamPR(world)
	if err != nil {
		return err
	}
	if pr.Title != title {
		return fmt.Errorf("the upstream PR's title is %q, want the work branch's own %q", pr.Title, title)
	}
	wantBody := description
	if h.prAttribution {
		wantBody = description + "\n\n" + acceptanceAttributionFooter
	}
	if pr.Description != wantBody {
		return fmt.Errorf("the upstream PR's body is %q, want %q", pr.Description, wantBody)
	}
	for _, identity := range []string{world.author.identifier(), world.reviewer.identifier(), world.author.name, "admin"} {
		if strings.Contains(pr.Description, identity) {
			return fmt.Errorf("the upstream PR's body names %q; agent attribution belongs in the commit authors, not the PR body", identity)
		}
	}
	return nil
}

// stepTheUpstreamPRURLIsRecorded asserts the URL landed on the work branch
// row -- the column an admin surface reads the accepted PR back from --
// and that it is the same URL the accept reported, so a row carrying some
// other pull request's link would fail here.
func (h *acceptanceHarness) stepTheUpstreamPRURLIsRecorded(ctx context.Context) error {
	world := worldFrom(ctx)
	recorded, err := h.recordedUpstreamPRURL(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if recorded != world.lastAcceptPRURL {
		return fmt.Errorf("work branch %s records upstream_pr_url %q, but the accept reported %q", world.workBranch, recorded, world.lastAcceptPRURL)
	}
	number, err := h.recordedUpstreamPRNumber(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	pr, err := h.theOneUpstreamPR(world)
	if err != nil {
		return err
	}
	if number != pr.Number {
		return fmt.Errorf("work branch %s records PR #%d, but the forge's pull request is #%d", world.workBranch, number, pr.Number)
	}
	return nil
}

// stepANewReviewRoundIsOpened asserts the re-review advanced the round by
// exactly one, and that the new round records the ADMIN as having asked
// for it -- distinguishing a send-back from the author's own re-request
// and from internal/catchup's server-opened restore round, which are the
// other two ways a round is ever opened.
func (h *acceptanceHarness) stepANewReviewRoundIsOpened(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.roundBefore == 0 {
		return fmt.Errorf("no pre-re-review round was recorded, so \"a new review round is opened\" would compare against nothing")
	}
	number, requestedBy, err := h.latestRound(ctx, world, world.workBranch)
	if err != nil {
		return err
	}
	if number != world.roundBefore+1 {
		return fmt.Errorf("the work branch is on round %d, want %d", number, world.roundBefore+1)
	}
	if requestedBy != "admin" {
		return fmt.Errorf("round %d was requested by %q, want \"admin\"", number, requestedBy)
	}
	return nil
}

// stepThePriorVerdictsAreMarkedStale asserts every verdict that existed
// before the new round now reads as stale, and still reads as the same
// reviewer's same outcome -- staleness is derived from the round
// comparison, so a verdict that vanished, or one whose round moved with
// the branch, would be a different bug this would catch rather than hide.
func (h *acceptanceHarness) stepThePriorVerdictsAreMarkedStale(ctx context.Context) error {
	world := worldFrom(ctx)
	if len(world.verdictsBefore) == 0 {
		return fmt.Errorf("no verdicts were recorded before the new round, so \"the prior verdicts are marked stale\" would hold vacuously")
	}
	verdicts, err := h.listVerdicts(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	for _, before := range world.verdictsBefore {
		found, err := verdictByReviewer(verdicts, before.Reviewer)
		if err != nil {
			return err
		}
		if found.Outcome != before.Outcome || found.Round != before.Round {
			return fmt.Errorf("%s's verdict reads back as %+v, want their prior %+v", before.Reviewer, found, before)
		}
		if !found.Stale {
			return fmt.Errorf("%s's prior verdict (round %d) is not marked stale", before.Reviewer, found.Round)
		}
	}
	return nil
}

// stepItNoLongerAppearsInTheProposalQueue asserts the branch has left the
// queue -- re-reading it, never reusing a cached page -- while still
// EXISTING, so the absence is a queue decision rather than a deleted row.
//
// It refuses to answer at all unless this scenario observed the branch IN
// the queue earlier (world.queued, set by the "a proposal ..." Given and
// by the advance's own precondition check). Absence proves a removal only
// if presence was established first.
func (h *acceptanceHarness) stepItNoLongerAppearsInTheProposalQueue(ctx context.Context) error {
	world := worldFrom(ctx)
	if !world.queued {
		return fmt.Errorf("this scenario never observed %s/%s in the proposal queue, so its absence now proves no removal", world.repo(), world.workBranch)
	}
	if _, _, err := h.workBranchStateConflict(ctx, world.repoID, world.workBranch); err != nil {
		return err
	}
	proposals, err := h.listProposals(ctx)
	if err != nil {
		return err
	}
	if _, ok := proposalFor(proposals, world.repo(), world.workBranch); ok {
		return fmt.Errorf("work branch %s/%s is still in the proposal queue", world.repo(), world.workBranch)
	}
	return nil
}

// stepItIsStillListedMarkedNotAcceptable is loam-u84g's acceptance
// assertion, and it is deliberately the exact negation of the step above.
//
// A conflicting target advance demotes the branch to draft and leaves its
// approve non-stale (docs/git-spec.md -> Target Advances & Catch-Up). This
// queue used to DROP such a branch, and an operator cannot act on a row that
// is not there: an approved P1 fix missed a release because the console
// offered every proposal it knew of and this one was simply not among them.
// The branch must therefore be PRESENT and marked, which is two separate
// facts -- listing it while still claiming it is acceptable would be worse
// than dropping it, since the admin would press a button the server refuses.
//
// world.queued is required first, exactly as the removal step requires it,
// so "still listed" cannot pass on a branch that was never in the queue.
func (h *acceptanceHarness) stepItIsStillListedMarkedNotAcceptable(ctx context.Context) error {
	world := worldFrom(ctx)
	if !world.queued {
		return fmt.Errorf("this scenario never observed %s/%s in the proposal queue, so its presence now proves nothing was preserved", world.repo(), world.workBranch)
	}
	proposals, err := h.listProposals(ctx)
	if err != nil {
		return err
	}
	proposal, ok := proposalFor(proposals, world.repo(), world.workBranch)
	if !ok {
		return fmt.Errorf("work branch %s/%s dropped out of the proposal queue when its target conflicted; a demoted branch with a live approve must stay visible (%d queued)", world.repo(), world.workBranch, len(proposals))
	}
	if proposal.GetAcceptable() {
		return fmt.Errorf("work branch %s/%s is listed as acceptable, but AcceptProposal refuses a demoted branch -- the console would offer a button that cannot work", world.repo(), world.workBranch)
	}
	if got := proposal.GetWorkBranch().GetConflict(); got != loamv1.WorkBranchConflict_WORK_BRANCH_CONFLICT_RESET {
		return fmt.Errorf("queued proposal %s carries conflict %s, want RESET -- the reason the row is blocked must travel with it", world.workBranch, got)
	}
	return nil
}

// stepAcceptingItIsStillRejected proves listing the blocked branch widened
// nothing: the accept gate answers exactly as it did when the branch was
// hidden. Without this the scenario could be satisfied by a change that made
// the queue honest and the gate lax at the same time.
func (h *acceptanceHarness) stepAcceptingItIsStillRejected(ctx context.Context) error {
	world := worldFrom(ctx)
	_, err := acceptProposal(ctx, h.newProposalServiceClient(), world.repo(), world.workBranch)
	if err == nil {
		return fmt.Errorf("accepting demoted work branch %s/%s succeeded; it must stay refused", world.repo(), world.workBranch)
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		return fmt.Errorf("accepting demoted work branch %s/%s failed with %s, want failed_precondition: %w", world.repo(), world.workBranch, got, err)
	}
	return nil
}

// stepTheReasonIsRecordedOnTheWorkBranch asserts close_reason holds the
// exact body the close RPC sent.
func (h *acceptanceHarness) stepTheReasonIsRecordedOnTheWorkBranch(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.closeReason == "" {
		return fmt.Errorf("this scenario closed nothing with a reason, so there is none to have been recorded")
	}
	var reason *string
	err := h.server.pool.QueryRow(ctx,
		`SELECT close_reason FROM work_branches WHERE repo_id = $1 AND name = $2`, world.repoID, world.workBranch).Scan(&reason)
	if err != nil {
		return fmt.Errorf("reading close_reason for work branch %s: %w", world.workBranch, err)
	}
	if reason == nil {
		return fmt.Errorf("work branch %s was closed with no recorded reason", world.workBranch)
	}
	if *reason != world.closeReason {
		return fmt.Errorf("work branch %s records the close reason %q, want %q", world.workBranch, *reason, world.closeReason)
	}
	return nil
}

// stepTheUpstreamPRIsClosed asserts the pull request Loam opened is now
// closed ON THE FORGE, and specifically closed rather than merged -- the
// two are different terminal states there and only one of them is what
// closing a work branch is supposed to produce.
func (h *acceptanceHarness) stepTheUpstreamPRIsClosed(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.upstreamPRNumber == 0 {
		return fmt.Errorf("no upstream PR was recorded for %s/%s, so \"the upstream PR is closed\" would hold vacuously", world.repo(), world.workBranch)
	}
	state, err := h.forgeClient.GetPRState(ctx, world.repo(), world.upstreamPRNumber)
	if err != nil {
		return fmt.Errorf("reading the state of upstream PR %s#%d: %w", world.repo(), world.upstreamPRNumber, err)
	}
	if state != "closed" {
		return fmt.Errorf("upstream PR %s#%d is %q, want \"closed\"", world.repo(), world.upstreamPRNumber, state)
	}
	return nil
}

// stepTheNextSyncSetsTheWorkBranchToState runs one sync tick and asserts
// the branch reached want -- having first asserted it was NOT already
// there, so the step cannot pass on a state that predated the tick.
func (h *acceptanceHarness) stepTheNextSyncSetsTheWorkBranchToState(ctx context.Context, want string) error {
	world := worldFrom(ctx)
	before, _, err := h.workBranchStateConflict(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if before == want {
		return fmt.Errorf("work branch %s was already in state %q before the sync, so this step could not observe the sync setting it", world.workBranch, want)
	}
	if err := h.stepTheNextSyncRuns(ctx); err != nil {
		return err
	}
	return h.requireWorkBranchState(ctx, world, world.workBranch, want)
}

// stepTheExistingUpstreamPRIsUpdatedInPlace asserts the second accept
// reused the first one's pull request and fast-forwarded its branch:
// same URL, same recorded number, and an upstream loam/<name> that now
// matches the caught-up tip AND has genuinely MOVED from where the first
// accept left it.
//
// That last clause is what separates "updated in place" from "did
// nothing": without it, an accept that skipped its push entirely would
// satisfy every other assertion here.
func (h *acceptanceHarness) stepTheExistingUpstreamPRIsUpdatedInPlace(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.upstreamPRNumber == 0 || world.firstUpstreamBranchSHA == "" {
		return fmt.Errorf("this scenario recorded no first accept, so there is no existing pull request to have been updated")
	}
	if world.lastAcceptPRURL != world.upstreamPRURL {
		return fmt.Errorf("the re-accept returned PR URL %q, want the existing %q", world.lastAcceptPRURL, world.upstreamPRURL)
	}
	number, err := h.recordedUpstreamPRNumber(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if number != world.upstreamPRNumber {
		return fmt.Errorf("the work branch now records PR #%d, want the original #%d", number, world.upstreamPRNumber)
	}
	upstreamBranch := "loam/" + world.workBranch
	upstreamSHA, err := h.upstreamRefSHA(ctx, world, "refs/heads/"+upstreamBranch)
	if err != nil {
		return err
	}
	mirrorSHA, err := mirrorRefSHA(world.mirrorDir, refnames.WorkBranch(world.workBranch))
	if err != nil {
		return fmt.Errorf("reading the mirror's %s: %w", refnames.WorkBranch(world.workBranch), err)
	}
	if upstreamSHA != mirrorSHA {
		return fmt.Errorf("upstream %s is at %s, want the caught-up tip %s", upstreamBranch, upstreamSHA, mirrorSHA)
	}
	if upstreamSHA == world.firstUpstreamBranchSHA {
		return fmt.Errorf("upstream %s never moved from %s, so the re-accept pushed nothing", upstreamBranch, world.firstUpstreamBranchSHA)
	}
	return nil
}

// stepNoNewUpstreamPRIsCreated asserts the forge's whole pull-request
// history for this repo is still the one Loam opened on the first accept.
//
// The check is on the forge's own record, including closed and merged
// pull requests, because the count IS the claim: an open-only view would
// empty itself the moment that one PR concluded and would then report
// "none" and pass for the wrong reason. The follow-on GetPRState confirms
// the forge never even ALLOCATED a further number, which is what a second
// CreatePR would have done.
func (h *acceptanceHarness) stepNoNewUpstreamPRIsCreated(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.upstreamPRNumber == 0 {
		return fmt.Errorf("this scenario recorded no pull request at all, so \"no new upstream PR is created\" would hold vacuously")
	}
	pr, err := h.theOneUpstreamPR(world)
	if err != nil {
		return err
	}
	if pr.Number != world.upstreamPRNumber {
		return fmt.Errorf("the forge's only pull request is #%d, want the original #%d", pr.Number, world.upstreamPRNumber)
	}
	if _, err := h.forgeClient.GetPRState(ctx, world.repo(), world.upstreamPRNumber+1); !errors.Is(err, forge.ErrRepoNotFound) {
		return fmt.Errorf("the forge answered for pull request #%d (err %v), so a second one was allocated", world.upstreamPRNumber+1, err)
	}
	return nil
}

// stepPriorVerdictsAreNotYetStale pins the timing loam-di9q settled:
// staleness is DERIVED from the round number
// (review_rounds.sql's is_current_round compares against MAX(number)), and
// MarkConflicted demotes without opening a round -- so at the moment of
// demotion the verdicts' round is still the max and they read as CURRENT.
// They go stale only when the catch-up restore opens round 2, which the
// next scenario asserts.
//
// The precondition is what stops this holding vacuously: a branch with no
// verdicts at all, or one already sitting on a later round, would satisfy
// "not stale" for the wrong reason. Both are rejected explicitly.
//
// Nothing can act on the branch during this window regardless:
// AcceptProposal gates independently on state = reviewed AND
// conflict = none (internal/handler/proposal/proposal.go), and a demoted
// branch fails both.
func (h *acceptanceHarness) stepPriorVerdictsAreNotYetStale(ctx context.Context) error {
	world := worldFrom(ctx)
	number, _, err := h.latestRound(ctx, world, world.workBranch)
	if err != nil {
		return err
	}
	if number != 1 {
		return fmt.Errorf("the branch is on round %d, so \"not yet stale\" would not be about the demotion; want round 1", number)
	}
	verdicts, err := h.listVerdicts(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if len(verdicts) == 0 {
		return fmt.Errorf("work branch %s carries no verdicts, so \"not yet stale\" would hold vacuously", world.workBranch)
	}
	for _, v := range verdicts {
		if v.Stale {
			return fmt.Errorf("verdict %+v reads stale after a demotion that opened no round; staleness must follow the catch-up restore, not the demotion (loam-di9q)", v)
		}
	}
	return nil
}

// stepItIsFlaggedAsConflicted asserts the conflict column alone, leaving
// the state assertion to its own Then. The demotion sets both, and
// work-branch-lifecycle.feature deliberately checks them separately so a
// change that moved the state but dropped the flag (or the reverse) fails
// on the half that actually broke.
//
// "reset" rather than "flagged" is the expected value here: the branch was
// reviewed, so it was DEMOTED, and workbranchstore distinguishes a demoted
// branch (conflict = reset) from one that was already draft and merely
// gained the flag (conflict = flagged). loam-giq.6's restore rule turns on
// exactly that distinction.
func (h *acceptanceHarness) stepItIsFlaggedAsConflicted(ctx context.Context) error {
	world := worldFrom(ctx)
	_, conflict, err := h.workBranchStateConflict(ctx, world.repoID, world.workBranch)
	if err != nil {
		return err
	}
	if conflict != "reset" {
		return fmt.Errorf("work branch %s carries conflict %q, want \"reset\" -- a reviewed branch is DEMOTED by a conflicting advance, not merely flagged", world.workBranch, conflict)
	}
	return nil
}

// stepTheTargetAdvancesWithConflictingChanges is the bare WHEN, for the
// scenario that asserts the demotion's own three consequences separately
// (state, conflict flag, verdict staleness) rather than folding them into
// one Given the way stepAConflictingTargetAdvanceResetIt does.
//
// It deliberately asserts NOTHING itself: each Then that follows is the
// assertion, and duplicating them here would mean a broken demotion failed
// on the When with a message about the wrong thing.
func (h *acceptanceHarness) stepTheTargetAdvancesWithConflictingChanges(ctx context.Context) error {
	return h.advanceTargetConflictingly(ctx, worldFrom(ctx))
}
