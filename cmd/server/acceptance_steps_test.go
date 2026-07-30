//go:build acceptance

// Step definitions. registerCloneAndPushSteps implements
// features/clone-and-push.feature's Background plus the four scenarios
// this bead demonstrates genuinely passing (see acceptance_test.go's own
// doc comment for how to run them, and this bead's report for which four
// and why). registerVocabularySteps implements docs/testing-spec.md Layer
// 1's core step-vocabulary table verbatim -- loam-li0.5's own scope, per
// its NOTES: infrastructure that makes every future @wip removal
// resolvable, not a claim that every row is exercised by a scenario in
// this suite's current, @wip-filtered green run.
//
// features/sync.feature's own steps live in acceptance_sync_test.go
// (loam-a16), which is where "the next sync runs" first became a genuine
// five-step Mirror Sync cycle rather than a tick into four
// not-implemented stand-ins.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cucumber/godog"

	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/refnames"
)

// registerCloneAndPushSteps wires every step clone-and-push.feature's
// Background and its four demonstrated scenarios need.
func (h *acceptanceHarness) registerCloneAndPushSteps(sc *godog.ScenarioContext) {
	sc.Step(`^the repo "([^"]*)" is enrolled with target branch "([^"]*)"$`, h.stepRepoIsEnrolled)
	sc.Step(`^I am the author agent "([^"]*)"$`, h.stepIAmTheAuthorAgent)
	sc.Step(`^I have started the work branch "([^"]*)"$`, h.stepIHaveStartedTheWorkBranch)
	sc.Step(`^I clone "([^"]*)" at "([^"]*)"$`, h.stepIClone)
	sc.Step(`^the clone is placed at "([^"]*)"$`, h.stepCloneIsPlacedAt)
	sc.Step(`^its only remote is the Loam server$`, h.stepCloneHasOneRemote)
	sc.Step(`^its git author is set to my agent identity$`, h.stepCloneAuthorIsAgentIdentity)
	sc.Step(`^my identity is carried on every git operation from the clone$`, h.stepCloneCarriesIdentityHeaders)
	sc.Step(`^I am in the clone checked out on "([^"]*)"$`, h.stepIAmInTheCloneCheckedOutOn)
	sc.Step(`^I commit and push$`, h.stepICommitAndPush)
	sc.Step(`^my commits reach the server on "([^"]*)"$`, h.stepMyCommitsReachTheServerOn)
	sc.Step(`^I push to the target branch "([^"]*)"$`, h.stepIPushToTheTargetBranch)
	sc.Step(`^the push is rejected as read-only$`, h.stepThePushIsRejectedAsReadOnly)
	sc.Step(`^a clone whose git configuration carries no agent identity$`, h.stepACloneWithNoAgentIdentity)
	sc.Step(`^I push from it$`, h.stepIPushFromIt)
	sc.Step(`^the push is rejected$`, h.stepThePushIsRejected)
	sc.Step(`^I push a branch that is not a registered work branch$`, h.stepIPushABranchThatIsNotARegisteredWorkBranch)
	sc.Step(`^the work branch "([^"]*)" belongs to another agent$`, h.stepTheWorkBranchBelongsToAnotherAgent)
	sc.Step(`^I push to "([^"]*)"$`, h.stepIPushToNamedBranch)
	sc.Step(`^I have rewritten the history of "([^"]*)" locally$`, h.stepIHaveRewrittenTheHistoryOfLocally)
	sc.Step(`^I force push$`, h.stepIForcePush)
}

// stepRepoIsEnrolled seeds world's upstream repo on the shared fake forge
// and then its repo row plus registered target branch, pointed at that
// upstream (see acceptance_sync_test.go's seedUpstreamRepo and
// acceptance_seed_test.go's insertRepoRow).
//
// The MIRROR is deliberately not built here. clone-and-push.feature's
// scenarios build theirs in stepIHaveStartedTheWorkBranch, once both the
// target branch and the work branch it also seeds are known, so it is
// created once with its full, final set of branches; sync.feature's
// scenarios instead clone theirs from the real upstream, in whichever
// Given first needs it (ensureMirrorFromUpstream). Building one here
// would collide with both.
func (h *acceptanceHarness) stepRepoIsEnrolled(ctx context.Context, repo, branch string) error {
	world := worldFrom(ctx)
	group, name, ok := strings.Cut(repo, "/")
	if !ok {
		return fmt.Errorf("repo %q must be shaped like <group>/<repo_name>", repo)
	}
	world.repoGroup, world.repoName, world.targetBranch = group, name, branch
	if err := h.seedUpstreamRepo(ctx, world); err != nil {
		return err
	}
	repoID, err := h.insertRepoRow(ctx, world)
	if err != nil {
		return err
	}
	world.repoID = repoID
	return nil
}

// stepIAmTheAuthorAgent records the literal agent identity a scenario
// names (e.g. "grace-hopper-3-author"), overriding newAcceptanceWorld's
// generated default so later steps' assertions can match the scenario's
// own literal text.
//
// It ALSO splits that literal back into the LOAM_AGENT_* triple that
// produces it, for replies.feature's author-side steps
// (acceptance_review_test.go), which drive the CLI as an explicit actor
// rather than as world's single agent identity. world.agentName/agentID/
// agentRole are left exactly as they were: clone-and-push.feature's
// identity-header and git-author assertions read those three fields
// against themselves, and re-deriving them here would change what those
// scenarios compare.
func (h *acceptanceHarness) stepIAmTheAuthorAgent(ctx context.Context, agentName string) error {
	world := worldFrom(ctx)
	world.agentName = agentName
	author, err := parseAcceptanceActor(agentName)
	if err != nil {
		return err
	}
	if author.role != "author" {
		return fmt.Errorf("agent %q has role %q, but this step names an AUTHOR", agentName, author.role)
	}
	world.author = author
	return nil
}

// stepIHaveStartedTheWorkBranch seeds world's work_branches row, builds the
// bare mirror on disk with both the target and work branches present, and
// reconciles it (installs the real pre-receive hook) -- see
// acceptance_seed_test.go's seedBareMirrorWithBranches and
// acceptanceHarness.reconcileSeededMirror.
func (h *acceptanceHarness) stepIHaveStartedTheWorkBranch(ctx context.Context, workBranch string) error {
	world := worldFrom(ctx)
	world.setPrimaryWorkBranch(workBranch)
	if err := h.insertWorkBranchRow(ctx, world.repoID, workBranch, world.targetBranch, "draft", world.agentIdentifier()); err != nil {
		return err
	}
	mirrorDir, err := seedBareMirrorWithBranches(ctx, h.server.dataDir, world.repo(), world.targetBranch, workBranch)
	if err != nil {
		return err
	}
	world.mirrorDir = mirrorDir
	return h.reconcileSeededMirror(ctx, mirrorDir)
}

// stepIClone is "When I clone ... at ...": exactly one driver call to the
// compiled loam binary (testing-spec Layer 1's Author actor driver).
func (h *acceptanceHarness) stepIClone(ctx context.Context, repo, branch string) error {
	world := worldFrom(ctx)
	world.lastCLI = h.runLoamCLI(world, "clone", repo, branch)
	world.clonePath = filepath.Join(world.workspace, world.repoName)
	return nil
}

// stepCloneIsPlacedAt asserts the clone succeeded and landed at the exact
// requested relative path.
func (h *acceptanceHarness) stepCloneIsPlacedAt(ctx context.Context, relPath string) error {
	world := worldFrom(ctx)
	if world.lastCLI.exitCode != 0 {
		return fmt.Errorf("loam clone exited %d, want 0\nstdout: %s\nstderr: %s", world.lastCLI.exitCode, world.lastCLI.stdout, world.lastCLI.stderr)
	}
	want := filepath.Join(world.workspace, strings.TrimPrefix(relPath, "./"))
	if want != world.clonePath {
		return fmt.Errorf("clone landed at %s, want %s", world.clonePath, want)
	}
	return assertDirExists(world.clonePath)
}

// stepCloneHasOneRemote asserts the clone's only remote is the Loam
// server (docs/git-spec.md -> The CLI's Role: single-remote bootstrap).
func (h *acceptanceHarness) stepCloneHasOneRemote(ctx context.Context) error {
	world := worldFrom(ctx)
	remotes := gitRemotes(world.clonePath)
	if len(remotes) != 1 {
		return fmt.Errorf("expected exactly one remote, found %d: %v", len(remotes), remotes)
	}
	return nil
}

// stepCloneAuthorIsAgentIdentity asserts the clone's user.name is set to
// the acting agent's own identity, so commits are attributed correctly.
func (h *acceptanceHarness) stepCloneAuthorIsAgentIdentity(ctx context.Context) error {
	world := worldFrom(ctx)
	got, ok := gitConfigGet(world.clonePath, "user.name")
	if !ok || got != world.agentName {
		return fmt.Errorf("clone user.name = %q (present=%v), want %q", got, ok, world.agentName)
	}
	return nil
}

// stepCloneCarriesIdentityHeaders asserts the three Loam-Agent-* headers
// are configured on the clone, so every subsequent git operation --
// including a plain `git push` with no loam involvement -- carries the
// agent's identity.
func (h *acceptanceHarness) stepCloneCarriesIdentityHeaders(ctx context.Context) error {
	world := worldFrom(ctx)
	headers := gitConfigGetAll(world.clonePath, "http.extraheader")
	want := []string{
		"Loam-Agent-Name: " + world.agentName,
		"Loam-Agent-Id: " + world.agentID,
		"Loam-Agent-Role: " + world.agentRole,
	}
	for _, w := range want {
		if !containsString(headers, w) {
			return fmt.Errorf("clone http.extraheader missing %q (has %v)", w, headers)
		}
	}
	return nil
}

// stepIAmInTheCloneCheckedOutOn is "Given I am in the clone checked out on
// ...": lazily clones (via the same single CLI driver call stepIClone
// uses) if this scenario has not already, then confirms the requested
// branch is the one checked out.
func (h *acceptanceHarness) stepIAmInTheCloneCheckedOutOn(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	h.ensureCloned(world)
	branches := gitLocalBranches(world.clonePath)
	if len(branches) != 1 || branches[0] != branch {
		return fmt.Errorf("clone is checked out on %v, want exactly [%s]", branches, branch)
	}
	return nil
}

// stepICommitAndPush is the core vocabulary row "I commit and push": one
// write, one git add, one git commit, one git push -- plain git inside the
// actor's own clone, no loam involvement, per docs/testing-spec.md Layer
// 1's step-vocabulary table.
func (h *acceptanceHarness) stepICommitAndPush(ctx context.Context) error {
	world := worldFrom(ctx)
	h.ensureCloned(world)
	// The refspec is the BARE work-branch name, not "HEAD": a
	// destination-less refspec is the only shape that consults
	// remote.origin.push, which is what maps the name to its reserved ref
	// path (refnames.ClientPushRefspec, written by `loam clone`).
	// "HEAD" resolves its destination by name instead and would aim at the
	// unregistered refs/heads/<name> -- which is exactly the mistake
	// docs/git-spec.md's "wrong ref path" rejection row exists for.
	return world.writeCommitAndPush("agent-change.txt", "acceptance change", "acceptance: agent change", world.workBranch)
}

// stepMyCommitsReachTheServerOn asserts the push landed: the mirror's
// branch ref now points at the clone's own HEAD.
func (h *acceptanceHarness) stepMyCommitsReachTheServerOn(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	if world.lastGitErr != nil {
		return fmt.Errorf("push failed: %v\n%s", world.lastGitErr, world.lastGitOutput)
	}
	ref := refnames.WorkBranch(branch)
	mirrorSHA, err := mirrorRefSHA(world.mirrorDir, ref)
	if err != nil {
		return fmt.Errorf("reading mirror ref %s: %w", ref, err)
	}
	cloneSHA, err := cloneHeadSHA(world.clonePath)
	if err != nil {
		return fmt.Errorf("reading clone HEAD: %w", err)
	}
	if mirrorSHA != cloneSHA {
		return fmt.Errorf("mirror's %s (%s) does not match the pushed commit (%s)", ref, mirrorSHA, cloneSHA)
	}
	return nil
}

// stepIPushToTheTargetBranch lazily clones (on the scenario's own work
// branch) if needed, commits a genuine change (a no-op push, where the
// destination ref already matches HEAD, never even reaches the server's
// pre-receive hook -- git reports "Everything up-to-date" and exits 0
// without sending a pack, which would silently defeat this scenario's own
// point), then pushes the clone's HEAD directly to the named target
// branch ref -- expected to be rejected as read-only.
func (h *acceptanceHarness) stepIPushToTheTargetBranch(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	h.ensureCloned(world)
	return world.writeCommitAndPush("target-branch-change.txt", "attempted target change", "acceptance: attempted target-branch push", "HEAD:refs/heads/"+branch)
}

// stepThePushIsRejectedAsReadOnly asserts the last push failed with the
// documented read-only reason (docs/git-spec.md's "Ref Policy (push)"
// rejection-reasons table).
func (h *acceptanceHarness) stepThePushIsRejectedAsReadOnly(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastGitErr == nil {
		return fmt.Errorf("push to the target branch succeeded, want rejection\n%s", world.lastGitOutput)
	}
	if !strings.Contains(world.lastGitOutput, "read-only") {
		return fmt.Errorf("push rejection did not mention read-only:\n%s", world.lastGitOutput)
	}
	return nil
}

// stepACloneWithNoAgentIdentity lazily clones if needed, then strips every
// Loam-Agent-* identity header from the clone's git configuration.
func (h *acceptanceHarness) stepACloneWithNoAgentIdentity(ctx context.Context) error {
	world := worldFrom(ctx)
	h.ensureCloned(world)
	if _, err := runPlainGit(world.clonePath, "config", "--unset-all", "http.extraheader"); err != nil {
		return fmt.Errorf("stripping identity headers: %w", err)
	}
	return nil
}

// stepIPushFromIt commits a small change and pushes from the (now
// identity-stripped) clone.
func (h *acceptanceHarness) stepIPushFromIt(ctx context.Context) error {
	world := worldFrom(ctx)
	return world.writeCommitAndPush("no-identity-change.txt", "no identity", "acceptance: no identity", "HEAD:"+refnames.WorkBranch(world.workBranch))
}

// stepIPushABranchThatIsNotARegisteredWorkBranch pushes the clone's HEAD to
// a plain refs/heads/ ref that names no registered work branch of this
// repo -- docs/git-spec.md "Ref Policy (push)" rule 1's "unknown ref"
// rejection for a brand-new ref (RefUpdate.OldSHA all-zero, since the ref
// does not exist in the mirror at all: internal/refpolicy's
// unknownOrReadOnlyVerdict picks the "create one with 'work start'" reason
// on exactly that condition). The explicit destination refspec is the same
// technique stepIPushToTheTargetBranch and stepIPushFromIt already use, so
// this reaches the named ref regardless of what remote.origin.push maps a
// bare `git push` to.
func (h *acceptanceHarness) stepIPushABranchThatIsNotARegisteredWorkBranch(ctx context.Context) error {
	world := worldFrom(ctx)
	h.ensureCloned(world)
	return world.writeCommitAndPush("unregistered-branch-change.txt", "unregistered branch attempt", "acceptance: push to an unregistered branch", "HEAD:refs/heads/not-a-registered-work-branch")
}

// acceptanceAnotherAgentAuthor is a work_branches.author value guaranteed to
// differ from any clone-and-push.feature scenario's own agentIdentifier()
// (acceptance_world_test.go) -- always shaped "<agentName>-<agentID>-author"
// for this file's single-actor scenarios. It stands in for "another agent"
// without needing a real, parseable LOAM_AGENT_* identity: the only thing
// internal/refpolicy.evaluateOne does with work_branches.author is compare
// it, as a string, against the pushing agent's own httpauth.Identity.
// Identifier().
const acceptanceAnotherAgentAuthor = "someone-else-1-author"

// stepTheWorkBranchBelongsToAnotherAgent seeds a SECOND work_branches row --
// name, distinct from world's own Background-seeded branch -- owned by an
// author that is not this scenario's acting agent: docs/git-spec.md "Ref
// Policy (push)" rule 2, "only the author may push". No mirror ref is
// created for it. internal/refpolicy.evaluateOne's author check runs
// against the Postgres row alone, before any git-level existence or
// fast-forward check, so a push CREATING this ref (old_sha all-zero) is
// rejected on ownership just as surely as one updating an existing ref
// would be -- there is no need to also build the ref in the bare mirror for
// this scenario to exercise the real rule.
func (h *acceptanceHarness) stepTheWorkBranchBelongsToAnotherAgent(ctx context.Context, name string) error {
	world := worldFrom(ctx)
	return h.insertWorkBranchRow(ctx, world.repoID, name, world.targetBranch, "draft", acceptanceAnotherAgentAuthor)
}

// stepIPushToNamedBranch pushes the clone's HEAD to a NAMED work branch's
// reserved ref path explicitly (refnames.WorkBranch(name)), regardless of
// which branch the clone is actually checked out on -- the same explicit-
// refspec technique stepIPushFromIt already uses. It backs both "Only the
// author may push to a work branch" (name belongs to another agent, per
// stepTheWorkBranchBelongsToAnotherAgent above) and "A terminal work branch
// rejects pushes" (name is this scenario's own branch, forced into a
// terminal state by acceptance_review_test.go's
// stepTheWorkBranchNamedIsInState, which is already wired into this suite
// via registerReviewSteps and shares this exact Gherkin sentence -- "the
// work branch \"...\" is in state \"...\"" -- so no new step is needed for
// that Given at all).
func (h *acceptanceHarness) stepIPushToNamedBranch(ctx context.Context, name string) error {
	world := worldFrom(ctx)
	h.ensureCloned(world)
	return world.writeCommitAndPush(name+"-push-attempt.txt", "push attempt to "+name, "acceptance: push attempt to "+name, "HEAD:"+refnames.WorkBranch(name))
}

// stepIHaveRewrittenTheHistoryOfLocally amends the clone's current HEAD
// commit -- same parent (none: it is the fixture's own root commit), new
// commit SHA -- so it is no longer a fast-forward of the branch's current
// tip on the server. "When I force push" is what exercises the
// consequence: docs/git-spec.md's own Enforcement Mechanics section
// attributes force-push rejection to STOCK GIT's receive.
// denyNonFastForwards (internal/mirrorreconcile.ReconcileMirror sets it on
// every mirror), explicitly NOT to internal/refpolicy's rules 1-3 -- this
// step's name is unused beyond documenting which branch the amend targets;
// the clone is always checked out on exactly one branch.
func (h *acceptanceHarness) stepIHaveRewrittenTheHistoryOfLocally(ctx context.Context, name string) error {
	world := worldFrom(ctx)
	h.ensureCloned(world)
	if out, err := runPlainGit(world.clonePath, "commit", "--amend", "--quiet", "--no-edit", "--allow-empty"); err != nil {
		return fmt.Errorf("amending local history of %s: %w\n%s", name, err, out)
	}
	return nil
}

// stepIForcePush force-pushes the clone's (now rewritten) HEAD to its own
// work branch's reserved ref path, bypassing git's own local fast-forward
// check with --force so the push actually reaches the server rather than
// being refused client-side before any network round trip.
func (h *acceptanceHarness) stepIForcePush(ctx context.Context) error {
	world := worldFrom(ctx)
	out, err := runPlainGit(world.clonePath, "push", "--force", "origin", "HEAD:"+refnames.WorkBranch(world.workBranch))
	world.lastGitOutput, world.lastGitErr = out, err
	return nil
}

// stepThePushIsRejected is the generic push-rejection assertion several
// clone-and-push.feature scenarios share (the more specific rejection-
// reason scenarios use their own dedicated Then step, e.g.
// stepThePushIsRejectedAsReadOnly). A non-zero exit is necessary but not
// sufficient: docs/git-spec.md's "Enforcement Mechanics" section names TWO
// distinct, legitimate rejection channels, and a genuine policy rejection
// must carry the fingerprint of one of them, so a push that fails for some
// unrelated reason (a broken fixture, a network error) cannot be mistaken
// for either:
//
//   - Loam's own pre-receive hook (the ref-policy table: read-only ref,
//     unknown ref, wrong ref path, not the author, terminal state), whose
//     reasons are `loam:`-prefixed and, per git's own smart-HTTP client
//     behavior, arrive on the pushing side as "remote: loam: ...".
//   - STOCK GIT's own receive.denyNonFastForwards/receive.denyDeletes
//     (mirrorreconcile.ReconcileMirror sets both on every mirror),
//     explicitly carved out by that same doc section as separate from
//     Loam's pre-receive rules ("force pushes and ref deletions are
//     rejected by git itself, with git's own messages") -- so it is never
//     `loam:`-prefixed and an earlier version of this step wrongly
//     treated it as an "unrelated reason" rather than the second
//     legitimate channel it is. "Force pushes are rejected" is exactly
//     this case: git's own message reads "remote: error: denying
//     non-fast-forward ...".
func (h *acceptanceHarness) stepThePushIsRejected(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastGitErr == nil {
		return fmt.Errorf("push succeeded, want rejection\n%s", world.lastGitOutput)
	}
	if strings.Contains(world.lastGitOutput, "remote: loam:") {
		return nil
	}
	if strings.Contains(world.lastGitOutput, "denying non-fast-forward") || strings.Contains(world.lastGitOutput, "denying ref deletion") {
		return nil
	}
	return fmt.Errorf("push failed, but not with a recognized Loam or stock-git policy rejection:\n%s", world.lastGitOutput)
}

// ensureCloned clones world's repo at its own work branch via the compiled
// loam binary, unless this scenario has already done so.
func (h *acceptanceHarness) ensureCloned(world *acceptanceWorld) {
	if world.clonePath != "" {
		return
	}
	world.lastCLI = h.runLoamCLI(world, "clone", world.repo(), world.workBranch)
	world.clonePath = filepath.Join(world.workspace, world.repoName)
}

// registerVocabularySteps wires docs/testing-spec.md Layer 1's core
// step-vocabulary table, verbatim, one step per row, each resolving to
// exactly one driver call -- loam-li0.5's own scope. "I commit and push" is
// already registered by registerCloneAndPushSteps above (stepICommitAndPush
// IS this row; clone-and-push.feature's own scenario exercises it for
// real; it is not duplicated here).
func (h *acceptanceHarness) registerVocabularySteps(sc *godog.ScenarioContext) {
	sc.Step(`^I request review$`, h.stepIRequestReview)
	sc.Step(`^the upstream PR merges$`, h.stepTheUpstreamPRMerges)
	sc.Step(`^the next sync runs$`, h.stepTheNextSyncRuns)
	sc.Step(`^after ingestion$`, h.stepAfterIngestion)
	sc.Step(`^I accept it$`, h.stepIAcceptIt)
}

// acceptanceWorkBranchOutput mirrors internal/cli/commands_work.go's own
// workBranchOutput JSON shape (repo, name, target, title, state) -- `loam
// work request-review`'s success output -- reproduced here rather than
// imported, since internal/cli's type is unexported. Description is
// additionally `work show`'s own field (internal/cli/commands_work_read.go
// -> workShowOutput); it decodes as the zero value for every OTHER command
// this type also serves, which carries no such key at all.
type acceptanceWorkBranchOutput struct {
	Repo        string `json:"repo"`
	Name        string `json:"name"`
	Target      string `json:"target"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
}

// stepIRequestReview is the core vocabulary row "I request review": one
// `loam work request-review <repo> <work-branch>` invocation, as this
// scenario's own author actor (world.author) -- not runLoamCLI's single
// world identity, which clone-and-push.feature's Background happens to
// leave self-consistent but which work-branch-lifecycle.feature's own
// Background ("I am the author agent ...", acceptance_steps_test.go's
// stepIAmTheAuthorAgent) does not, since that step overrides world.agentName
// alone and leaves agentID/agentRole at their generated defaults.
//
// Unlike a strict "assert success" step, this one tolerates a non-zero
// exit: features/work-branch-lifecycle.feature uses this exact sentence for
// BOTH the success path (a titled, described branch moving to reviewable)
// and the precondition-refusal path (an untitled draft branch), and godog
// dispatches on sentence text alone, so one function has to serve both --
// the refusal itself is asserted by the following Then
// (stepTheRequestIsRejectedWithAPreconditionError, acceptance_proposal_test.go),
// which reads the same world.lastCLI this leaves behind. A scenario that
// expected success but got a rejection still fails, just one step later, on
// the JSON its own Then cannot find -- this is not the "silent pass on any
// outcome" trap docs/testing-spec.md's row exists to rule out; per its own
// wording ("parse JSON, assert exit code") the exit code IS asserted, just
// by whichever Then this sentence's scenario actually uses.
func (h *acceptanceHarness) stepIRequestReview(ctx context.Context) error {
	world := worldFrom(ctx)
	world.requestReviews++
	world.lastCLI = h.runLoamAs(world, world.author, "", "work", "request-review", world.repo(), world.workBranch)
	if world.lastCLI.exitCode != 0 {
		return nil
	}
	var out acceptanceWorkBranchOutput
	if err := json.Unmarshal([]byte(world.lastCLI.stdout), &out); err != nil {
		return fmt.Errorf("decoding loam work request-review JSON output: %w\nstdout: %s", err, world.lastCLI.stdout)
	}
	world.lastWorkBranch = out
	return nil
}

// stepTheUpstreamPRMerges is the core vocabulary row "the upstream PR
// merges": fakeforge's MergePR control-API call followed by exactly one
// sync tick, per docs/testing-spec.md's own resolution for this row.
func (h *acceptanceHarness) stepTheUpstreamPRMerges(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := h.forge.MergePR(ctx, world.repo(), world.upstreamPRNumber); err != nil {
		return fmt.Errorf("merging upstream PR for %s: %w", world.repo(), err)
	}
	_, err := h.syncHarness.Tick(ctx)
	return err
}

// stepTheNextSyncRuns is the core vocabulary row "the next sync runs": one
// mirrorsync.Scheduler.Tick, run to completion, via the harness's own
// testsched.SyncHarness (see newSyncHarness's doc comment for why its
// Run method is never reachable at all).
//
// Tick's own returned error is ONLY a ListRepos failure
// (mirrorsync.Scheduler.Tick's doc comment): every per-repo cycle failure
// is logged and written to repos.sync_state by the scheduler itself
// (scheduler.go's cycle/ReportError), never propagated back through Tick.
// A step that only checked Tick's return value would pass even though the
// cycle it just ran genuinely errored, so this reads repos.sync_state back
// for world's own repo afterward and fails loudly if it is 'error'.
//
// The one exception is a scenario whose own Given deliberately broke the
// upstream (features/sync.feature's deleted-target-branch and
// unreachable-forge scenarios): those set world.expectSyncError, and
// their own Then steps assert on the recorded error instead. The flag is
// only ever set by the Given that caused the failure, so it can never
// silence a cycle that errored for a reason the scenario did not arrange.
//
// Reading sync_state once, straight after Tick, survives loam-4q2's
// single-sample trap, but no longer for the reason it originally did.
// Tick still blocks until every cycle it started has finished reporting
// (mirrorsync.Scheduler.Tick -> waitIdle, which releases only after the
// terminal report) -- but this column now has a SECOND, asynchronous
// writer: the live in-process ingest.Pool this suite wires
// (loam-c94.13), which flips it to syncing on claim and to idle/error
// when the job resolves, on its own goroutine and its own schedule. A
// single sample of the raw value would now genuinely cycle underneath
// this read.
//
// What makes the read sound is that assertSyncNotErrored classifies by
// AUTHOR, not by timing: it ignores an 'error' carrying
// ingest.SyncErrorPrefix. See its own doc comment for why that leaves no
// false failure reachable regardless of when the ingest writer lands.
func (h *acceptanceHarness) stepTheNextSyncRuns(ctx context.Context) error {
	world := worldFrom(ctx)
	if _, err := h.syncHarness.Tick(ctx); err != nil {
		return err
	}
	if world.expectSyncError {
		return nil
	}
	return h.assertSyncNotErrored(ctx, world.repo())
}

// assertSyncNotErrored reads repos.sync_state and repos.sync_error back for
// repo and fails loudly if the tick just run left it in the 'error' state
// for a reason the SYNC cycle is responsible for.
//
// The author check is what keeps this honest now that two components
// write the column (internal/ingest/pool.go's SyncErrorPrefix block).
// Ownership of sync_state passes to the ingest worker for any tick whose
// step 4 enqueued a job -- internal/mirrorsync/state.Reporter's
// ReportIdle/ReportError deliberately do not write in that case -- so
// after such a tick the value here means "how is the INGEST going", a
// question this step is not asking. An ingest failure (the default
// outcome in this suite: the acceptance server points at an embedder
// that is not running) would otherwise fail every subsequent "the next
// sync runs" step in the scenario, for a reason the step's own error
// message would misattribute to the sync cycle.
//
// No false failure is reachable from the second writer, whenever it
// happens to land: every value the ingest worker can write is either not
// 'error' ('syncing' on claim, 'idle' on success) or is an 'error'
// carrying the prefix. The remaining, deliberate gap is the opposite
// direction -- an ingest 'idle' overwriting a genuine sync 'error' would
// read as a pass -- which is inherent to one column with two authors and
// cannot arise in the two scenarios that assert on a sync error
// (features/sync.feature's deleted-target-branch and unreachable-forge),
// since a cycle that fails at step 1 or 2 never reaches step 4 and so
// never starts an ingest writer for that repo at all.
func (h *acceptanceHarness) assertSyncNotErrored(ctx context.Context, repo string) error {
	var syncState string
	var syncError *string
	err := h.server.pool.QueryRow(ctx, `SELECT sync_state, sync_error FROM repos WHERE name = $1`, repo).Scan(&syncState, &syncError)
	if err != nil {
		return fmt.Errorf("reading sync_state for repo %s after tick: %w", repo, err)
	}
	if syncState != "error" {
		return nil
	}
	message := ""
	if syncError != nil {
		message = *syncError
	}
	if strings.HasPrefix(message, ingest.SyncErrorPrefix) {
		return nil
	}
	return fmt.Errorf("sync cycle for repo %s ended in sync_state=error: %s", repo, message)
}

// stepAfterIngestion is the core vocabulary row "after ingestion": one
// blocking drain of the real, in-process ingest.Pool run() itself
// constructed (see acceptance_test.go's onReady callback), for this
// scenario's own repo.
func (h *acceptanceHarness) stepAfterIngestion(ctx context.Context) error {
	world := worldFrom(ctx)
	return h.ingestHarness.DrainIngestQueue(ctx, mirrorsync.RepoID(world.repo()))
}

// stepIAcceptIt is the core vocabulary row "I accept it": one
// ProposalService.AcceptProposal call as the Admin actor (connect-go
// client, HTTP basic auth). See acceptance_admin_test.go's
// acceptanceAdminHTTPClient for the basic-auth driver construction.
//
// ProposalService IS registered in buildRouter as of loam-ofg.14
// (registerProposalService), so this step now reaches a real handler
// against the live pool: it resolves the branch, checks the reviewed /
// unconflicted / >= 1 current-round-approve preconditions, and delegates
// to the production *mirrorsync.StoreProposalAccepter. An earlier version
// of this comment said the call failed at the transport layer with "no
// such service registered"; that stopped being true when that bead landed.
//
// It does NOT go through that handler in this suite, though. The RPC's
// preconditions are exercised for real by "When I try to accept it"; its
// engine leg binds a real *forge.Forgejo the fake forge cannot answer, so
// the accept itself is run through the same production
// *mirrorsync.StoreProposalAccepter the handler delegates to. See
// acceptProposalForReal (acceptance_proposal_test.go) for the whole
// argument, including what that costs and which scenario stays @wip
// because of it.
//
// This step fails on a refused accept -- it is the SUCCESSFUL "When I
// accept it"; the refusals are "When I try to accept it", a different
// sentence with its own step.
func (h *acceptanceHarness) stepIAcceptIt(ctx context.Context) error {
	return h.acceptProposalForReal(ctx, worldFrom(ctx))
}

// assertDirExists asserts path exists and is a directory.
func assertDirExists(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("expected clone directory %s to exist: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("expected %s to be a directory", path)
	}
	return nil
}

// containsString reports whether values contains want verbatim.
func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
