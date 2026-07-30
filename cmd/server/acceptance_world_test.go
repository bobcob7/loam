//go:build acceptance

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/google/uuid"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/mirrorsync"
)

// acceptanceWorldKey is the context.Value key one scenario's *acceptanceWorld
// is stored under, godog's own documented pattern for per-scenario state
// (godog.ScenarioContext.Before returns a new ctx that every step function
// for that scenario receives).
type acceptanceWorldKey struct{}

// acceptanceScenarioCounter hands out a monotonically increasing suffix so
// every scenario gets its own uniquely named repo/work-branch, even though
// every scenario in this suite shares one Postgres database and one
// in-process server for the whole run (see TestFeatures' own doc comment
// for why). atomic, not a plain int, purely as a defensive habit: godog
// scenarios run sequentially by default in this suite (Options.Concurrency
// is left at its zero value), but nothing here depends on that remaining
// true.
var acceptanceScenarioCounter atomic.Int64

// acceptanceWorld is one scenario's own mutable fixture state: the repo/
// branch names this scenario seeded, the author identity it acts as, its
// own workspace tmpdir (the Author/Reviewer actor's per-actor workspace,
// testing-spec Layer 1's own requirement), and the outcome of whichever
// driver call a When step most recently made, for a later Then step to
// assert on.
type acceptanceWorld struct {
	workspace      string
	repoGroup      string
	repoName       string
	repoID         uuid.UUID
	targetBranch   string
	workBranch     string
	mirrorDir      string
	agentName      string
	agentID        string
	agentRole      string
	clonePath      string
	lastCLI        loamCLIResult
	lastGitOutput  string
	lastGitErr     error
	lastWorkBranch acceptanceWorkBranchOutput
	// upstreamSeeded/upstreamURL record this scenario's repo on the shared
	// fake forge: the clone URL every fetch, push, and ls-remote in the
	// scenario targets, and the flag afterScenario removes it on. Both are
	// set by seedUpstreamRepo (acceptance_sync_test.go), called from the
	// "the repo ... is enrolled ..." Background step.
	upstreamSeeded bool
	upstreamURL    string
	// divergedSHA and workBranchSHA are pre-tick mirror SHAs a later Then
	// step compares against, so an assertion cannot pass on an equality
	// that already held before the sync ran (loam-8vg).
	divergedSHA   string
	workBranchSHA string
	// workBranchesBefore snapshots name -> "<state>/<conflict>" for every
	// work branch of the repo before a sync that is expected to fail, for
	// "existing work branches are left untouched" to diff against.
	workBranchesBefore map[string]string
	// expectSyncError marks a scenario whose Given deliberately broke the
	// upstream, so "the next sync runs" tolerates the resulting
	// sync_state = 'error' instead of failing the step on it. It is set
	// only by a Given that caused the failure, never by a Then, so the
	// suppression can never be reached by a scenario that merely happened
	// to error.
	expectSyncError bool
	// The review-scenario state (acceptance_review_test.go). author,
	// reviewer, and otherReviewer are the three agent identities
	// reviewing.feature and replies.feature act as; every one of their
	// driver calls names one explicitly, rather than using the single
	// agentName/agentID/agentRole triple above, because the whole point of
	// several of those scenarios is that different agents see different
	// things.
	author        acceptanceActor
	reviewer      acceptanceActor
	otherReviewer acceptanceActor
	// staged is the staged items the comment steps created, in staging
	// order; secondWorkBranch is the additional, roundless branch the
	// "a work branch in state ..." Given seeds; myThreadID/otherThreadID are
	// the two published threads the resolve and reply steps address.
	staged           []acceptanceStagedComment
	secondWorkBranch string
	myThreadID       string
	otherThreadID    string
	// latestOutcome records, per reviewer identifier, the outcome that
	// reviewer most recently submitted in this scenario -- what "each
	// reviewer appears once with their latest outcome" is checked against.
	latestOutcome map[string]string
	// lastList/lastVerdicts/verdictsBefore/expectedRound carry one step's
	// observation forward to the Then that asserts on it.
	lastList       acceptanceWorkList
	lastVerdicts   []acceptanceVerdict
	verdictsBefore []acceptanceVerdict
	expectedRound  uint32
	// primarySeeded records whether some fixture step has already seeded
	// THIS scenario's main work branch (the one world.workBranch names and
	// every unqualified assertion step reads). It is what lets the
	// unnamed "a work branch ..." fixture steps mean the obvious thing in
	// both of the shapes the feature files use them in: the FIRST such
	// step in a scenario seeds the scenario's own branch, and any later
	// one seeds an additional branch under secondWorkBranch. See
	// claimWorkBranch.
	primarySeeded bool
	// The admin-proposals state (acceptance_proposal_test.go).
	// lastProposals is the ProposalService.ListProposals page the "I open
	// the proposal queue" step read; queued records that this scenario
	// observed the branch IN that queue at some point, so a later "it no
	// longer appears" cannot pass against a branch that was never there.
	lastProposals []*adminv1.Proposal
	queued        bool
	// lastRPCErr is the outcome of the most recent admin RPC a "When I
	// try to ..." step made, and rpcAttempted marks that such a step ran
	// at all -- the two together are what let the shared "the attempt is
	// rejected as a failed precondition" Then answer for an admin RPC as
	// well as for a CLI invocation, without either path being able to
	// satisfy the other's assertion by accident.
	lastRPCErr   error
	rpcAttempted bool
	// lastAcceptPRURL/lastAcceptUpstreamBranch are the most recent
	// AcceptProposal response; upstreamPRURL and firstUpstreamBranchSHA
	// are the FIRST accept's, kept separately so a re-accept can be
	// compared against what the original one produced rather than against
	// itself.
	lastAcceptPRURL          string
	lastAcceptUpstreamBranch string
	upstreamPRURL            string
	firstUpstreamBranchSHA   string
	// roundBefore is the branch's review-round number as it stood before
	// the re-review, for "a new review round is opened" to compare
	// against; closeReason is the body the close RPC sent, for "the reason
	// is recorded on the work branch" to read back.
	roundBefore int
	closeReason string
	// requestReviews counts the request-review calls this harness has made
	// in this scenario, and requestReviewsAtReset the count as it stood
	// when a conflicting advance demoted the branch. Their difference is
	// what "no request for review was needed" is asserted on: the claim is
	// about what nobody did, and a row-only check could not tell a
	// server-opened restore round from one this harness asked for and the
	// server then relabelled.
	requestReviews        int
	requestReviewsAtReset int
	// upstreamPRNumber is the PR number the forge allocated for this
	// scenario's proposal, read back out of
	// work_branches.upstream_pr_number AFTER the production accept engine
	// (mirrorsync.StoreProposalAccepter, loam-giq.7) wrote it -- not off
	// the forge response, and no longer seeded by a direct UPDATE. Reading
	// the column rather than the return value is deliberate: that column
	// is the entire poll set of mirrorsync.StorePRPoller, so an accept
	// that opened a PR without recording it must fail the fixture here
	// rather than surface three steps later as a poller that polled
	// nothing. stepTheUpstreamPRMerges still fails loudly against a zero
	// value (fakeforge has no PR #0) rather than passing vacuously.
	upstreamPRNumber int
	// The roles/instructions state (acceptance_roles_test.go, loam-ofg.11).
	// configuredRoleInstructions is the instructions text the "the ... role
	// has instructions configured" Given set on a role via the admin
	// RoleService, for "it receives the reviewer instructions" to compare
	// the CLI's response against; lastInstructions is that later When
	// step's decoded `loam instructions` response.
	configuredRoleInstructions string
	lastInstructions           acceptanceInstructionsOutput
	// The enrollment state (acceptance_enrollment_test.go, loam-ofg.12).
	// lastEnrolledRepo is the most recent EnrolledRepo this scenario
	// observed -- an EnrollRepo/SetTargetBranches response or a plain
	// GetRepo view -- for a following Then to assert on; lastProbeBranches/
	// lastProbeHead are ProbeRepo's own read-only response, kept separately
	// since a probe never touches lastEnrolledRepo (it enrolls nothing).
	lastEnrolledRepo  *adminv1.EnrolledRepo
	lastProbeBranches []string
	lastProbeHead     string
	// The reindex/Jobs-view/failed-ingest state (acceptance_ingest_test.go,
	// loam-7d0). lastReindexJob is ReindexRepo's own response job, for "a
	// full ingest job runs for it" to confirm the SAME target branch
	// against; lastIngestJobs is the page "I open the Jobs view" most
	// recently read. ingestedRefBeforeFailure and lastFailedJobID record
	// the pre-failure baseline and the specific job row a deliberately
	// broken ingest produced, so "the job is retried"/"is shown as failed"
	// assert on THIS job rather than merely the latest one for the repo.
	lastReindexJob           *adminv1.IngestJob
	lastIngestJobs           []*adminv1.IngestJob
	ingestedRefBeforeFailure string
	lastFailedJobID          uuid.UUID
	// accepterOverride is a scenario-scoped *mirrorsync.StoreProposalAccepter
	// acceptProposalForReal uses INSTEAD OF the harness's whole-suite
	// h.accepter when set -- built by "the server is configured without PR
	// attribution" (features/sync.feature, acceptance_sync_test.go) so that
	// one scenario can exercise the PRAttribution knob without touching the
	// process-wide LOAM_PR_ATTRIBUTION every other scenario in this suite
	// depends on (see newAcceptanceAccepterWithAttribution).
	accepterOverride *mirrorsync.StoreProposalAccepter
	// secondRepo is the SECOND enrolled-and-ingested repo loam-kywt's
	// "Searching across all repos" and "Graph fan-out does not link repos
	// in the MVP" scenarios need to prove real cross-repo behavior rather
	// than a single-repo scan that would pass vacuously (this bead's own
	// NOTES call out exactly this trap). Built by ensureSecondEnrolledRepo
	// (acceptance_ingest_test.go), lazily and at most once per scenario;
	// nil for every scenario that never needs one. It is a full,
	// independent *acceptanceWorld (its own repo/mirror/ingest state) so
	// the same seedUpstreamRepo/insertRepoRow/ingestIndexedBranch helpers
	// this file and acceptance_ingest_test.go already use for the PRIMARY
	// repo work unmodified against it too.
	secondRepo *acceptanceWorld
}

// repo returns this scenario's full "<group>/<repo_name>" identifier.
func (w *acceptanceWorld) repo() string { return w.repoGroup + "/" + w.repoName }

// setPrimaryWorkBranch records name as this scenario's own work branch --
// the one world.workBranch names and every unqualified step reads -- and
// marks the slot taken, so a later unnamed "a work branch ..." fixture
// step seeds an ADDITIONAL branch rather than silently renaming this one.
func (w *acceptanceWorld) setPrimaryWorkBranch(name string) {
	w.workBranch = name
	w.primarySeeded = true
}

// claimWorkBranch hands an unnamed "a work branch ..." fixture step the
// name it should seed under, first come first served: the scenario's own
// branch if nothing has claimed it yet, otherwise a distinct second one
// recorded as secondWorkBranch.
//
// Both feature shapes fall out of that one rule. reviewing.feature's
// Background names its branch explicitly ("a work branch \"wb-9c2f1a\" is
// in state \"reviewable\""), so its later "a work branch in state
// \"draft\"" is genuinely an ADDITIONAL, roundless branch and the
// scenario's own branch is left untouched -- which is exactly what that
// scenario needs. admin-proposals.feature never names a branch at all, so
// its first such Given IS the scenario's branch and "When I close it" has
// something to refer to; a second one (the disapproved branch in the queue
// scenario) becomes the second.
//
// Getting this wrong fails loudly rather than quietly: an assertion step
// reading world.workBranch would find no such row and report it, never
// pass on the wrong branch.
func (w *acceptanceWorld) claimWorkBranch() string {
	if !w.primarySeeded {
		w.primarySeeded = true
		return w.workBranch
	}
	w.secondWorkBranch = w.workBranch + "-second"
	return w.secondWorkBranch
}

// writeCommitAndPush writes filename with content into this scenario's
// clone, commits it with message, and pushes to refspec -- plain git, no
// loam involvement, exactly what the core vocabulary row "I commit and
// push" (docs/testing-spec.md Layer 1) resolves to. The commit sets an
// explicit committer identity (via the clone's own user.name/user.email,
// already configured by `loam clone`'s bootstrapCloneIdentity) rather than
// relying on any ambient gitconfig, per this repo's own constraint that
// CI carries no global one.
//
// A write/add/commit failure is a BROKEN FIXTURE, not a policy rejection
// under test, so it is returned directly (failing the calling step with a
// clear "git add: ..." message) rather than folded into lastGitOutput/
// lastGitErr -- those two fields carry only the PUSH's own outcome, which
// is what every Then step in this file (stepThePushIsRejected,
// stepThePushIsRejectedAsReadOnly, stepMyCommitsReachTheServerOn) actually
// means to assert on. Conflating the two would let a scenario green
// because its own setup broke, not because the server's policy genuinely
// rejected anything -- exactly the trap the four gitpushsuite-backed
// scenarios (loam-inq) would otherwise fall into, since all of them share
// this helper.
func (w *acceptanceWorld) writeCommitAndPush(filename, content, message, refspec string) error {
	if err := os.WriteFile(filepath.Join(w.clonePath, filename), []byte(content+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", filename, err)
	}
	if out, err := runPlainGit(w.clonePath, "add", filename); err != nil {
		return fmt.Errorf("git add %s: %w\n%s", filename, err, out)
	}
	if out, err := runPlainGit(w.clonePath, "commit", "--quiet", "-m", message); err != nil {
		return fmt.Errorf("git commit: %w\n%s", err, out)
	}
	out, err := runPlainGit(w.clonePath, "push", "origin", refspec)
	w.lastGitOutput, w.lastGitErr = out, err
	return nil
}

// newAcceptanceWorld builds a fresh acceptanceWorld for one scenario, with
// a uniquely suffixed repo/work-branch/agent identity so concurrent or
// merely sequential scenarios sharing this suite's one database never
// collide, and its own workspace tmpdir for the CLI actor driver to clone
// into.
func newAcceptanceWorld(t *testing.T) *acceptanceWorld {
	n := acceptanceScenarioCounter.Add(1)
	return &acceptanceWorld{
		workspace:     t.TempDir(),
		repoGroup:     "acceptance",
		repoName:      fmt.Sprintf("repo-%d", n),
		targetBranch:  "main",
		workBranch:    fmt.Sprintf("wb-%d", n),
		agentName:     fmt.Sprintf("acceptance-author-%d", n),
		agentID:       fmt.Sprintf("%d", n),
		agentRole:     "author",
		author:        acceptanceActor{name: "acceptance-author", id: fmt.Sprintf("%d", n), role: "author"},
		otherReviewer: mustAcceptanceActor(acceptanceOtherReviewerID),
		latestOutcome: map[string]string{},
	}
}

// mustAcceptanceActor parses a compile-time-constant agent identifier,
// panicking on a malformed one: the only inputs are this package's own
// constants, so a failure here is a typo in the harness, not a runtime
// condition any scenario could reach.
func mustAcceptanceActor(identifier string) acceptanceActor {
	actor, err := parseAcceptanceActor(identifier)
	if err != nil {
		panic("acceptance harness: " + err.Error())
	}
	return actor
}

// worldFrom retrieves the current scenario's *acceptanceWorld from ctx,
// panicking if none is set -- every step in this suite runs inside a
// scenario beforeScenario has already initialized, so a missing world
// means a step was invoked outside godog's own lifecycle, a programming
// error worth failing loudly on rather than a nil-pointer step further on.
func worldFrom(ctx context.Context) *acceptanceWorld {
	w, ok := ctx.Value(acceptanceWorldKey{}).(*acceptanceWorld)
	if !ok {
		panic("acceptance harness: no scenario world in context")
	}
	return w
}

// beforeScenario is godog's ScenarioContext.Before hook: it builds a fresh
// acceptanceWorld and stores it on ctx for every step in this scenario to
// retrieve via worldFrom. h.t.TempDir() (the whole suite's *testing.T) is
// what actually creates and schedules cleanup of the workspace directory;
// per-scenario subtests are not used here since godog itself, not `go
// test`, drives scenario iteration.
func (h *acceptanceHarness) beforeScenario(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
	world := newAcceptanceWorld(h.t)
	return context.WithValue(ctx, acceptanceWorldKey{}, world), nil
}

// afterScenario removes this scenario's fixtures that this suite's single
// shared server/database would otherwise keep for the whole run:
//
//   - the bare mirror on disk (lives under the shared server's own
//     LOAM_DATA_DIR, not under the scenario's own workspace tmpdir h.t's
//     TempDir cleanup already reaches).
//   - this scenario's repo on the shared fake forge. The fake is one
//     instance for the whole suite, and SeedRepoFiles refuses to seed
//     over an existing repo, so without this every scenario after the
//     first to name a given repo would either fail to seed or silently
//     inherit the previous scenario's branches and PR numbers -- the
//     forge-side twin of the repos-row collision described below.
//   - the repos row itself (cascading to repo_target_branches and
//     work_branches via their ON DELETE CASCADE foreign keys). This is
//     the fixture-isolation seam clone-and-push.feature's own Background
//     needs: every scenario in that file names the SAME literal repo
//     ("bobcob7/doc-server"), by design (they are exercising different
//     properties of what reads, in the Gherkin, as one conceptual repo),
//     so nothing in newAcceptanceWorld's own uniqueness suffix ever
//     applies to it -- world.repoGroup/repoName are overwritten by
//     stepRepoIsEnrolled with the scenario's own literal text. Without
//     this delete, the second scenario naming that repo would collide on
//     repos_name_key (observed directly while building this harness).
//
// The repos delete is preceded by an ingest DRAIN and is verified rather
// than fired and forgotten. Both halves are there for one observed failure
// mode. Almost every scenario's sync tick enqueues a job on the live,
// in-process ingest.Pool, which keeps writing -- to repos.sync_state, and
// to the chunk/edge tables whose foreign keys this delete has to cascade
// through -- on its own goroutine, after the scenario's last step has
// returned. A DELETE landing in the middle of that work can lose a
// deadlock, and because the outcome used to be discarded the row simply
// SURVIVED: every later scenario naming the same literal repo then failed
// its Background on repos_name_key, and the first genuine failure in a run
// turned into a cascade of eight unrelated ones. Draining first removes
// the contention at its source; verifying the delete means that if it ever
// fails anyway, THIS scenario says so instead of the next eight.
func (h *acceptanceHarness) afterScenario(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
	world := worldFrom(ctx)
	if world.mirrorDir != "" {
		_ = os.RemoveAll(world.mirrorDir)
	}
	teardownCtx, cancel := context.WithTimeout(context.Background(), acceptanceTeardownTimeout)
	defer cancel()
	if world.repoID != (uuid.UUID{}) {
		if err := h.ingestHarness.DrainIngestQueue(teardownCtx, mirrorsync.RepoID(world.repo())); err != nil {
			return ctx, fmt.Errorf("draining the ingest queue before removing %s: %w", world.repo(), err)
		}
		if _, err := h.server.pool.Exec(teardownCtx, `DELETE FROM repos WHERE id = $1`, world.repoID); err != nil {
			return ctx, fmt.Errorf("removing this scenario's repos row for %s: %w", world.repo(), err)
		}
	}
	if world.upstreamSeeded {
		_ = h.forge.RemoveRepo(teardownCtx, world.repo())
	}
	if world.secondRepo != nil {
		if err := h.teardownSecondRepo(teardownCtx, world.secondRepo); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

// teardownSecondRepo mirrors afterScenario's own primary-repo teardown
// (mirror removal, an ingest drain before the delete, the repos row
// delete, and the fake-forge repo removal) for world.secondRepo -- a
// second, fully independent repo some scenarios enroll alongside the
// primary one (see acceptanceWorld.secondRepo's own doc comment). Without
// this, a scenario that used ensureSecondEnrolledRepo would leak a repos
// row and a fake-forge repo past its own scenario boundary, and the next
// scenario naming acceptanceSecondRepoGroup/acceptanceSecondRepoName would
// collide on repos_name_key exactly as described in afterScenario's own
// doc comment for the primary repo.
func (h *acceptanceHarness) teardownSecondRepo(ctx context.Context, second *acceptanceWorld) error {
	if second.mirrorDir != "" {
		_ = os.RemoveAll(second.mirrorDir)
	}
	if second.repoID != (uuid.UUID{}) {
		if err := h.ingestHarness.DrainIngestQueue(ctx, mirrorsync.RepoID(second.repo())); err != nil {
			return fmt.Errorf("draining the ingest queue before removing the second repo %s: %w", second.repo(), err)
		}
		if _, err := h.server.pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, second.repoID); err != nil {
			return fmt.Errorf("removing the second repo's repos row for %s: %w", second.repo(), err)
		}
	}
	if second.upstreamSeeded {
		_ = h.forge.RemoveRepo(ctx, second.repo())
	}
	return nil
}

// acceptanceTeardownTimeout bounds afterScenario's ingest drain and repos
// delete. It is generous relative to the work involved (a drain of at most
// the one job this scenario's own ticks enqueued) and exists only so a
// wedged worker fails the run with a clear message rather than hanging it.
const acceptanceTeardownTimeout = 60 * time.Second

// agentIdentifier renders this world's agent triple the way
// internal/handler/workbranch's authorIdentifier() does at
// CreateWorkBranch time -- "<name>-<id>-<role>" -- which is what
// work_branches.author actually holds and what internal/refpolicy compares
// a pushing agent against.
//
// The seeders used to pass world.agentName, the BARE name. That agreed
// with the bare-name comparison refpolicy made before loam-ppb was fixed,
// so this suite was self-consistent and disagreed with production, where
// an author could never push to the work branch they had just started.
func (w *acceptanceWorld) agentIdentifier() string {
	return w.agentName + "-" + w.agentID + "-" + w.agentRole
}
