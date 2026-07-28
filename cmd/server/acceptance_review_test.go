//go:build acceptance

// Step definitions for features/reviewing.feature and
// features/replies.feature (loam-ohc). Everything here drives the compiled
// loam CLI as a real subprocess, exactly as acceptance_steps_test.go's
// clone-and-push steps do -- the difference being that these scenarios need
// TWO (sometimes three) distinct agent identities acting against the same
// work branch, so every driver call here goes through runLoamAs with an
// explicit acceptanceActor rather than through runLoamCLI's single
// world-level identity.
//
// Three facts shape almost every step below:
//
//   - Staged comments never reach the server. They live in the ACTING
//     agent's own local .loam staging area, keyed by (repo, work-branch,
//     agent identifier) -- internal/cli/staging.go's OpenStaging. That is
//     why "no one else can see the comment" is asserted by reading `work
//     comments` as a genuinely different agent, and why "I can see it among
//     my staged comments" reads `work comments --staged` as the same one.
//
//   - Rounds live on Thread and Comment, not on WorkBranch. `work show` has
//     no round field to report (internal/cli/commands_work_read.go ->
//     workShowOutput), so every round assertion here reads `work comments`
//     or `work verdicts`.
//
//   - `work diff` fails with precondition_failed for essentially every work
//     branch, because nothing creates refs/heads/<name> in the mirror yet
//     (loam-5iu). No step here calls it: "I stage a comment on a line of the
//     diff" anchors to a file/line of the seeded upstream fixture directly.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"connectrpc.com/connect"
	"github.com/cucumber/godog"
	"github.com/google/uuid"
)

// acceptanceActor is one agent identity the CLI runs as: the three
// LOAM_AGENT_* variables, which the CLI joins into the "<name>-<id>-<role>"
// identifier the server records as a comment's author, a verdict's
// reviewer, and a staging area's key (internal/cli/config.go ->
// loadConfig; internal/httpauth.Identity.Identifier).
type acceptanceActor struct {
	name string
	id   string
	role string
}

// identifier renders this actor exactly as the CLI and the server both do.
func (a acceptanceActor) identifier() string { return a.name + "-" + a.id + "-" + a.role }

// parseAcceptanceActor splits a scenario's literal agent identifier (e.g.
// "ada-lovelace-7-reviewer") back into the three environment variables that
// produce it. The split is from the RIGHT -- role, then id, then whatever
// remains is the name -- because only the name may contain interior dashes
// ("ada-lovelace"), and LOAM_AGENT_NAME must itself be shaped
// "<first>-<last>" (internal/cli/config.go -> requireAgentName).
//
// Reconstructing the environment rather than inventing one matters: the
// feature files assert on these literal identifiers ("the reviewer
// 'alan-turing-4-reviewer' also submitted..."), and a `work verdicts` row
// carries the identifier the SERVER derived from the headers. If this split
// were wrong, every such assertion would compare a scenario literal against
// a differently-shaped identifier and the scenarios could only pass by
// never comparing them.
func parseAcceptanceActor(identifier string) (acceptanceActor, error) {
	parts := strings.Split(identifier, "-")
	if len(parts) < 4 {
		return acceptanceActor{}, fmt.Errorf("agent identifier %q must be shaped <first>-<last>-<id>-<role>", identifier)
	}
	role := parts[len(parts)-1]
	id := parts[len(parts)-2]
	name := strings.Join(parts[:len(parts)-2], "-")
	actor := acceptanceActor{name: name, id: id, role: role}
	if actor.identifier() != identifier {
		return acceptanceActor{}, fmt.Errorf("agent identifier %q did not round-trip through name/id/role (%q)", identifier, actor.identifier())
	}
	return actor, nil
}

// acceptanceOtherReviewerID is the second reviewer identity these scenarios
// need: named literally by reviewing.feature's "Listing verdicts shows each
// reviewer once", and reused (unnamed there) as "another reviewer" by the
// resolve-authorization and new-round scenarios, so the whole file has one
// answer to "who is the other reviewer".
const acceptanceOtherReviewerID = "alan-turing-4-reviewer"

// The bodies staged by the comment steps. They are distinct strings so an
// assertion that finds "the" published comment cannot be satisfied by the
// wrong one, and so an edit is observable as a change rather than as a
// no-op.
const (
	acceptanceFirstStagedBody  = "acceptance: the first staged note"
	acceptanceSecondStagedBody = "acceptance: the second staged note"
	acceptanceEditedStagedBody = "acceptance: the first staged note, revised"
	acceptanceRoundStagedBody  = "acceptance: a note staged for the current round"
	acceptanceReviewerThread   = "acceptance: a thread opened by me"
	acceptanceOtherThreadBody  = "acceptance: a thread opened by another reviewer"
	acceptanceReplyBody        = "acceptance: the author's reply"
)

// acceptanceStagedComment mirrors internal/cli/commands_work_comment.go's
// stagedCommentOutput -- `work comment`'s success shape and each row of
// `work comments --staged` -- reproduced here rather than imported, since
// that type is unexported.
type acceptanceStagedComment struct {
	Staged  bool   `json:"staged"`
	ID      string `json:"id"`
	File    string `json:"file"`
	Line    uint32 `json:"line"`
	Body    string `json:"body"`
	Resolve string `json:"resolve"`
}

// acceptanceComment and acceptanceThread mirror commands_work_read.go's
// commentOutput/threadOutput -- `work comments`' published-thread shape.
type acceptanceComment struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	Round  uint32 `json:"round"`
}

type acceptanceThread struct {
	ID       string              `json:"id"`
	Resolved bool                `json:"resolved"`
	File     string              `json:"file"`
	Line     uint32              `json:"line"`
	Round    uint32              `json:"round"`
	Comments []acceptanceComment `json:"comments"`
}

// acceptanceVerdict mirrors commands_work_read.go's workVerdictOutput --
// one row of `work verdicts`, one per reviewer.
type acceptanceVerdict struct {
	Reviewer string `json:"reviewer"`
	Outcome  string `json:"outcome"`
	Round    uint32 `json:"round"`
	Stale    bool   `json:"stale"`
}

// acceptanceWorkListRow/acceptanceWorkList mirror commands_work_read.go's
// workListRow/workListOutput -- `work list`'s envelope.
type acceptanceWorkListRow struct {
	Repo   string `json:"repo"`
	Name   string `json:"name"`
	Target string `json:"target"`
	Title  string `json:"title"`
	Author string `json:"author"`
	State  string `json:"state"`
}

type acceptanceWorkList struct {
	Truncated bool                    `json:"truncated"`
	Results   []acceptanceWorkListRow `json:"results"`
}

// acceptanceVerdictResult mirrors commands_work_verdict.go's verdictOutput.
type acceptanceVerdictResult struct {
	Repo       string `json:"repo"`
	WorkBranch string `json:"work_branch"`
	Outcome    string `json:"outcome"`
	Published  uint32 `json:"published"`
}

// acceptanceCLIError mirrors internal/cli/errors.go's errorPayload: the
// structured error document every failing `loam` invocation writes to the
// active output format. Steps assert on Error.Code, not merely on a
// non-zero exit -- "rejected as a failed precondition" and "rejected as not
// found" are different claims, and a command that failed for an unrelated
// reason (a broken fixture, a missing flag) must not satisfy either.
type acceptanceCLIError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// registerReviewSteps wires every step reviewing.feature and
// replies.feature need. `I am the author agent "..."` is NOT registered
// here: it already exists (registerCloneAndPushSteps ->
// stepIAmTheAuthorAgent), which now also records the parsed actor these
// steps drive the CLI as.
func (h *acceptanceHarness) registerReviewSteps(sc *godog.ScenarioContext) {
	sc.Step(`^a work branch "([^"]*)" is in state "([^"]*)"$`, h.stepAWorkBranchNamedIsInState)
	sc.Step(`^the work branch "([^"]*)" is in state "([^"]*)"$`, h.stepTheWorkBranchNamedIsInState)
	sc.Step(`^a work branch in state "([^"]*)"$`, h.stepAWorkBranchInState)
	sc.Step(`^I am the reviewer agent "([^"]*)"$`, h.stepIAmTheReviewerAgent)
	sc.Step(`^I list work branches awaiting my review$`, h.stepIListWorkBranchesAwaitingMyReview)
	sc.Step(`^"([^"]*)" is included$`, h.stepIsIncluded)
	sc.Step(`^I stage a comment on a line of the diff$`, h.stepIStageACommentOnALineOfTheDiff)
	sc.Step(`^no one else can see the comment$`, h.stepNoOneElseCanSeeTheComment)
	sc.Step(`^I can see it among my staged comments$`, h.stepICanSeeItAmongMyStagedComments)
	sc.Step(`^I have staged two comments$`, h.stepIHaveStagedTwoComments)
	sc.Step(`^I edit one staged comment and discard the other$`, h.stepIEditOneStagedCommentAndDiscardTheOther)
	sc.Step(`^my staged comments reflect the edit and the removal$`, h.stepMyStagedCommentsReflectTheEditAndRemoval)
	sc.Step(`^I submit a verdict with outcome "([^"]*)"$`, h.stepISubmitAVerdictWithOutcome)
	sc.Step(`^I submitted a verdict with outcome "([^"]*)"$`, h.stepISubmitAVerdictWithOutcome)
	sc.Step(`^I submit a verdict with outcome "([^"]*)" and no comments$`, h.stepISubmitAnOutcomeOnlyVerdict)
	sc.Step(`^both comments become visible on the work branch$`, h.stepBothCommentsBecomeVisible)
	sc.Step(`^my staged comments are cleared$`, h.stepMyStagedCommentsAreCleared)
	sc.Step(`^the verdict is recorded with outcome "([^"]*)"$`, h.stepTheVerdictIsRecordedWithOutcome)
	sc.Step(`^a thread I opened on the work branch$`, h.stepAThreadIOpened)
	sc.Step(`^a thread opened by another reviewer$`, h.stepAThreadOpenedByAnotherReviewer)
	sc.Step(`^I resolve the thread I opened$`, h.stepIResolveTheThreadIOpened)
	sc.Step(`^it is marked resolved$`, h.stepItIsMarkedResolved)
	sc.Step(`^I try to resolve the other reviewer's thread$`, h.stepITryToResolveTheOtherReviewersThread)
	sc.Step(`^the attempt is rejected$`, h.stepTheAttemptIsRejected)
	sc.Step(`^my recorded verdict for the round is "([^"]*)"$`, h.stepMyRecordedVerdictForTheRoundIs)
	sc.Step(`^the reviewer "([^"]*)" also submitted an "([^"]*)" verdict$`, h.stepTheReviewerAlsoSubmittedAVerdict)
	sc.Step(`^I list the verdicts$`, h.stepIListTheVerdicts)
	sc.Step(`^each reviewer appears once with their latest outcome$`, h.stepEachReviewerAppearsOnce)
	sc.Step(`^none are marked stale$`, h.stepNoneAreMarkedStale)
	sc.Step(`^I try to submit a verdict on it$`, h.stepITryToSubmitAVerdictOnIt)
	sc.Step(`^the attempt is rejected as a failed precondition$`, h.stepTheAttemptIsRejectedAsFailedPrecondition)
	sc.Step(`^another reviewer's verdict has marked the work branch "([^"]*)"$`, h.stepAnotherReviewersVerdictHasMarkedTheWorkBranch)
	sc.Step(`^the author requests review again$`, h.stepTheAuthorRequestsReviewAgain)
	sc.Step(`^my comments are published in the new round$`, h.stepMyCommentsArePublishedInTheNewRound)
	sc.Step(`^the work branch is on its second review round$`, h.stepTheWorkBranchIsOnItsSecondRound)
	sc.Step(`^I stage a comment and submit a verdict with outcome "([^"]*)"$`, h.stepIStageACommentAndSubmitAVerdict)
	sc.Step(`^the verdict is recorded against the second round$`, h.stepTheVerdictIsRecordedAgainstTheSecondRound)
	sc.Step(`^the published comment is recorded against the second round$`, h.stepThePublishedCommentIsRecordedAgainstTheSecondRound)
	sc.Step(`^it has a thread opened by the reviewer "([^"]*)"$`, h.stepItHasAThreadOpenedByTheReviewer)
	sc.Step(`^I reply to the thread$`, h.stepIReplyToTheThread)
	sc.Step(`^my reply is visible on the thread right away$`, h.stepMyReplyIsVisibleRightAway)
	sc.Step(`^it was not staged$`, h.stepItWasNotStaged)
	sc.Step(`^the work branch stays in state "([^"]*)"$`, h.stepTheWorkBranchStaysInState)
	sc.Step(`^the work branch has one "([^"]*)" verdict$`, h.stepTheWorkBranchHasOneVerdict)
	sc.Step(`^the verdicts are unchanged$`, h.stepTheVerdictsAreUnchanged)
	sc.Step(`^the thread was raised in the first round$`, h.stepTheThreadWasRaisedInTheFirstRound)
	sc.Step(`^my reply is recorded against the second round$`, h.stepMyReplyIsRecordedAgainstTheSecondRound)
	sc.Step(`^the thread still shows it was raised in the first round$`, h.stepTheThreadStillShowsTheFirstRound)
	sc.Step(`^I reply to a thread that does not exist$`, h.stepIReplyToAThreadThatDoesNotExist)
	sc.Step(`^the reply is rejected as a failed precondition$`, h.stepTheAttemptIsRejectedAsFailedPrecondition)
	sc.Step(`^the reply is rejected as not found$`, h.stepTheReplyIsRejectedAsNotFound)
}

// --- driver plumbing ---

// runLoamAs runs the compiled loam binary as actor, with stdin as its
// standard input, from the scenario's own workspace directory.
//
// It is deliberately separate from runLoamCLI (acceptance_git_test.go)
// rather than a parameterization of it: runLoamCLI's identity is the
// world's single agent, which is exactly right for clone-and-push (one
// author, one clone) and exactly wrong here, where the same work branch is
// acted on by an author, a reviewer, and a second reviewer within one
// scenario, and where the whole point of several scenarios is that those
// three see different things.
//
// The working directory is the workspace root, never a clone: every command
// these scenarios run is given its repo and work branch as explicit
// positional arguments, so no workspace inference is needed
// (internal/cli/workspace.go -> resolveWorkBranchIdentity: "an explicit
// positional argument always wins"), and the staging area lands under
// <workspace>/.loam/staging keyed by the acting agent's own identifier.
func (h *acceptanceHarness) runLoamAs(world *acceptanceWorld, actor acceptanceActor, stdin string, args ...string) loamCLIResult {
	cmd := exec.Command(h.loamBinary, args...)
	cmd.Dir = world.workspace
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"LOAM_SERVER_URL=" + h.server.baseURL,
		"LOAM_AGENT_NAME=" + actor.name,
		"LOAM_AGENT_ID=" + actor.id,
		"LOAM_AGENT_ROLE=" + actor.role,
	}
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
			stderr.WriteString("\n[harness] loam did not exit normally: " + runErr.Error())
		}
	}
	return loamCLIResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

// requireLoamOK fails the calling step unless the invocation exited 0,
// reporting the whole invocation verbatim. Every step that means to observe
// a SUCCESSFUL command routes through here rather than ignoring the exit
// code, so a scenario can never green on a command that silently failed.
func requireLoamOK(res loamCLIResult, what string) error {
	if res.exitCode != 0 {
		return fmt.Errorf("%s exited %d, want 0\nstdout: %s\nstderr: %s", what, res.exitCode, res.stdout, res.stderr)
	}
	return nil
}

// decodeLoamJSON decodes a successful invocation's JSON document.
func decodeLoamJSON[T any](res loamCLIResult, what string, out *T) error {
	if err := requireLoamOK(res, what); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(res.stdout), out); err != nil {
		return fmt.Errorf("decoding %s output: %w\nstdout: %s", what, err, res.stdout)
	}
	return nil
}

// requireLoamRejected asserts an invocation failed with the exact error
// CODE the scenario names (internal/cli/errormapper.go's code constants),
// and with that code's own exit class. Asserting the code, not just a
// non-zero exit, is what stops "the attempt is rejected" from being
// satisfied by a typo in the harness's own command line -- which would
// exit 2 as a usage error and otherwise look identical.
func requireLoamRejected(res loamCLIResult, what, wantCode string, wantExit int) error {
	if res.exitCode == 0 {
		return fmt.Errorf("%s exited 0, want a %s rejection\nstdout: %s", what, wantCode, res.stdout)
	}
	var payload acceptanceCLIError
	if err := json.Unmarshal([]byte(res.stdout), &payload); err != nil {
		return fmt.Errorf("decoding %s error document: %w\nstdout: %s\nstderr: %s", what, err, res.stdout, res.stderr)
	}
	if payload.Error.Code != wantCode {
		return fmt.Errorf("%s was rejected as %q (%s), want %q", what, payload.Error.Code, payload.Error.Message, wantCode)
	}
	if res.exitCode != wantExit {
		return fmt.Errorf("%s exited %d, want %d for a %s rejection (%s)", what, res.exitCode, wantExit, wantCode, payload.Error.Message)
	}
	return nil
}

// --- read helpers ---

// listThreads reads a work branch's PUBLISHED threads as actor.
func (h *acceptanceHarness) listThreads(world *acceptanceWorld, actor acceptanceActor, workBranch string) ([]acceptanceThread, error) {
	res := h.runLoamAs(world, actor, "", "work", "comments", world.repo(), workBranch)
	var threads []acceptanceThread
	err := decodeLoamJSON(res, fmt.Sprintf("loam work comments (as %s)", actor.identifier()), &threads)
	return threads, err
}

// listStaged reads actor's OWN local staging area for a work branch.
func (h *acceptanceHarness) listStaged(world *acceptanceWorld, actor acceptanceActor, workBranch string) ([]acceptanceStagedComment, error) {
	res := h.runLoamAs(world, actor, "", "work", "comments", world.repo(), workBranch, "--staged")
	var staged []acceptanceStagedComment
	err := decodeLoamJSON(res, fmt.Sprintf("loam work comments --staged (as %s)", actor.identifier()), &staged)
	return staged, err
}

// listVerdicts reads a work branch's verdicts as actor.
func (h *acceptanceHarness) listVerdicts(world *acceptanceWorld, actor acceptanceActor, workBranch string) ([]acceptanceVerdict, error) {
	res := h.runLoamAs(world, actor, "", "work", "verdicts", world.repo(), workBranch)
	var verdicts []acceptanceVerdict
	err := decodeLoamJSON(res, fmt.Sprintf("loam work verdicts (as %s)", actor.identifier()), &verdicts)
	return verdicts, err
}

// showWorkBranch reads a work branch's metadata as actor.
func (h *acceptanceHarness) showWorkBranch(world *acceptanceWorld, actor acceptanceActor, workBranch string) (acceptanceWorkBranchOutput, error) {
	res := h.runLoamAs(world, actor, "", "work", "show", world.repo(), workBranch)
	var out acceptanceWorkBranchOutput
	err := decodeLoamJSON(res, "loam work show", &out)
	return out, err
}

// stageComment stages one new-thread comment as actor and returns the
// staged item the CLI reports.
func (h *acceptanceHarness) stageComment(world *acceptanceWorld, actor acceptanceActor, workBranch, body string, line int) (acceptanceStagedComment, error) {
	args := []string{"work", "comment", world.repo(), workBranch}
	if line > 0 {
		args = append(args, "--file", acceptanceAuthFile, "--line", fmt.Sprintf("%d", line))
	}
	res := h.runLoamAs(world, actor, body, args...)
	var staged acceptanceStagedComment
	if err := decodeLoamJSON(res, fmt.Sprintf("loam work comment (as %s)", actor.identifier()), &staged); err != nil {
		return acceptanceStagedComment{}, err
	}
	if !staged.Staged || staged.ID == "" {
		return acceptanceStagedComment{}, fmt.Errorf("loam work comment reported staged=%v id=%q, want a staged item with an id", staged.Staged, staged.ID)
	}
	return staged, nil
}

// submitVerdict publishes actor's staged batch with outcome, recording the
// outcome as that reviewer's latest for the "each reviewer appears once
// with their latest outcome" assertion.
func (h *acceptanceHarness) submitVerdict(world *acceptanceWorld, actor acceptanceActor, workBranch, outcome string) (acceptanceVerdictResult, error) {
	res := h.runLoamAs(world, actor, "", "work", "verdict", world.repo(), workBranch, "--outcome", outcome)
	world.lastCLI = res
	var out acceptanceVerdictResult
	if err := decodeLoamJSON(res, fmt.Sprintf("loam work verdict --outcome %s (as %s)", outcome, actor.identifier()), &out); err != nil {
		return acceptanceVerdictResult{}, err
	}
	if out.Outcome != outcome {
		return acceptanceVerdictResult{}, fmt.Errorf("loam work verdict recorded outcome %q, want %q (the server's own echoed value)", out.Outcome, outcome)
	}
	world.latestOutcome[actor.identifier()] = outcome
	return out, nil
}

// requestReview drives `loam work request-review` as actor: draft ->
// reviewable opening round 1, or reviewed -> reviewable opening the next
// round (internal/handler/workbranch/workbranch.go -> RequestReview).
// It counts every call on the world, which is what makes "no request for
// review was needed" (features/work-branch-lifecycle.feature, via
// acceptance_proposal_test.go) a claim about what this harness actually
// did rather than only about what the resulting row says.
func (h *acceptanceHarness) requestReview(world *acceptanceWorld, actor acceptanceActor, workBranch string) error {
	world.requestReviews++
	res := h.runLoamAs(world, actor, "", "work", "request-review", world.repo(), workBranch)
	return requireLoamOK(res, fmt.Sprintf("loam work request-review (as %s)", actor.identifier()))
}

// setTitleAndDescription drives `loam work set --title <t>` with the
// description on stdin. RequestReview refuses a branch carrying neither
// (docs/cli-spec.md -> State gates), so this is a mandatory part of getting
// a seeded draft branch to reviewable, not decoration -- and it is done
// through the CLI rather than by widening insertWorkBranchRow's INSERT,
// because that refusal is a real server-side precondition worth crossing
// the same way an author would.
func (h *acceptanceHarness) setTitleAndDescription(world *acceptanceWorld, actor acceptanceActor, workBranch string) error {
	res := h.runLoamAs(world, actor, "Seeded by the acceptance suite so review can be requested.",
		"work", "set", world.repo(), workBranch, "--title", "Acceptance review fixture")
	return requireLoamOK(res, fmt.Sprintf("loam work set (as %s)", actor.identifier()))
}

// workBranchID reads a work branch's primary key, for the two fixture
// helpers below that must address it directly.
func (h *acceptanceHarness) workBranchID(ctx context.Context, world *acceptanceWorld, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := h.server.pool.QueryRow(ctx, `SELECT id FROM work_branches WHERE repo_id = $1 AND name = $2`, world.repoID, name).Scan(&id)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("reading work_branches id for %s/%s: %w", world.repo(), name, err)
	}
	return id, nil
}

// currentRoundNumber reads the work branch's highest review-round number, 0
// if it has none. Used to VERIFY a fixture reached the round a scenario
// says it is on -- reading the rounds table directly because no CLI command
// reports a work branch's current round (WorkBranch carries no round field;
// see this file's own header).
func (h *acceptanceHarness) currentRoundNumber(ctx context.Context, world *acceptanceWorld, name string) (int, error) {
	id, err := h.workBranchID(ctx, world, name)
	if err != nil {
		return 0, err
	}
	var number int
	err = h.server.pool.QueryRow(ctx, `SELECT COALESCE(MAX(number), 0) FROM review_rounds WHERE work_branch_id = $1`, id).Scan(&number)
	if err != nil {
		return 0, fmt.Errorf("reading current round number for %s/%s: %w", world.repo(), name, err)
	}
	return number, nil
}

// forceWorkBranchState writes a work branch's state directly.
//
// It is used for exactly one thing: reaching a state that is a scenario's
// PRECONDITION and whose own transition is under test elsewhere or out of
// scope entirely -- replies.feature's reviewed and complete Backgrounds.
// Reaching "reviewed" through a real verdict would plant a phantom verdict
// that replies.feature's "the work branch has one 'approve' verdict"
// scenario explicitly counts, and reaching "complete" needs the whole
// proposal-acceptance chain, which replies.feature is not about.
//
// It is deliberately NOT used for reviewing.feature's "The first verdict
// marks the work branch reviewed", whose entire subject is that transition:
// that scenario reaches reviewed by submitting a real verdict and asserts on
// the result. This is the same direct-SQL fixture technique
// acceptance_seed_test.go's insertWorkBranchRow already establishes for the
// initial state.
func (h *acceptanceHarness) forceWorkBranchState(ctx context.Context, world *acceptanceWorld, name, state string) error {
	tag, err := h.server.pool.Exec(ctx, `UPDATE work_branches SET state = $1, updated_at = now() WHERE repo_id = $2 AND name = $3`, state, world.repoID, name)
	if err != nil {
		return fmt.Errorf("forcing work branch %s/%s to state %s: %w", world.repo(), name, state, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("forcing work branch %s/%s to state %s updated %d rows, want 1", world.repo(), name, state, tag.RowsAffected())
	}
	return nil
}

// threadByAuthor finds the single published thread whose OPENING comment
// carries author and body. Matching on the opening comment is the same rule
// the CLI's own resolve pre-check uses for "the thread's author"
// (internal/cli/commands_work_comment.go -> requireThreadAuthor), and
// matching on the body too keeps two threads opened by the same reviewer
// distinguishable.
func threadByAuthor(threads []acceptanceThread, author, body string) (acceptanceThread, error) {
	for _, thread := range threads {
		if len(thread.Comments) == 0 {
			continue
		}
		if thread.Comments[0].Author == author && thread.Comments[0].Body == body {
			return thread, nil
		}
	}
	return acceptanceThread{}, fmt.Errorf("no published thread opened by %s with body %q (threads: %+v)", author, body, threads)
}

// threadByID finds a published thread by id.
func threadByID(threads []acceptanceThread, id string) (acceptanceThread, error) {
	for _, thread := range threads {
		if thread.ID == id {
			return thread, nil
		}
	}
	return acceptanceThread{}, fmt.Errorf("thread %s is no longer among the published threads (%+v)", id, threads)
}

// verdictByReviewer finds a reviewer's row in a `work verdicts` result,
// failing if the reviewer appears more than once -- ListVerdicts promises
// one row per reviewer (internal/handler/workbranch/review.go ->
// ListVerdicts), and a step that silently took the first of several would
// hide exactly the duplication that promise forbids.
func verdictByReviewer(verdicts []acceptanceVerdict, reviewer string) (acceptanceVerdict, error) {
	var found acceptanceVerdict
	seen := 0
	for _, v := range verdicts {
		if v.Reviewer == reviewer {
			found = v
			seen++
		}
	}
	if seen == 0 {
		return acceptanceVerdict{}, fmt.Errorf("no verdict recorded for reviewer %s (verdicts: %+v)", reviewer, verdicts)
	}
	if seen > 1 {
		return acceptanceVerdict{}, fmt.Errorf("reviewer %s appears %d times in `work verdicts`, want exactly one row (verdicts: %+v)", reviewer, seen, verdicts)
	}
	return found, nil
}

// --- fixture steps ---

// stepAWorkBranchNamedIsInState is reviewing.feature's and
// replies.feature's Background line "a work branch \"...\" is in state
// \"...\"". It seeds the row as draft (acceptance_seed_test.go's
// insertWorkBranchRow) and then reaches the requested state the way
// production does: `loam work request-review` as the author opens review
// round 1 and moves the branch to reviewable. Without that round nothing
// downstream works at all -- a verdict, a thread, and a reply all resolve
// the branch's CURRENT round, and a branch with none is a failed
// precondition by design.
//
// No mirror is built and no clone is made: every command these two feature
// files drive names its repo and work branch explicitly, so none of them
// touches git.
func (h *acceptanceHarness) stepAWorkBranchNamedIsInState(ctx context.Context, name, state string) error {
	world := worldFrom(ctx)
	world.setPrimaryWorkBranch(name)
	if err := h.insertWorkBranchRow(ctx, world.repoID, name, world.targetBranch, "draft", world.author.identifier()); err != nil {
		return err
	}
	if state == "draft" {
		return nil
	}
	if err := h.setTitleAndDescription(world, world.author, name); err != nil {
		return err
	}
	if err := h.requestReview(world, world.author, name); err != nil {
		return err
	}
	if state == "reviewable" {
		return nil
	}
	return h.forceWorkBranchState(ctx, world, name, state)
}

// stepTheWorkBranchNamedIsInState forces an ALREADY SEEDED work branch into
// state -- replies.feature's "Replying on a completed work branch is
// rejected". It names the branch, which is what distinguishes it in Gherkin
// from the bare assertion "the work branch is in state \"...\"" below:
// godog dispatches on the step's text alone and never sees the
// Given/When/Then keyword, so a fixture step and an assertion step cannot
// share one sentence. Conflating them would be the worse failure: a Then
// that quietly SET the state it was asked to check would pass no matter
// what the production code did.
func (h *acceptanceHarness) stepTheWorkBranchNamedIsInState(ctx context.Context, name, state string) error {
	return h.forceWorkBranchState(ctx, worldFrom(ctx), name, state)
}

// stepAWorkBranchInState seeds one work branch directly in state, with no
// review round at all, under whichever name acceptanceWorld.claimWorkBranch
// hands it (see that method for the first-come rule and why both feature
// files it serves get the branch they mean).
//
// In reviewing.feature's "A verdict cannot be submitted before review is
// requested" that is an ADDITIONAL, roundless branch, leaving the
// Background's own branch untouched so the rejection cannot be confused
// with anything about it. In admin-proposals.feature's "Closing a work
// branch ends it" it is the scenario's own branch, which is what "When I
// close it" then closes.
func (h *acceptanceHarness) stepAWorkBranchInState(ctx context.Context, state string) error {
	world := worldFrom(ctx)
	name := world.claimWorkBranch()
	return h.insertWorkBranchRow(ctx, world.repoID, name, world.targetBranch, state, world.author.identifier())
}

// stepIAmTheReviewerAgent records the literal reviewer identity a scenario
// names, split back into the LOAM_AGENT_* triple that produces it.
func (h *acceptanceHarness) stepIAmTheReviewerAgent(ctx context.Context, identifier string) error {
	world := worldFrom(ctx)
	reviewer, err := parseAcceptanceActor(identifier)
	if err != nil {
		return err
	}
	if reviewer.role != "reviewer" {
		return fmt.Errorf("agent %q has role %q, but this step names a REVIEWER (the reviewer role is what carries work.verdict)", identifier, reviewer.role)
	}
	world.reviewer = reviewer
	return nil
}

// --- reviewing.feature ---

// stepIListWorkBranchesAwaitingMyReview drives `loam work list
// --awaiting-review` as the reviewer. No --repo filter: "awaiting MY
// review" is a question about the caller, not about one repo, and the
// server answers it from the caller's own identity headers.
func (h *acceptanceHarness) stepIListWorkBranchesAwaitingMyReview(ctx context.Context) error {
	world := worldFrom(ctx)
	res := h.runLoamAs(world, world.reviewer, "", "work", "list", "--awaiting-review")
	return decodeLoamJSON(res, "loam work list --awaiting-review", &world.lastList)
}

// stepIsIncluded asserts the named work branch is among the listed results.
func (h *acceptanceHarness) stepIsIncluded(ctx context.Context, name string) error {
	world := worldFrom(ctx)
	for _, row := range world.lastList.Results {
		if row.Name == name && row.Repo == world.repo() {
			return nil
		}
	}
	return fmt.Errorf("work branch %s/%s is not among the listed results (%+v)", world.repo(), name, world.lastList.Results)
}

// stepIStageACommentOnALineOfTheDiff stages one line-anchored comment as
// the reviewer. The anchor names a real file and line of the seeded
// upstream fixture rather than one read out of `loam work diff`, which
// currently fails for every work branch (loam-5iu) -- the anchor is not
// what this scenario is about, and depending on diff would make it
// unrunnable for a reason unrelated to staging.
func (h *acceptanceHarness) stepIStageACommentOnALineOfTheDiff(ctx context.Context) error {
	world := worldFrom(ctx)
	_, err := h.stageComment(world, world.reviewer, world.workBranch, acceptanceFirstStagedBody, 8)
	return err
}

// stepNoOneElseCanSeeTheComment asserts the staged comment has not reached
// the server, by reading the work branch's published threads as a DIFFERENT
// agent (the author) and as a second reviewer. Reading as two other agents
// rather than one closes the "maybe the server just hides it from authors"
// reading, and the assertion is on the BODY, not merely on the thread
// count, so it stays honest if some unrelated thread ever exists.
func (h *acceptanceHarness) stepNoOneElseCanSeeTheComment(ctx context.Context) error {
	world := worldFrom(ctx)
	for _, other := range []acceptanceActor{world.author, world.otherReviewer} {
		threads, err := h.listThreads(world, other, world.workBranch)
		if err != nil {
			return err
		}
		for _, thread := range threads {
			for _, comment := range thread.Comments {
				if comment.Body == acceptanceFirstStagedBody {
					return fmt.Errorf("%s can see the staged comment %q on %s/%s, but it was never submitted", other.identifier(), comment.Body, world.repo(), world.workBranch)
				}
			}
		}
	}
	return nil
}

// stepICanSeeItAmongMyStagedComments asserts the staged comment IS visible
// to its own author through `work comments --staged`.
func (h *acceptanceHarness) stepICanSeeItAmongMyStagedComments(ctx context.Context) error {
	world := worldFrom(ctx)
	staged, err := h.listStaged(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	for _, item := range staged {
		if item.Body == acceptanceFirstStagedBody && item.Staged {
			return nil
		}
	}
	return fmt.Errorf("staged comment %q is not among %s's staged comments (%+v)", acceptanceFirstStagedBody, world.reviewer.identifier(), staged)
}

// stepIHaveStagedTwoComments stages two distinct comments as the reviewer.
func (h *acceptanceHarness) stepIHaveStagedTwoComments(ctx context.Context) error {
	world := worldFrom(ctx)
	first, err := h.stageComment(world, world.reviewer, world.workBranch, acceptanceFirstStagedBody, 8)
	if err != nil {
		return err
	}
	second, err := h.stageComment(world, world.reviewer, world.workBranch, acceptanceSecondStagedBody, 18)
	if err != nil {
		return err
	}
	if first.ID == second.ID {
		return fmt.Errorf("both staged comments were given the same id %q", first.ID)
	}
	world.staged = []acceptanceStagedComment{first, second}
	return nil
}

// stepIEditOneStagedCommentAndDiscardTheOther replaces the first staged
// comment's body and removes the second.
func (h *acceptanceHarness) stepIEditOneStagedCommentAndDiscardTheOther(ctx context.Context) error {
	world := worldFrom(ctx)
	if len(world.staged) != 2 {
		return fmt.Errorf("this step needs two staged comments, have %d", len(world.staged))
	}
	edit := h.runLoamAs(world, world.reviewer, acceptanceEditedStagedBody, "work", "comment", world.repo(), world.workBranch, "--edit", world.staged[0].ID)
	if err := requireLoamOK(edit, "loam work comment --edit"); err != nil {
		return err
	}
	discard := h.runLoamAs(world, world.reviewer, "", "work", "comment", world.repo(), world.workBranch, "--discard", world.staged[1].ID)
	var discarded acceptanceStagedComment
	if err := decodeLoamJSON(discard, "loam work comment --discard", &discarded); err != nil {
		return err
	}
	if discarded.Staged {
		return fmt.Errorf("loam work comment --discard reported the item as still staged (%+v)", discarded)
	}
	return nil
}

// stepMyStagedCommentsReflectTheEditAndRemoval asserts the staging area now
// holds exactly the edited item: the edit's new body under the SAME id, and
// no trace of the discarded one.
func (h *acceptanceHarness) stepMyStagedCommentsReflectTheEditAndRemoval(ctx context.Context) error {
	world := worldFrom(ctx)
	staged, err := h.listStaged(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if len(staged) != 1 {
		return fmt.Errorf("%d comments are staged after one edit and one discard, want 1 (%+v)", len(staged), staged)
	}
	if staged[0].ID != world.staged[0].ID {
		return fmt.Errorf("the surviving staged comment has id %q, want the edited one %q", staged[0].ID, world.staged[0].ID)
	}
	if staged[0].Body != acceptanceEditedStagedBody {
		return fmt.Errorf("the surviving staged comment reads %q, want the edited body %q", staged[0].Body, acceptanceEditedStagedBody)
	}
	return nil
}

// stepISubmitAVerdictWithOutcome publishes the reviewer's staged batch with
// outcome. It backs both "When I submit a verdict with outcome ..." and the
// past-tense Given "I submitted a verdict with outcome ..." -- the same
// single driver call either way.
func (h *acceptanceHarness) stepISubmitAVerdictWithOutcome(ctx context.Context, outcome string) error {
	world := worldFrom(ctx)
	_, err := h.submitVerdict(world, world.reviewer, world.workBranch, outcome)
	return err
}

// stepISubmitAnOutcomeOnlyVerdict is "I submit a verdict with outcome ...
// and no comments": it first proves the staging area really is empty (so
// "and no comments" is a fact about this invocation, not an assumption),
// then submits, then requires the server to report zero comments published.
func (h *acceptanceHarness) stepISubmitAnOutcomeOnlyVerdict(ctx context.Context, outcome string) error {
	world := worldFrom(ctx)
	staged, err := h.listStaged(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if len(staged) != 0 {
		return fmt.Errorf("this step submits an OUTCOME-ONLY verdict, but %d comments are staged (%+v)", len(staged), staged)
	}
	out, err := h.submitVerdict(world, world.reviewer, world.workBranch, outcome)
	if err != nil {
		return err
	}
	if out.Published != 0 {
		return fmt.Errorf("an outcome-only verdict published %d comments, want 0", out.Published)
	}
	return nil
}

// stepBothCommentsBecomeVisible asserts the two staged comments are now
// published on the work branch, each in its own thread, and reads them as a
// DIFFERENT agent -- "visible on the work branch" is a claim about what
// everyone else can see, and reading them back as the reviewer who staged
// them would be satisfied by a staging area that was never published at all.
func (h *acceptanceHarness) stepBothCommentsBecomeVisible(ctx context.Context) error {
	world := worldFrom(ctx)
	threads, err := h.listThreads(world, world.author, world.workBranch)
	if err != nil {
		return err
	}
	for _, body := range []string{acceptanceFirstStagedBody, acceptanceSecondStagedBody} {
		if _, err := threadByAuthor(threads, world.reviewer.identifier(), body); err != nil {
			return err
		}
	}
	return nil
}

// stepMyStagedCommentsAreCleared asserts the reviewer's staging area is
// empty after a successful publish.
func (h *acceptanceHarness) stepMyStagedCommentsAreCleared(ctx context.Context) error {
	world := worldFrom(ctx)
	staged, err := h.listStaged(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if len(staged) != 0 {
		return fmt.Errorf("%d comments are still staged after the verdict published (%+v)", len(staged), staged)
	}
	return nil
}

// stepTheWorkBranchStaysInState asserts the work branch's state through
// `loam work show` -- the CLI read, not a direct database peek.
//
// It is registered ONLY for "the work branch stays in state ...".
// "the work branch is in state ..." already has a step definition
// (acceptance_sync_test.go's stepTheWorkBranchIsInState, which reads
// work_branches.state directly), and godog dispatches on the sentence, so
// reviewing.feature's "Then the work branch is in state \"reviewed\"" is
// answered by that one; a second registration of the same sentence would be
// ambiguous rather than additive.
func (h *acceptanceHarness) stepTheWorkBranchStaysInState(ctx context.Context, state string) error {
	world := worldFrom(ctx)
	out, err := h.showWorkBranch(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if out.State != state {
		return fmt.Errorf("work branch %s/%s is in state %q, want %q", world.repo(), world.workBranch, out.State, state)
	}
	return nil
}

// stepTheVerdictIsRecordedWithOutcome asserts the caller's own verdict was
// recorded with outcome.
func (h *acceptanceHarness) stepTheVerdictIsRecordedWithOutcome(ctx context.Context, outcome string) error {
	world := worldFrom(ctx)
	verdicts, err := h.listVerdicts(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	mine, err := verdictByReviewer(verdicts, world.reviewer.identifier())
	if err != nil {
		return err
	}
	if mine.Outcome != outcome {
		return fmt.Errorf("recorded verdict for %s is %q, want %q", world.reviewer.identifier(), mine.Outcome, outcome)
	}
	return nil
}

// stepAThreadIOpened opens a thread as the reviewer the only way a reviewer
// can: stage a comment, then publish it with a verdict. It then reads the
// thread back to learn the server-assigned id the resolve steps need.
func (h *acceptanceHarness) stepAThreadIOpened(ctx context.Context) error {
	world := worldFrom(ctx)
	if _, err := h.stageComment(world, world.reviewer, world.workBranch, acceptanceReviewerThread, 8); err != nil {
		return err
	}
	if _, err := h.submitVerdict(world, world.reviewer, world.workBranch, "neutral"); err != nil {
		return err
	}
	threads, err := h.listThreads(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	thread, err := threadByAuthor(threads, world.reviewer.identifier(), acceptanceReviewerThread)
	if err != nil {
		return err
	}
	world.myThreadID = thread.ID
	return nil
}

// stepAThreadOpenedByAnotherReviewer opens a second thread as a genuinely
// different reviewer identity, through the same real publish path.
func (h *acceptanceHarness) stepAThreadOpenedByAnotherReviewer(ctx context.Context) error {
	world := worldFrom(ctx)
	if _, err := h.stageComment(world, world.otherReviewer, world.workBranch, acceptanceOtherThreadBody, 18); err != nil {
		return err
	}
	if _, err := h.submitVerdict(world, world.otherReviewer, world.workBranch, "neutral"); err != nil {
		return err
	}
	threads, err := h.listThreads(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	thread, err := threadByAuthor(threads, world.otherReviewer.identifier(), acceptanceOtherThreadBody)
	if err != nil {
		return err
	}
	if thread.ID == world.myThreadID {
		return fmt.Errorf("the other reviewer's thread and mine resolved to the same id %s", thread.ID)
	}
	world.otherThreadID = thread.ID
	return nil
}

// stepIResolveTheThreadIOpened stages a resolve of the caller's own thread
// and publishes it. A resolve is not a standalone command: it is staged by
// `work comment --resolve` and applied by the verdict that publishes the
// batch (internal/reviewpublish/publish.go -> Publish, step 4).
func (h *acceptanceHarness) stepIResolveTheThreadIOpened(ctx context.Context) error {
	world := worldFrom(ctx)
	res := h.runLoamAs(world, world.reviewer, "", "work", "comment", world.repo(), world.workBranch, "--resolve", world.myThreadID)
	if err := requireLoamOK(res, "loam work comment --resolve (my own thread)"); err != nil {
		return err
	}
	_, err := h.submitVerdict(world, world.reviewer, world.workBranch, "neutral")
	return err
}

// stepItIsMarkedResolved asserts the caller's own thread is now resolved --
// and that the other reviewer's is NOT, so a publish that resolved
// everything it could reach would fail here rather than read as success.
func (h *acceptanceHarness) stepItIsMarkedResolved(ctx context.Context) error {
	world := worldFrom(ctx)
	threads, err := h.listThreads(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	mine, err := threadByID(threads, world.myThreadID)
	if err != nil {
		return err
	}
	if !mine.Resolved {
		return fmt.Errorf("thread %s (opened by me) is not marked resolved", world.myThreadID)
	}
	other, err := threadByID(threads, world.otherThreadID)
	if err != nil {
		return err
	}
	if other.Resolved {
		return fmt.Errorf("thread %s (opened by the other reviewer) was resolved too; only the thread I resolved should be", world.otherThreadID)
	}
	return nil
}

// stepITryToResolveTheOtherReviewersThread attempts to stage a resolve of a
// thread the caller did not open. The attempt is refused before it is even
// staged (internal/cli/commands_work_comment.go -> requireThreadAuthor);
// the server refuses the same thing inside the publish transaction.
func (h *acceptanceHarness) stepITryToResolveTheOtherReviewersThread(ctx context.Context) error {
	world := worldFrom(ctx)
	world.lastCLI = h.runLoamAs(world, world.reviewer, "", "work", "comment", world.repo(), world.workBranch, "--resolve", world.otherThreadID)
	return nil
}

// stepTheAttemptIsRejected asserts the last attempt was refused as an
// AUTHORIZATION failure, not merely that it exited non-zero.
func (h *acceptanceHarness) stepTheAttemptIsRejected(ctx context.Context) error {
	world := worldFrom(ctx)
	return requireLoamRejected(world.lastCLI, "resolving another reviewer's thread", "unauthorized", 2)
}

// stepMyRecordedVerdictForTheRoundIs asserts the caller's verdict for the
// round is outcome -- and, via verdictByReviewer, that there is exactly ONE
// row for them, which is what "replaces" means.
func (h *acceptanceHarness) stepMyRecordedVerdictForTheRoundIs(ctx context.Context, outcome string) error {
	return h.stepTheVerdictIsRecordedWithOutcome(ctx, outcome)
}

// stepTheReviewerAlsoSubmittedAVerdict has a second, named reviewer submit
// an outcome-only verdict of their own.
func (h *acceptanceHarness) stepTheReviewerAlsoSubmittedAVerdict(ctx context.Context, identifier, outcome string) error {
	world := worldFrom(ctx)
	other, err := parseAcceptanceActor(identifier)
	if err != nil {
		return err
	}
	if other.identifier() == world.reviewer.identifier() {
		return fmt.Errorf("%q is the acting reviewer, not another one", identifier)
	}
	_, err = h.submitVerdict(world, other, world.workBranch, outcome)
	return err
}

// stepIListTheVerdicts drives `loam work verdicts` as the reviewer.
func (h *acceptanceHarness) stepIListTheVerdicts(ctx context.Context) error {
	world := worldFrom(ctx)
	verdicts, err := h.listVerdicts(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	world.lastVerdicts = verdicts
	return nil
}

// stepEachReviewerAppearsOnce asserts the listing holds exactly one row per
// reviewer who voted in this scenario, carrying that reviewer's LATEST
// outcome. It compares against world.latestOutcome -- the outcome every
// submitVerdict call recorded -- so a listing that dropped a reviewer, or
// reported a superseded outcome, fails here.
func (h *acceptanceHarness) stepEachReviewerAppearsOnce(ctx context.Context) error {
	world := worldFrom(ctx)
	seen := make(map[string]struct{}, len(world.lastVerdicts))
	for _, v := range world.lastVerdicts {
		if _, dup := seen[v.Reviewer]; dup {
			return fmt.Errorf("reviewer %s appears more than once in the listing (%+v)", v.Reviewer, world.lastVerdicts)
		}
		seen[v.Reviewer] = struct{}{}
		want, ok := world.latestOutcome[v.Reviewer]
		if !ok {
			return fmt.Errorf("the listing carries a verdict from %s, who never submitted one in this scenario (%+v)", v.Reviewer, world.lastVerdicts)
		}
		if v.Outcome != want {
			return fmt.Errorf("reviewer %s is listed with outcome %q, want their latest %q", v.Reviewer, v.Outcome, want)
		}
	}
	if len(seen) != len(world.latestOutcome) {
		return fmt.Errorf("the listing holds %d reviewers, want %d (%+v vs %+v)", len(seen), len(world.latestOutcome), world.lastVerdicts, world.latestOutcome)
	}
	return nil
}

// stepNoneAreMarkedStale asserts every listed verdict is current. Staleness
// is derived server-side from each verdict's round against the branch's
// current one (internal/handler/workbranch/review.go -> ListVerdicts); this
// step re-derives nothing.
func (h *acceptanceHarness) stepNoneAreMarkedStale(ctx context.Context) error {
	world := worldFrom(ctx)
	if len(world.lastVerdicts) == 0 {
		return fmt.Errorf("no verdicts were listed, so \"none are marked stale\" would hold vacuously")
	}
	for _, v := range world.lastVerdicts {
		if v.Stale {
			return fmt.Errorf("verdict from %s (round %d) is marked stale", v.Reviewer, v.Round)
		}
	}
	return nil
}

// stepITryToSubmitAVerdictOnIt attempts a verdict on the SECOND, roundless
// work branch the preceding Given seeded.
func (h *acceptanceHarness) stepITryToSubmitAVerdictOnIt(ctx context.Context) error {
	world := worldFrom(ctx)
	world.lastCLI = h.runLoamAs(world, world.reviewer, "", "work", "verdict", world.repo(), world.secondWorkBranch, "--outcome", "approve")
	return nil
}

// stepTheAttemptIsRejectedAsFailedPrecondition asserts the last attempt was
// refused as a state-gate violation. It backs reviewing.feature's "the
// attempt is rejected as a failed precondition", replies.feature's "the
// reply is rejected as a failed precondition", and admin-proposals.feature's
// two refused AcceptProposal scenarios.
//
// Those last two are not CLI invocations -- no `loam` command accepts a
// proposal; the admin's only surface is the ProposalService RPC -- so this
// step has two arms. Which arm runs is decided by whether a "When I try
// to ..." step actually made an admin RPC in this scenario
// (world.rpcAttempted), never by which of the two recorded outcomes looks
// more like a rejection: an unset acceptanceWorld carries a zero
// loamCLIResult (exit 0) and a nil lastRPCErr, so a scenario whose When
// never ran at all fails whichever arm it lands in rather than passing on
// the other's silence.
func (h *acceptanceHarness) stepTheAttemptIsRejectedAsFailedPrecondition(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.rpcAttempted {
		return requireRPCRejected(world.lastRPCErr, "the attempted admin RPC", connect.CodeFailedPrecondition)
	}
	return requireLoamRejected(world.lastCLI, "the attempted operation", "precondition_failed", 2)
}

// stepAnotherReviewersVerdictHasMarkedTheWorkBranch has the second reviewer
// cast a verdict and asserts it moved the branch into state -- the real
// reviewable -> reviewed flip, not a forced one.
func (h *acceptanceHarness) stepAnotherReviewersVerdictHasMarkedTheWorkBranch(ctx context.Context, state string) error {
	world := worldFrom(ctx)
	if _, err := h.submitVerdict(world, world.otherReviewer, world.workBranch, "approve"); err != nil {
		return err
	}
	return h.stepTheWorkBranchStaysInState(ctx, state)
}

// stepTheAuthorRequestsReviewAgain drives a re-review, opening the next
// round, and verifies the round number actually advanced.
func (h *acceptanceHarness) stepTheAuthorRequestsReviewAgain(ctx context.Context) error {
	world := worldFrom(ctx)
	before, err := h.currentRoundNumber(ctx, world, world.workBranch)
	if err != nil {
		return err
	}
	if err := h.requestReview(world, world.author, world.workBranch); err != nil {
		return err
	}
	after, err := h.currentRoundNumber(ctx, world, world.workBranch)
	if err != nil {
		return err
	}
	if after != before+1 {
		return fmt.Errorf("requesting review again left the work branch on round %d, want %d", after, before+1)
	}
	world.expectedRound = uint32(after)
	return nil
}

// stepMyCommentsArePublishedInTheNewRound asserts both comments staged
// BEFORE the round advanced were published against the NEW round -- the
// round the verdict landed in, not the one they were written during.
func (h *acceptanceHarness) stepMyCommentsArePublishedInTheNewRound(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.expectedRound < 2 {
		return fmt.Errorf("this step expects a new round, but the work branch is on round %d", world.expectedRound)
	}
	threads, err := h.listThreads(world, world.author, world.workBranch)
	if err != nil {
		return err
	}
	for _, body := range []string{acceptanceFirstStagedBody, acceptanceSecondStagedBody} {
		thread, err := threadByAuthor(threads, world.reviewer.identifier(), body)
		if err != nil {
			return err
		}
		if thread.Round != world.expectedRound {
			return fmt.Errorf("thread %q was published against round %d, want the new round %d", body, thread.Round, world.expectedRound)
		}
		if thread.Comments[0].Round != world.expectedRound {
			return fmt.Errorf("comment %q was published against round %d, want the new round %d", body, thread.Comments[0].Round, world.expectedRound)
		}
	}
	return nil
}

// stepTheWorkBranchIsOnItsSecondRound advances the work branch to review
// round 2 through the real transitions, from whichever state it is in: a
// reviewable branch needs a verdict first (only reviewed and draft may be
// sent back to reviewable), a reviewed one is already ready for the
// re-review. It then verifies the round number, reading review_rounds
// directly because no CLI command reports a branch's current round.
func (h *acceptanceHarness) stepTheWorkBranchIsOnItsSecondRound(ctx context.Context) error {
	world := worldFrom(ctx)
	out, err := h.showWorkBranch(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if out.State == "reviewable" {
		if _, err := h.submitVerdict(world, world.otherReviewer, world.workBranch, "approve"); err != nil {
			return err
		}
	}
	if err := h.stepTheAuthorRequestsReviewAgain(ctx); err != nil {
		return err
	}
	if world.expectedRound != 2 {
		return fmt.Errorf("the work branch is on round %d, want its second", world.expectedRound)
	}
	return nil
}

// stepIStageACommentAndSubmitAVerdict stages one comment and publishes it
// with outcome, in one step, as reviewing.feature's last scenario words it.
func (h *acceptanceHarness) stepIStageACommentAndSubmitAVerdict(ctx context.Context, outcome string) error {
	world := worldFrom(ctx)
	if _, err := h.stageComment(world, world.reviewer, world.workBranch, acceptanceRoundStagedBody, 8); err != nil {
		return err
	}
	out, err := h.submitVerdict(world, world.reviewer, world.workBranch, outcome)
	if err != nil {
		return err
	}
	if out.Published != 1 {
		return fmt.Errorf("the verdict published %d comments, want 1", out.Published)
	}
	return nil
}

// stepTheVerdictIsRecordedAgainstTheSecondRound asserts the caller's verdict
// carries round 2 -- and is not stale, since round 2 is the current one.
func (h *acceptanceHarness) stepTheVerdictIsRecordedAgainstTheSecondRound(ctx context.Context) error {
	world := worldFrom(ctx)
	verdicts, err := h.listVerdicts(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	mine, err := verdictByReviewer(verdicts, world.reviewer.identifier())
	if err != nil {
		return err
	}
	if mine.Round != 2 {
		return fmt.Errorf("my verdict is recorded against round %d, want 2", mine.Round)
	}
	if mine.Stale {
		return fmt.Errorf("my verdict for the CURRENT round is marked stale")
	}
	return nil
}

// stepThePublishedCommentIsRecordedAgainstTheSecondRound asserts the
// comment published alongside that verdict carries round 2 on both the
// thread and the comment.
func (h *acceptanceHarness) stepThePublishedCommentIsRecordedAgainstTheSecondRound(ctx context.Context) error {
	world := worldFrom(ctx)
	threads, err := h.listThreads(world, world.author, world.workBranch)
	if err != nil {
		return err
	}
	thread, err := threadByAuthor(threads, world.reviewer.identifier(), acceptanceRoundStagedBody)
	if err != nil {
		return err
	}
	if thread.Round != 2 {
		return fmt.Errorf("the published thread is recorded against round %d, want 2", thread.Round)
	}
	if thread.Comments[0].Round != 2 {
		return fmt.Errorf("the published comment is recorded against round %d, want 2", thread.Comments[0].Round)
	}
	return nil
}

// --- replies.feature ---

// stepItHasAThreadOpenedByTheReviewer opens replies.feature's Background
// thread as the named reviewer, through the real publish path (stage +
// verdict). The verdict it necessarily casts is that reviewer's only one,
// which is what lets "the work branch has one 'approve' verdict" mean
// exactly what it says: re-submitting replaces it in place rather than
// adding a second row.
func (h *acceptanceHarness) stepItHasAThreadOpenedByTheReviewer(ctx context.Context, identifier string) error {
	world := worldFrom(ctx)
	reviewer, err := parseAcceptanceActor(identifier)
	if err != nil {
		return err
	}
	world.reviewer = reviewer
	if _, err := h.stageComment(world, reviewer, world.workBranch, acceptanceReviewerThread, 8); err != nil {
		return err
	}
	if _, err := h.submitVerdict(world, reviewer, world.workBranch, "neutral"); err != nil {
		return err
	}
	threads, err := h.listThreads(world, reviewer, world.workBranch)
	if err != nil {
		return err
	}
	thread, err := threadByAuthor(threads, reviewer.identifier(), acceptanceReviewerThread)
	if err != nil {
		return err
	}
	world.myThreadID = thread.ID
	return nil
}

// stepIReplyToTheThread posts a reply as the AUTHOR. It deliberately does
// not assert on the exit code: the same sentence is the When of scenarios
// that expect success and of one that expects a rejection, so the exit code
// is the following Then's business.
func (h *acceptanceHarness) stepIReplyToTheThread(ctx context.Context) error {
	world := worldFrom(ctx)
	world.lastCLI = h.runLoamAs(world, world.author, acceptanceReplyBody, "work", "reply", world.repo(), world.workBranch, "--thread", world.myThreadID)
	return nil
}

// stepIReplyToAThreadThatDoesNotExist replies to a well-formed thread id
// that names no thread. The id is a real UUID, not a malformed string: a
// malformed one is an INVALID ARGUMENT (usage, exit 2), a different
// rejection from the not-found this scenario asks for.
func (h *acceptanceHarness) stepIReplyToAThreadThatDoesNotExist(ctx context.Context) error {
	world := worldFrom(ctx)
	missing := uuid.Must(uuid.NewV7()).String()
	world.lastCLI = h.runLoamAs(world, world.author, acceptanceReplyBody, "work", "reply", world.repo(), world.workBranch, "--thread", missing)
	return nil
}

// stepMyReplyIsVisibleRightAway asserts the reply landed on the thread and
// is visible to another agent immediately -- read as the REVIEWER, since
// "visible right away" is a claim about everyone, and reading it back as
// its own author would also be true of something merely staged.
func (h *acceptanceHarness) stepMyReplyIsVisibleRightAway(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := requireLoamOK(world.lastCLI, "loam work reply"); err != nil {
		return err
	}
	threads, err := h.listThreads(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	thread, err := threadByID(threads, world.myThreadID)
	if err != nil {
		return err
	}
	for _, comment := range thread.Comments {
		if comment.Author == world.author.identifier() && comment.Body == acceptanceReplyBody {
			return nil
		}
	}
	return fmt.Errorf("the author's reply is not on thread %s (%+v)", world.myThreadID, thread.Comments)
}

// stepItWasNotStaged asserts the reply never touched the author's local
// staging area: `work reply` is immediate, unlike `work comment`.
func (h *acceptanceHarness) stepItWasNotStaged(ctx context.Context) error {
	world := worldFrom(ctx)
	staged, err := h.listStaged(world, world.author, world.workBranch)
	if err != nil {
		return err
	}
	if len(staged) != 0 {
		return fmt.Errorf("the reply left %d item(s) in the author's staging area (%+v)", len(staged), staged)
	}
	return nil
}

// stepTheWorkBranchHasOneVerdict makes the reviewer's verdict the outcome
// the scenario names, and asserts the branch carries exactly one verdict
// afterwards -- the snapshot every "the verdicts are unchanged" assertion
// is then compared against.
func (h *acceptanceHarness) stepTheWorkBranchHasOneVerdict(ctx context.Context, outcome string) error {
	world := worldFrom(ctx)
	if _, err := h.submitVerdict(world, world.reviewer, world.workBranch, outcome); err != nil {
		return err
	}
	verdicts, err := h.listVerdicts(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if len(verdicts) != 1 || verdicts[0].Outcome != outcome {
		return fmt.Errorf("the work branch carries %+v, want exactly one %q verdict", verdicts, outcome)
	}
	world.verdictsBefore = verdicts
	return nil
}

// stepTheVerdictsAreUnchanged asserts replying moved nothing about the
// verdicts: same reviewers, same outcomes, same rounds, same stale flags.
func (h *acceptanceHarness) stepTheVerdictsAreUnchanged(ctx context.Context) error {
	world := worldFrom(ctx)
	verdicts, err := h.listVerdicts(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	if len(verdicts) != len(world.verdictsBefore) {
		return fmt.Errorf("the work branch carries %d verdicts after the reply, had %d before (%+v vs %+v)", len(verdicts), len(world.verdictsBefore), verdicts, world.verdictsBefore)
	}
	for i, before := range world.verdictsBefore {
		if verdicts[i] != before {
			return fmt.Errorf("verdict %d changed across the reply: %+v, was %+v", i, verdicts[i], before)
		}
	}
	world.lastVerdicts = verdicts
	return nil
}

// stepTheThreadWasRaisedInTheFirstRound asserts the Background thread
// carries round 1, so the later "still shows it was raised in the first
// round" is a claim about something that was true to begin with.
func (h *acceptanceHarness) stepTheThreadWasRaisedInTheFirstRound(ctx context.Context) error {
	return h.assertThreadRound(ctx, 1)
}

// stepTheThreadStillShowsTheFirstRound asserts the thread's own round is
// untouched by a reply made in a later round -- Thread.round is the round
// it was RAISED in and is never inherited by, or updated from, its comments.
func (h *acceptanceHarness) stepTheThreadStillShowsTheFirstRound(ctx context.Context) error {
	return h.assertThreadRound(ctx, 1)
}

// assertThreadRound asserts the Background thread's raised-in round.
func (h *acceptanceHarness) assertThreadRound(ctx context.Context, want uint32) error {
	world := worldFrom(ctx)
	threads, err := h.listThreads(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	thread, err := threadByID(threads, world.myThreadID)
	if err != nil {
		return err
	}
	if thread.Round != want {
		return fmt.Errorf("thread %s was raised in round %d, want %d", world.myThreadID, thread.Round, want)
	}
	return nil
}

// stepMyReplyIsRecordedAgainstTheSecondRound asserts the reply carries the
// round it was MADE in (2), not the round its thread was raised in (1).
func (h *acceptanceHarness) stepMyReplyIsRecordedAgainstTheSecondRound(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := requireLoamOK(world.lastCLI, "loam work reply"); err != nil {
		return err
	}
	threads, err := h.listThreads(world, world.reviewer, world.workBranch)
	if err != nil {
		return err
	}
	thread, err := threadByID(threads, world.myThreadID)
	if err != nil {
		return err
	}
	for _, comment := range thread.Comments {
		if comment.Author != world.author.identifier() || comment.Body != acceptanceReplyBody {
			continue
		}
		if comment.Round != 2 {
			return fmt.Errorf("the reply is recorded against round %d, want 2", comment.Round)
		}
		return nil
	}
	return fmt.Errorf("the author's reply is not on thread %s (%+v)", world.myThreadID, thread.Comments)
}

// stepTheReplyIsRejectedAsNotFound asserts the reply was refused because
// the thread does not exist -- exit 3, the one code with its own exit class.
func (h *acceptanceHarness) stepTheReplyIsRejectedAsNotFound(ctx context.Context) error {
	world := worldFrom(ctx)
	return requireLoamRejected(world.lastCLI, "loam work reply", "not_found", 3)
}
