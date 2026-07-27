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
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cucumber/godog"

	"github.com/bobcob7/loam/internal/mirrorsync"
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
}

// stepRepoIsEnrolled seeds world's repo row plus its registered target
// branch (see acceptance_seed_test.go's insertRepoRow). The mirror itself
// is not built until stepIHaveStartedTheWorkBranch, once both the target
// branch and the work branch it also seeds are known, so it is created
// once with its full, final set of branches.
func (h *acceptanceHarness) stepRepoIsEnrolled(ctx context.Context, repo, branch string) error {
	world := worldFrom(ctx)
	group, name, ok := strings.Cut(repo, "/")
	if !ok {
		return fmt.Errorf("repo %q must be shaped like <group>/<repo_name>", repo)
	}
	world.repoGroup, world.repoName, world.targetBranch = group, name, branch
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
func (h *acceptanceHarness) stepIAmTheAuthorAgent(ctx context.Context, agentName string) error {
	world := worldFrom(ctx)
	world.agentName = agentName
	return nil
}

// stepIHaveStartedTheWorkBranch seeds world's work_branches row, builds the
// bare mirror on disk with both the target and work branches present, and
// reconciles it (installs the real pre-receive hook) -- see
// acceptance_seed_test.go's seedBareMirrorWithBranches and
// acceptanceHarness.reconcileSeededMirror.
func (h *acceptanceHarness) stepIHaveStartedTheWorkBranch(ctx context.Context, workBranch string) error {
	world := worldFrom(ctx)
	world.workBranch = workBranch
	if err := h.insertWorkBranchRow(ctx, world.repoID, workBranch, world.targetBranch, "draft", world.agentName); err != nil {
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
	return world.writeCommitAndPush("agent-change.txt", "acceptance change", "acceptance: agent change", "HEAD")
}

// stepMyCommitsReachTheServerOn asserts the push landed: the mirror's
// branch ref now points at the clone's own HEAD.
func (h *acceptanceHarness) stepMyCommitsReachTheServerOn(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	if world.lastGitErr != nil {
		return fmt.Errorf("push failed: %v\n%s", world.lastGitErr, world.lastGitOutput)
	}
	mirrorSHA, err := mirrorRefSHA(world.mirrorDir, "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("reading mirror ref refs/heads/%s: %w", branch, err)
	}
	cloneSHA, err := cloneHeadSHA(world.clonePath)
	if err != nil {
		return fmt.Errorf("reading clone HEAD: %w", err)
	}
	if mirrorSHA != cloneSHA {
		return fmt.Errorf("mirror's refs/heads/%s (%s) does not match the pushed commit (%s)", branch, mirrorSHA, cloneSHA)
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
// documented read-only reason (docs/git-spec.md's Ref Policy table).
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
	return world.writeCommitAndPush("no-identity-change.txt", "no identity", "acceptance: no identity", "HEAD:refs/heads/"+world.workBranch)
}

// stepThePushIsRejected is the generic push-rejection assertion several
// clone-and-push.feature scenarios share: any non-zero exit is
// sufficient (the more specific rejection-reason scenarios use their own
// dedicated Then step, e.g. stepThePushIsRejectedAsReadOnly).
func (h *acceptanceHarness) stepThePushIsRejected(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastGitErr == nil {
		return fmt.Errorf("push succeeded, want rejection\n%s", world.lastGitOutput)
	}
	return nil
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

// stepIRequestReview is the core vocabulary row "I request review": one
// `loam work request-review` invocation, parsed as JSON success or a
// non-zero exit -- resolvable today (the CLI subprocess driver, `loam
// work request-review <repo> <work-branch>`), even though the command
// itself still returns errNotImplemented (internal/cli/commands_work.go)
// pending its own implementation bead. No scenario in this suite's
// default (@wip-filtered) run calls this step yet.
func (h *acceptanceHarness) stepIRequestReview(ctx context.Context) error {
	world := worldFrom(ctx)
	world.lastCLI = h.runLoamCLI(world, "work", "request-review", world.repo(), world.workBranch)
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
func (h *acceptanceHarness) stepTheNextSyncRuns(ctx context.Context) error {
	_, err := h.syncHarness.Tick(ctx)
	return err
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
// acceptanceAdminHTTPClient for the basic-auth driver construction;
// ProposalService itself has no handler registered in buildRouter yet
// (no un-@wip scenario needs it today), so this call currently fails at
// the transport layer with "no such service registered" until that
// handler lands -- still the correct, resolvable driver call for the step.
func (h *acceptanceHarness) stepIAcceptIt(ctx context.Context) error {
	world := worldFrom(ctx)
	client := h.newProposalServiceClient()
	resp, err := acceptProposal(ctx, client, world.repo(), world.workBranch)
	if err != nil {
		return err
	}
	world.lastProposalPRURL = resp
	return nil
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
