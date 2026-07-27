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
	return world.writeCommitAndPush("no-identity-change.txt", "no identity", "acceptance: no identity", "HEAD:refs/heads/"+world.workBranch)
}

// stepThePushIsRejected is the generic push-rejection assertion several
// clone-and-push.feature scenarios share (the more specific rejection-
// reason scenarios use their own dedicated Then step, e.g.
// stepThePushIsRejectedAsReadOnly). A non-zero exit is necessary but not
// sufficient: it also requires the output carry a `remote: loam:`-prefixed
// reason (docs/git-spec.md's "Ref Policy (push)" rejection-reasons table
// -- every documented reason is `loam:`-prefixed and, per git's own
// smart-HTTP client behavior, arrives on the pushing side as "remote:
// loam: ..."), so a push that fails for some unrelated reason (a broken
// fixture, a network error, git rejecting a non-fast-forward on its own
// before ever reaching Loam's pre-receive hook) cannot be mistaken for a
// genuine policy rejection.
func (h *acceptanceHarness) stepThePushIsRejected(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastGitErr == nil {
		return fmt.Errorf("push succeeded, want rejection\n%s", world.lastGitOutput)
	}
	if !strings.Contains(world.lastGitOutput, "remote: loam:") {
		return fmt.Errorf("push failed, but not with a loam policy rejection:\n%s", world.lastGitOutput)
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

// acceptanceWorkBranchOutput mirrors internal/cli/commands_work.go's own
// workBranchOutput JSON shape (repo, name, target, title, state) -- `loam
// work request-review`'s success output -- reproduced here rather than
// imported, since internal/cli's type is unexported.
type acceptanceWorkBranchOutput struct {
	Repo   string `json:"repo"`
	Name   string `json:"name"`
	Target string `json:"target"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

// stepIRequestReview is the core vocabulary row "I request review": one
// `loam work request-review <repo> <work-branch>` invocation, asserting
// exit 0 and decoding its JSON success output, per
// docs/testing-spec.md's own resolution for this row ("parse JSON,
// assert exit code"). A non-zero exit is reported verbatim with stdout
// and stderr rather than silently accepted -- a silent return here would
// let any future scenario using this step pass regardless of what the
// CLI actually did, exactly the failure mode this bead exists to
// prevent.
func (h *acceptanceHarness) stepIRequestReview(ctx context.Context) error {
	world := worldFrom(ctx)
	world.lastCLI = h.runLoamCLI(world, "work", "request-review", world.repo(), world.workBranch)
	if world.lastCLI.exitCode != 0 {
		return fmt.Errorf("loam work request-review exited %d, want 0\nstdout: %s\nstderr: %s", world.lastCLI.exitCode, world.lastCLI.stdout, world.lastCLI.stderr)
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
// Reading sync_state once, straight after Tick, is safe from loam-8vg's
// single-sample trap: Tick blocks until every cycle it started has
// finished reporting (mirrorsync.Scheduler.Tick -> waitIdle, which
// releases only after the terminal report), and no other writer of this
// column exists in the tree today -- the ingest worker's own
// sync_state write (loam-c94.13) is not implemented, so nothing can move
// the value between the report and this read.
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
// repo and fails loudly if the tick just run left it in the 'error' state.
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
// acceptanceAdminHTTPClient for the basic-auth driver construction;
// ProposalService itself has no handler registered in buildRouter yet
// (no un-@wip scenario needs it today), so this call currently fails at
// the transport layer with "no such service registered" until that
// handler lands -- still the correct, resolvable driver call for the step.
func (h *acceptanceHarness) stepIAcceptIt(ctx context.Context) error {
	world := worldFrom(ctx)
	client := h.newProposalServiceClient()
	_, err := acceptProposal(ctx, client, world.repo(), world.workBranch)
	return err
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
