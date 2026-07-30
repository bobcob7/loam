//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/cucumber/godog"
	"github.com/google/uuid"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"

	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/refnames"
)

// acceptanceRetryPollTimeout/acceptanceRetryPollInterval bound "the job is
// retried"'s poll of ingest_jobs.attempts: the production Pool's default
// backoff (1s base, doubling) means a second attempt lands within a couple
// of seconds of the first failure, so 20s leaves generous CI slack without
// letting a genuinely stuck job hang the suite.
const (
	acceptanceRetryPollTimeout  = 20 * time.Second
	acceptanceRetryPollInterval = 200 * time.Millisecond
)

// registerIngestAndQuerySteps wires the steps ingestion.feature and
// code-intelligence.feature need to observe an INDEX rather than a ref.
//
// Everything here became reachable only once the suite stopped pointing
// LOAM_EMBEDDER_URL at a local Ollama that is not running (see
// startAcceptanceEmbedder): before that, every ingest job in this suite
// failed at the embed step, so no scenario could assert on an index
// because no index was ever built. loam-7d0 recorded these scenarios as
// unreachable for a different reason -- that the pipeline was incomplete
// -- and that reason no longer holds: the pipeline is complete and the
// embedder was the last missing piece.
func (h *acceptanceHarness) registerIngestAndQuerySteps(sc *godog.ScenarioContext) {
	// "I am signed in to the web interface as the admin" is deliberately
	// NOT registered here: registerSyncSteps already owns it, and godog
	// rejects a duplicate pattern.
	sc.Step(`^enrollment completes$`, h.stepEnrollmentCompletes)
	sc.Step(`^the indexed branch "([^"]*)" is ingested$`, h.stepIndexedBranchIsIngested)
	sc.Step(`^graph and search queries return results for it$`, h.stepGraphAndSearchReturnResults)
	sc.Step(`^"([^"]*)" has been ingested$`, h.stepBranchHasBeenIngested)
	sc.Step(`^its indexed branch has been ingested$`, h.stepIndexedBranchHasBeenIngested)
	sc.Step(`^"([^"]*)" advances with a commit that adds "([^"]*)" and removes "([^"]*)"$`, h.stepBranchAdvancesAddingAndRemoving)
	sc.Step(`^after ingestion a graph query finds "([^"]*)"$`, h.stepAfterIngestionAGraphQueryFinds)
	sc.Step(`^a graph query no longer finds "([^"]*)"$`, h.stepAGraphQueryNoLongerFinds)
	sc.Step(`^I am working inside a clone of "([^"]*)"$`, h.stepIAmWorkingInsideACloneOf)
	sc.Step(`^I ask the graph where "([^"]*)" is defined$`, h.stepIAskTheGraphWhereIsDefined)
	sc.Step(`^I get the file and line of its definition$`, h.stepIGetTheFileAndLineOfItsDefinition)
	sc.Step(`^I search for "([^"]*)"$`, h.stepISearchFor)
	sc.Step(`^I get the most relevant doc and code chunks$`, h.stepIGetTheMostRelevantChunks)
	sc.Step(`^each result names its repo, file, and line range$`, h.stepEachResultNamesRepoFileAndLines)
	sc.Step(`^"([^"]*)" has advanced past the last ingested commit$`, h.stepBranchHasAdvancedPastTheLastIngestedCommit)
	sc.Step(`^I run a graph query$`, h.stepIRunAGraphQuery)
	sc.Step(`^the response names the commit the index was built from$`, h.stepResponseNamesTheCommitTheIndexWasBuiltFrom)
	sc.Step(`^I can tell the results predate the tip of "([^"]*)"$`, h.stepResultsPredateTheTipOf)

	// loam-7d0: three of the four scenarios formerly tagged @wip in
	// features/ingestion.feature. "Edges reflect the current code even in
	// unchanged files" stays @wip -- see this file's own note near the
	// bottom on why that one is not implementable as written.
	sc.Step(`^I reindex "([^"]*)"$`, h.stepIReindex)
	sc.Step(`^a full ingest job runs for it$`, h.stepAFullIngestJobRunsForIt)
	sc.Step(`^once it succeeds, queries reflect the current indexed branch$`, h.stepOnceItSucceedsQueriesReflectTheCurrentIndexedBranch)

	sc.Step(`^ingest jobs have run for enrolled repos$`, h.stepIngestJobsHaveRunForEnrolledRepos)
	sc.Step(`^I open the Jobs view$`, h.stepIOpenTheJobsView)
	sc.Step(`^I see each job's repo, status, and timing$`, h.stepISeeEachJobsRepoStatusAndTiming)

	sc.Step(`^"([^"]*)" has been ingested successfully$`, h.stepBranchHasBeenIngestedSuccessfully)
	sc.Step(`^the next ingestion fails$`, h.stepTheNextIngestionFails)
	sc.Step(`^the job is shown as failed with its error$`, h.stepTheJobIsShownAsFailedWithItsError)
	sc.Step(`^graph and search queries still return the previous index$`, h.stepGraphAndSearchQueriesStillReturnThePreviousIndex)
	sc.Step(`^the reported ingested commit is unchanged$`, h.stepTheReportedIngestedCommitIsUnchanged)
	sc.Step(`^the job is retried$`, h.stepTheJobIsRetried)
}

// acceptanceEnvelope is the {ingested, truncated, results} envelope every
// `loam graph <subquery>` and `loam search` emits (internal/cli's
// graphQueryOutput and searchOutput). Row shapes differ per subquery, so
// results are decoded generically and only the fields the assertions
// below name are addressed.
type acceptanceEnvelope struct {
	Ingested []struct {
		Repo   string `json:"repo"`
		Target string `json:"target"`
		Ref    string `json:"ref"`
		At     string `json:"at"`
	} `json:"ingested"`
	Results []map[string]any `json:"results"`
}

// ingestIndexedBranch is the shared "this repo's indexed branch is now in
// the index" helper behind every Given/When below that needs an index.
//
// It clones the mirror from the real upstream over the production
// transport if the scenario has none yet, then enqueues a FULL ingest on
// the real, in-process ingest.Pool run() built and blocks until the queue
// for this repo is empty. That is exactly the sequence
// RepoAdminService.EnrollRepo performs as its own last two steps (clone,
// then Enqueue(KindFull)) -- reproduced rather than driven through the
// RPC because the Background step has already inserted the repos row, so
// EnrollRepo would answer AlreadyExists and never reach either step.
func (h *acceptanceHarness) ingestIndexedBranch(ctx context.Context, world *acceptanceWorld) error {
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	if err := h.server.ingestPool.Enqueue(ctx, world.repoID, world.targetBranch, ingest.KindFull); err != nil {
		return fmt.Errorf("enqueuing the initial ingest for %s: %w", world.repo(), err)
	}
	if err := h.ingestHarness.DrainIngestQueue(ctx, mirrorsync.RepoID(world.repo())); err != nil {
		return fmt.Errorf("draining the ingest queue for %s: %w", world.repo(), err)
	}
	return h.assertIngestSucceeded(ctx, world)
}

// assertIngestSucceeded fails loudly if the drain completed with the job
// in a failed state. DrainIngestQueue returns once nothing is queued or
// running, which a FAILED job also satisfies during its retry backoff, so
// without this check a broken ingest would read as a completed one and
// every downstream assertion would fail somewhere less informative.
func (h *acceptanceHarness) assertIngestSucceeded(ctx context.Context, world *acceptanceWorld) error {
	var status, jobError string
	err := h.server.pool.QueryRow(ctx,
		`SELECT status, COALESCE(error, '') FROM ingest_jobs WHERE repo_id = $1 ORDER BY queued_at DESC LIMIT 1`,
		world.repoID).Scan(&status, &jobError)
	if err != nil {
		return fmt.Errorf("reading the latest ingest job for %s: %w", world.repo(), err)
	}
	if status != "succeeded" {
		return fmt.Errorf("the ingest job for %s finished as %q: %s", world.repo(), status, jobError)
	}
	return nil
}

// ingestedRef reads back the commit the index was actually built from --
// repo_target_branches.ingested_ref, the same column handler.ScopeResolver
// populates the `ingested` envelope from.
func (h *acceptanceHarness) ingestedRef(ctx context.Context, world *acceptanceWorld) (string, error) {
	var ref *string
	err := h.server.pool.QueryRow(ctx,
		`SELECT ingested_ref FROM repo_target_branches WHERE repo_id = $1 AND branch = $2`,
		world.repoID, world.targetBranch).Scan(&ref)
	if err != nil {
		return "", fmt.Errorf("reading ingested_ref for %s: %w", world.repo(), err)
	}
	if ref == nil {
		return "", nil
	}
	return *ref, nil
}

// queryDir is where a query subprocess runs: inside the scenario's clone
// when it has one (so the CLI infers its scope from the directory), and
// the workspace root otherwise.
func (w *acceptanceWorld) queryDir() string {
	if w.clonePath != "" {
		return w.clonePath
	}
	return w.workspace
}

// runQuery runs one `loam` query subprocess from queryDir, records it as
// the scenario's last CLI result, and decodes its envelope. A non-zero
// exit is returned as an error carrying both streams: a query step that
// swallowed a failed exit would let every following Then assert against
// an empty envelope and pass for the wrong reason.
func (h *acceptanceHarness) runQuery(world *acceptanceWorld, args ...string) (acceptanceEnvelope, error) {
	world.lastCLI = h.runLoamCLIIn(world, world.queryDir(), args...)
	var envelope acceptanceEnvelope
	if world.lastCLI.exitCode != 0 {
		return envelope, fmt.Errorf("loam %s exited %d, want 0\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), world.lastCLI.exitCode, world.lastCLI.stdout, world.lastCLI.stderr)
	}
	if err := json.Unmarshal([]byte(world.lastCLI.stdout), &envelope); err != nil {
		return envelope, fmt.Errorf("decoding loam %s output: %w\nstdout: %s", strings.Join(args, " "), err, world.lastCLI.stdout)
	}
	return envelope, nil
}

// scopeArgs returns the scope flags a query needs. Inside a clone the CLI
// infers the repo from the working directory, which is the behaviour the
// "I am working inside a clone" Background exists to set up, so nothing
// is passed; outside one, the repo is named explicitly.
func (w *acceptanceWorld) scopeArgs() []string {
	if w.clonePath != "" {
		return nil
	}
	return []string{"--repo", w.repo()}
}

// stepEnrollmentCompletes is ingestion.feature's "When enrollment
// completes".
func (h *acceptanceHarness) stepEnrollmentCompletes(ctx context.Context) error {
	return h.ingestIndexedBranch(ctx, worldFrom(ctx))
}

// stepBranchHasBeenIngested is ingestion.feature's Given `"main" has been
// ingested`.
func (h *acceptanceHarness) stepBranchHasBeenIngested(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	if branch != world.targetBranch {
		return fmt.Errorf("scenario ingests %q but this repo's indexed branch is %q", branch, world.targetBranch)
	}
	return h.ingestIndexedBranch(ctx, world)
}

// stepIndexedBranchHasBeenIngested is code-intelligence.feature's Background
// "its indexed branch has been ingested".
func (h *acceptanceHarness) stepIndexedBranchHasBeenIngested(ctx context.Context) error {
	return h.ingestIndexedBranch(ctx, worldFrom(ctx))
}

// stepIndexedBranchIsIngested asserts the index now names a real commit
// on the indexed branch, and that it is the mirror's actual tip -- not
// merely that some ingest ran.
func (h *acceptanceHarness) stepIndexedBranchIsIngested(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	if branch != world.targetBranch {
		return fmt.Errorf("scenario names indexed branch %q but this repo's is %q", branch, world.targetBranch)
	}
	ref, err := h.ingestedRef(ctx, world)
	if err != nil {
		return err
	}
	if ref == "" {
		return fmt.Errorf("repo %s reports no ingested commit for %s", world.repo(), branch)
	}
	tip, err := mirrorRefSHA(world.mirrorDir, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if ref != tip {
		return fmt.Errorf("repo %s is indexed at %s but the mirror's %s is at %s", world.repo(), ref, branch, tip)
	}
	return nil
}

// stepGraphAndSearchReturnResults proves the index is queryable, not just
// present: one graph query and one search must each come back with at
// least one row and with the ingested commit named in the envelope.
func (h *acceptanceHarness) stepGraphAndSearchReturnResults(ctx context.Context) error {
	world := worldFrom(ctx)
	ref, err := h.ingestedRef(ctx, world)
	if err != nil {
		return err
	}
	graph, err := h.runQuery(world, append([]string{"graph", "def", acceptanceDefinedSymbol}, world.scopeArgs()...)...)
	if err != nil {
		return err
	}
	if len(graph.Results) == 0 {
		return fmt.Errorf("graph def %s returned no results for the freshly ingested branch", acceptanceDefinedSymbol)
	}
	if err := assertEnvelopeRef(graph, world.repo(), world.targetBranch, ref); err != nil {
		return fmt.Errorf("graph def %s: %w", acceptanceDefinedSymbol, err)
	}
	search, err := h.runQuery(world, append([]string{"search", "how is authentication handled"}, world.scopeArgs()...)...)
	if err != nil {
		return err
	}
	if len(search.Results) == 0 {
		return fmt.Errorf("search returned no results for the freshly ingested branch")
	}
	return assertEnvelopeRef(search, world.repo(), world.targetBranch, ref)
}

// stepBranchAdvancesAddingAndRemoving pushes one upstream commit that
// both adds and removes a symbol, then runs a real sync cycle so advance
// detection is what enqueues the resulting ingest -- the production
// trigger, not a direct Enqueue.
func (h *acceptanceHarness) stepBranchAdvancesAddingAndRemoving(ctx context.Context, branch, added, removed string) error {
	world := worldFrom(ctx)
	if added != acceptanceAddedSymbol || removed != acceptanceRemovedSymbol {
		return fmt.Errorf("scenario advances with add=%q remove=%q, but this fixture's advance adds %q and removes %q",
			added, removed, acceptanceAddedSymbol, acceptanceRemovedSymbol)
	}
	if err := h.forge.AdvanceBranch(ctx, world.repo(), branch, fakeforge.AdvanceOptions{
		Path:    acceptanceAuthFile,
		Content: []byte(acceptanceAdvancedAuthContent),
		Message: "add " + added + ", remove " + removed,
	}); err != nil {
		return fmt.Errorf("advancing %s upstream: %w", branch, err)
	}
	// One real scheduler cycle: fetch, detect the advance, enqueue. The
	// ingest job this step produces is enqueued by production advance
	// detection (mirrorsync's StoreIngestEnqueuer), never by this
	// harness -- which is the whole point of the scenario, since a direct
	// Enqueue here would prove the ingest pipeline works while proving
	// nothing about what triggers it.
	if _, err := h.syncHarness.Tick(ctx); err != nil {
		return fmt.Errorf("running the sync cycle after advancing %s: %w", branch, err)
	}
	return nil
}

// stepAfterIngestionAGraphQueryFinds drains the queue the sync cycle
// filled, then asserts the symbol the advance ADDED now resolves.
func (h *acceptanceHarness) stepAfterIngestionAGraphQueryFinds(ctx context.Context, symbol string) error {
	world := worldFrom(ctx)
	if err := h.ingestHarness.DrainIngestQueue(ctx, mirrorsync.RepoID(world.repo())); err != nil {
		return fmt.Errorf("draining the ingest queue for %s: %w", world.repo(), err)
	}
	if err := h.assertIngestSucceeded(ctx, world); err != nil {
		return err
	}
	envelope, err := h.runQuery(world, append([]string{"graph", "def", symbol}, world.scopeArgs()...)...)
	if err != nil {
		return err
	}
	if len(envelope.Results) == 0 {
		return fmt.Errorf("graph def %s returned no results after the advance was ingested", symbol)
	}
	return nil
}

// stepAGraphQueryNoLongerFinds asserts the symbol the advance REMOVED is
// gone. A removed symbol is a NotFound from the server, which the CLI
// reports as a non-zero exit -- so a zero exit here means the stale
// symbol survived the rebuild, which is the failure this step exists to
// catch.
func (h *acceptanceHarness) stepAGraphQueryNoLongerFinds(ctx context.Context, symbol string) error {
	world := worldFrom(ctx)
	world.lastCLI = h.runLoamCLIIn(world, world.queryDir(), append([]string{"graph", "def", symbol}, world.scopeArgs()...)...)
	if world.lastCLI.exitCode == 0 {
		return fmt.Errorf("graph def %s still resolves after the advance removed it: %s", symbol, world.lastCLI.stdout)
	}
	return nil
}

// stepIAmWorkingInsideACloneOf is code-intelligence.feature's Background
// "I am working inside a clone of ...".
//
// It registers a work branch, points the branch at the indexed branch's
// tip in the mirror the ingest step already cloned, hardens that mirror,
// and then runs a real `loam clone`. Adding the ref to the EXISTING
// mirror matters: seeding a fresh one (the technique clone-and-push's own
// Background uses) would replace the mirror the index was just built
// from, and every subsequent query would be reading an index of a repo
// that no longer exists on disk.
func (h *acceptanceHarness) stepIAmWorkingInsideACloneOf(ctx context.Context, repo string) error {
	world := worldFrom(ctx)
	if repo != world.repo() {
		return fmt.Errorf("scenario clones %q but this scenario's repo is %q", repo, world.repo())
	}
	if world.mirrorDir == "" {
		return fmt.Errorf("no mirror for %s: this step must follow the step that ingests the indexed branch", repo)
	}
	if err := h.insertWorkBranchRow(ctx, world.repoID, world.workBranch, world.targetBranch, "draft", world.agentIdentifier()); err != nil {
		return err
	}
	tip, err := mirrorRefSHA(world.mirrorDir, "refs/heads/"+world.targetBranch)
	if err != nil {
		return err
	}
	if out, err := runPlainGit("", "--git-dir="+world.mirrorDir, "update-ref", refnames.WorkBranch(world.workBranch), tip); err != nil {
		return fmt.Errorf("creating %s in the mirror: %w\n%s", world.workBranch, err, out)
	}
	if err := h.reconcileSeededMirror(ctx, world.mirrorDir); err != nil {
		return err
	}
	result := h.runLoamCLI(world, "clone", repo, world.workBranch)
	if result.exitCode != 0 {
		return fmt.Errorf("loam clone %s %s exited %d\nstdout: %s\nstderr: %s",
			repo, world.workBranch, result.exitCode, result.stdout, result.stderr)
	}
	world.clonePath = filepath.Join(world.workspace, world.repoName)
	return assertDirExists(world.clonePath)
}

// stepIAskTheGraphWhereIsDefined is "When I ask the graph where X is
// defined". No scope flag is passed when the scenario established a
// clone, so the CLI's own directory-based scope inference is what
// resolves the repo.
func (h *acceptanceHarness) stepIAskTheGraphWhereIsDefined(ctx context.Context, symbol string) error {
	world := worldFrom(ctx)
	_, err := h.runQuery(world, append([]string{"graph", "def", symbol}, world.scopeArgs()...)...)
	return err
}

// stepIGetTheFileAndLineOfItsDefinition asserts the definition rows carry
// a real file AND a real line, not merely that some row came back: a row
// with an empty file or a zero line would still be "a result" while
// answering none of the question the scenario asked.
func (h *acceptanceHarness) stepIGetTheFileAndLineOfItsDefinition(ctx context.Context) error {
	world := worldFrom(ctx)
	var envelope acceptanceEnvelope
	if err := json.Unmarshal([]byte(world.lastCLI.stdout), &envelope); err != nil {
		return fmt.Errorf("decoding the last graph query's output: %w\nstdout: %s", err, world.lastCLI.stdout)
	}
	if len(envelope.Results) == 0 {
		return fmt.Errorf("the graph query returned no definitions: %s", world.lastCLI.stdout)
	}
	for i, row := range envelope.Results {
		file, _ := row["file"].(string)
		line, _ := row["line"].(float64)
		if file == "" {
			return fmt.Errorf("definition %d names no file: %v", i, row)
		}
		if line <= 0 {
			return fmt.Errorf("definition %d names no line: %v", i, row)
		}
	}
	return nil
}

// stepISearchFor is "When I search for ...".
func (h *acceptanceHarness) stepISearchFor(ctx context.Context, query string) error {
	world := worldFrom(ctx)
	_, err := h.runQuery(world, append([]string{"search", query}, world.scopeArgs()...)...)
	return err
}

// stepIGetTheMostRelevantChunks asserts search returned chunks from BOTH
// a document and a code file. "doc and code chunks" is the scenario's own
// wording, and a result set drawn from only one of the two would satisfy
// a bare non-empty check while failing the thing the scenario claims.
func (h *acceptanceHarness) stepIGetTheMostRelevantChunks(ctx context.Context) error {
	world := worldFrom(ctx)
	var envelope acceptanceEnvelope
	if err := json.Unmarshal([]byte(world.lastCLI.stdout), &envelope); err != nil {
		return fmt.Errorf("decoding the last search output: %w\nstdout: %s", err, world.lastCLI.stdout)
	}
	if len(envelope.Results) == 0 {
		return fmt.Errorf("search returned no chunks: %s", world.lastCLI.stdout)
	}
	var sawDoc, sawCode bool
	for _, row := range envelope.Results {
		file, _ := row["file"].(string)
		switch {
		case strings.HasSuffix(file, ".md"):
			sawDoc = true
		case strings.HasSuffix(file, ".go"):
			sawCode = true
		}
	}
	if !sawDoc || !sawCode {
		return fmt.Errorf("search returned doc=%t code=%t chunks, want both: %s", sawDoc, sawCode, world.lastCLI.stdout)
	}
	return nil
}

// stepEachResultNamesRepoFileAndLines asserts the provenance half of the
// scenario: every row names its repo, its file, and a well-formed,
// non-inverted line range.
func (h *acceptanceHarness) stepEachResultNamesRepoFileAndLines(ctx context.Context) error {
	world := worldFrom(ctx)
	var envelope acceptanceEnvelope
	if err := json.Unmarshal([]byte(world.lastCLI.stdout), &envelope); err != nil {
		return fmt.Errorf("decoding the last search output: %w\nstdout: %s", err, world.lastCLI.stdout)
	}
	if len(envelope.Results) == 0 {
		return fmt.Errorf("search returned no chunks to check provenance on")
	}
	for i, row := range envelope.Results {
		repo, _ := row["repo"].(string)
		file, _ := row["file"].(string)
		lines, _ := row["lines"].([]any)
		if repo != world.repo() {
			return fmt.Errorf("result %d names repo %q, want %q", i, repo, world.repo())
		}
		if file == "" {
			return fmt.Errorf("result %d names no file: %v", i, row)
		}
		if len(lines) != 2 {
			return fmt.Errorf("result %d has line range %v, want exactly [start, end]", i, lines)
		}
		start, _ := lines[0].(float64)
		end, _ := lines[1].(float64)
		if start <= 0 || end < start {
			return fmt.Errorf("result %d has line range [%v, %v], which is not a real range", i, start, end)
		}
	}
	return nil
}

// stepBranchHasAdvancedPastTheLastIngestedCommit moves upstream forward
// and deliberately runs NO sync and NO ingest, so the index is genuinely
// behind the branch's real tip when the query runs. Ingesting here would
// make the "results predate the tip" assertion below unsatisfiable, since
// the two would be equal.
func (h *acceptanceHarness) stepBranchHasAdvancedPastTheLastIngestedCommit(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	return h.forge.AdvanceBranch(ctx, world.repo(), branch, fakeforge.AdvanceOptions{
		Path:    "NOTES.md",
		Content: []byte("# Notes\n\nCommitted after the last ingest.\n"),
		Message: "advance past the ingested commit",
	})
}

// stepIRunAGraphQuery is "When I run a graph query" -- any query; the
// scenario is about the envelope, not the rows.
func (h *acceptanceHarness) stepIRunAGraphQuery(ctx context.Context) error {
	world := worldFrom(ctx)
	_, err := h.runQuery(world, append([]string{"graph", "def", acceptanceDefinedSymbol}, world.scopeArgs()...)...)
	return err
}

// stepResponseNamesTheCommitTheIndexWasBuiltFrom asserts the envelope's
// ingested entry names the exact commit repo_target_branches recorded,
// rather than merely carrying some non-empty string.
func (h *acceptanceHarness) stepResponseNamesTheCommitTheIndexWasBuiltFrom(ctx context.Context) error {
	world := worldFrom(ctx)
	ref, err := h.ingestedRef(ctx, world)
	if err != nil {
		return err
	}
	if ref == "" {
		return fmt.Errorf("repo %s records no ingested commit, so no response could name one", world.repo())
	}
	var envelope acceptanceEnvelope
	if err := json.Unmarshal([]byte(world.lastCLI.stdout), &envelope); err != nil {
		return fmt.Errorf("decoding the last graph query's output: %w\nstdout: %s", err, world.lastCLI.stdout)
	}
	return assertEnvelopeRef(envelope, world.repo(), world.targetBranch, ref)
}

// stepResultsPredateTheTipOf asserts the caller can SEE the index is
// stale: the commit the response names differs from the branch's real
// upstream tip.
func (h *acceptanceHarness) stepResultsPredateTheTipOf(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	tip, err := h.upstreamRefSHA(ctx, world, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	ref, err := h.ingestedRef(ctx, world)
	if err != nil {
		return err
	}
	if ref == tip {
		return fmt.Errorf("the index is at %s, the same commit as upstream's %s tip: nothing marks the results as stale", ref, branch)
	}
	return nil
}

// --- loam-7d0: "Edges reflect the current code even in unchanged files" ---
//
// This scenario stays @wip. It was implemented literally and run: given
// file "handler.go" references "Login" defined in "auth.go", And only
// "auth.go" changes to rename "Login" to "Authenticate", a graph query for
// references to "Authenticate" returned zero rows for handler.go (verified
// against the real pipeline, not inferred).
//
// The reason is structural, not a missing step definition.
// internal/codegraph.Store.RecomputeGraphEdges resolves graph_edges by
// joining symbol_references AGAINST symbols BY NAME
// (internal/db/queries/code_graph.sql's ResolveGraphEdgeCandidates), and
// `graph refs` (LookupReferencesByName) reads symbol_references directly,
// also by name -- see that query's own doc comment: "intra-repo,
// name-based, approximate" (docs/ingestion-spec.md "Edge resolution").
// handler.go's OWN reference row is written once, at whatever ingest last
// reparsed handler.go, and holds the literal text "Login". Renaming Login
// to Authenticate touches only auth.go; diffplan's own incremental
// contract (TestPlan_Incremental_ClassifiesAddModifyDeleteRename_
// UnchangedFileNeverAppears, internal/diffplan/plan_test.go) guarantees
// handler.go is never reparsed for that commit, so its stored reference
// name never becomes "Authenticate" -- not on an incremental ingest, and
// not on a full one either, since a full rebuild reparses handler.go's
// UNCHANGED bytes and extracts the same literal "Login" text again.
// docs/ingestion-spec.md's own "moved" language ("correct against the
// current symbol set even when an unchanged file referenced a symbol that
// MOVED") describes a symbol relocating to a different FILE while keeping
// its name -- genuinely handled by this name-based join -- not a rename,
// which is a different operation this schema has no mechanism to track
// (there is no stable symbol identity across a rename: ReplaceFileSymbols
// deletes and reinserts with a fresh uuid.NewV7 every time). This is also
// consistent with internal/ingest/orchestrator's own integration test
// covering this exact rename fixture
// (TestIngest_MidFlightReaderSeesThePreviousIndexUntilTheSingleCommit,
// integration_test.go): it asserts on `symbols` (Login gone, Authenticate
// present) and never asserts anything about handler.go's edges/references,
// because there is nothing true to assert there.
//
// Per this bead's own guidance (and loam-ofg.18's precedent), the tag
// stays rather than weakening the scenario or its assertions to pass.

// --- loam-7d0: "The admin can force a reindex" ---

// stepIReindex is "When I reindex X": ensures a mirror exists (mirroring
// what a real prior enrollment would already have done) and then drives
// the REAL RepoAdminService.ReindexRepo RPC, never a direct Enqueue.
func (h *acceptanceHarness) stepIReindex(ctx context.Context, repo string) error {
	world := worldFrom(ctx)
	if repo != world.repo() {
		return fmt.Errorf("scenario reindexes %q but this scenario's repo is %q", repo, world.repo())
	}
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	resp, err := h.newRepoAdminServiceClient().ReindexRepo(ctx, connect.NewRequest(&adminv1.ReindexRepoRequest{Repo: repo}))
	if err != nil {
		return fmt.Errorf("reindexing %s: %w", repo, err)
	}
	if resp.Msg.GetJob().GetKind() != adminv1.IngestKind_INGEST_KIND_FULL {
		return fmt.Errorf("ReindexRepo enqueued a %s job for %s, want FULL", resp.Msg.GetJob().GetKind(), repo)
	}
	world.lastReindexJob = resp.Msg.GetJob()
	return nil
}

// stepAFullIngestJobRunsForIt drains the job ReindexRepo enqueued and
// requires it to have actually succeeded, as kind FULL, for the SAME
// target branch ReindexRepo's own response named.
func (h *acceptanceHarness) stepAFullIngestJobRunsForIt(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastReindexJob == nil {
		return fmt.Errorf("no reindex was requested in this scenario yet")
	}
	if err := h.ingestHarness.DrainIngestQueue(ctx, mirrorsync.RepoID(world.repo())); err != nil {
		return fmt.Errorf("draining the ingest queue for %s: %w", world.repo(), err)
	}
	var status, kind, jobError string
	err := h.server.pool.QueryRow(ctx,
		`SELECT status, kind, COALESCE(error, '') FROM ingest_jobs WHERE repo_id = $1 AND target_branch = $2 ORDER BY queued_at DESC LIMIT 1`,
		world.repoID, world.lastReindexJob.GetTargetBranch()).Scan(&status, &kind, &jobError)
	if err != nil {
		return fmt.Errorf("reading the latest ingest job for %s: %w", world.repo(), err)
	}
	if kind != "full" {
		return fmt.Errorf("latest ingest job for %s is kind %q, want full", world.repo(), kind)
	}
	if status != "succeeded" {
		return fmt.Errorf("the reindex job for %s finished as %q: %s", world.repo(), status, jobError)
	}
	return nil
}

// stepOnceItSucceedsQueriesReflectTheCurrentIndexedBranch reuses
// stepGraphAndSearchQueriesReflect (acceptance_enrollment_test.go) against
// ReindexRepo's own named target branch.
func (h *acceptanceHarness) stepOnceItSucceedsQueriesReflectTheCurrentIndexedBranch(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastReindexJob == nil {
		return fmt.Errorf("no reindex was requested in this scenario yet")
	}
	return h.stepGraphAndSearchQueriesReflect(ctx, world.lastReindexJob.GetTargetBranch())
}

// --- loam-7d0: "Viewing ingest job activity" ---

// stepIngestJobsHaveRunForEnrolledRepos is the Given: it drives one real,
// successful ingest for this scenario's own repo through the same
// clone-and-Enqueue-KindFull sequence every other Given in this file uses,
// so the Jobs view has a genuine row to show.
func (h *acceptanceHarness) stepIngestJobsHaveRunForEnrolledRepos(ctx context.Context) error {
	return h.ingestIndexedBranch(ctx, worldFrom(ctx))
}

// stepIOpenTheJobsView drives the REAL RepoAdminService.ListIngestJobs RPC
// with no filter, exactly what the web Jobs view itself calls
// (docs/web-spec.md), and records the page for the following Then.
func (h *acceptanceHarness) stepIOpenTheJobsView(ctx context.Context) error {
	world := worldFrom(ctx)
	resp, err := h.newRepoAdminServiceClient().ListIngestJobs(ctx, connect.NewRequest(&adminv1.ListIngestJobsRequest{}))
	if err != nil {
		return fmt.Errorf("listing ingest jobs: %w", err)
	}
	world.lastIngestJobs = resp.Msg.GetJobs()
	return nil
}

// stepISeeEachJobsRepoStatusAndTiming asserts every returned job names a
// real repo, a real (non-UNSPECIFIED) status, and a real queued_at timing
// -- and that this scenario's OWN just-ingested repo is genuinely among
// them, so this cannot pass against an empty or unrelated page.
func (h *acceptanceHarness) stepISeeEachJobsRepoStatusAndTiming(ctx context.Context) error {
	world := worldFrom(ctx)
	if len(world.lastIngestJobs) == 0 {
		return fmt.Errorf("the Jobs view returned no jobs")
	}
	var sawThisRepo bool
	for i, job := range world.lastIngestJobs {
		if job.GetRepo() == "" {
			return fmt.Errorf("job %d names no repo", i)
		}
		if job.GetStatus() == adminv1.IngestStatus_INGEST_STATUS_UNSPECIFIED {
			return fmt.Errorf("job %d for %s reports no status", i, job.GetRepo())
		}
		if job.GetQueuedAt() == "" {
			return fmt.Errorf("job %d for %s reports no queued_at timing", i, job.GetRepo())
		}
		if job.GetRepo() == world.repo() {
			sawThisRepo = true
		}
	}
	if !sawThisRepo {
		return fmt.Errorf("the Jobs view did not include this scenario's own repo %s among %d job(s)", world.repo(), len(world.lastIngestJobs))
	}
	return nil
}

// --- loam-7d0: "A failed ingest keeps the previous index" ---

// stepBranchHasBeenIngestedSuccessfully is the Given "X has been ingested
// successfully" -- the same real ingest stepBranchHasBeenIngested drives,
// under this scenario's own wording.
func (h *acceptanceHarness) stepBranchHasBeenIngestedSuccessfully(ctx context.Context, branch string) error {
	return h.stepBranchHasBeenIngested(ctx, branch)
}

// stepTheNextIngestionFails deliberately removes the LOCAL bare mirror on
// disk -- the local-storage analogue of stepUpstreamForgeIsUnreachable
// (acceptance_sync_test.go), and the exact "mirror missing or invalid"
// fault internal/ingest/orchestrator's gitReader.ResolveRef already
// classifies as errMirrorMissing -- then drives a real ingest job for it
// through the SAME live ingest.Pool every other scenario in this file
// uses. This is an environment-level fault, not a stubbed collaborator or
// a hand-written failed row: the orchestrator's own rollback and
// retry/backoff logic is what is actually being exercised.
func (h *acceptanceHarness) stepTheNextIngestionFails(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.mirrorDir == "" {
		return fmt.Errorf("no mirror exists yet for %s to break", world.repo())
	}
	ref, err := h.ingestedRef(ctx, world)
	if err != nil {
		return err
	}
	if ref == "" {
		return fmt.Errorf("repo %s has no ingested commit recorded yet; nothing to keep", world.repo())
	}
	world.ingestedRefBeforeFailure = ref
	if err := os.RemoveAll(world.mirrorDir); err != nil {
		return fmt.Errorf("removing the mirror for %s to force a real ingest failure: %w", world.repo(), err)
	}
	if err := h.server.ingestPool.Enqueue(ctx, world.repoID, world.targetBranch, ingest.KindIncremental); err != nil {
		return fmt.Errorf("enqueuing the next ingest for %s: %w", world.repo(), err)
	}
	if err := h.ingestHarness.DrainIngestQueue(ctx, mirrorsync.RepoID(world.repo())); err != nil {
		return fmt.Errorf("draining the ingest queue for %s: %w", world.repo(), err)
	}
	var id uuid.UUID
	var status, jobError string
	if err := h.server.pool.QueryRow(ctx,
		`SELECT id, status, COALESCE(error, '') FROM ingest_jobs WHERE repo_id = $1 AND target_branch = $2 ORDER BY queued_at DESC LIMIT 1`,
		world.repoID, world.targetBranch).Scan(&id, &status, &jobError); err != nil {
		return fmt.Errorf("reading the latest ingest job for %s: %w", world.repo(), err)
	}
	if status != "failed" {
		return fmt.Errorf("the next ingest for %s finished as %q, want failed (the mirror was deliberately removed): %s", world.repo(), status, jobError)
	}
	if jobError == "" {
		return fmt.Errorf("job %s for %s is failed but recorded no error", id, world.repo())
	}
	world.lastFailedJobID = id
	return nil
}

// stepTheJobIsShownAsFailedWithItsError re-reads the job through the REAL
// admin surface (ListIngestJobs), the same RPC the web Jobs view calls,
// rather than trusting the raw ingest_jobs read stepTheNextIngestionFails
// already did.
func (h *acceptanceHarness) stepTheJobIsShownAsFailedWithItsError(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastFailedJobID == (uuid.UUID{}) {
		return fmt.Errorf("no ingest job has failed in this scenario yet")
	}
	repo := world.repo()
	resp, err := h.newRepoAdminServiceClient().ListIngestJobs(ctx, connect.NewRequest(&adminv1.ListIngestJobsRequest{Repo: &repo}))
	if err != nil {
		return fmt.Errorf("listing ingest jobs for %s: %w", repo, err)
	}
	for _, job := range resp.Msg.GetJobs() {
		if job.GetId() != world.lastFailedJobID.String() {
			continue
		}
		if job.GetStatus() != adminv1.IngestStatus_INGEST_STATUS_FAILED {
			return fmt.Errorf("job %s for %s is shown as %s, want FAILED", job.GetId(), repo, job.GetStatus())
		}
		if job.GetError() == "" {
			return fmt.Errorf("job %s for %s is shown as failed but names no error", job.GetId(), repo)
		}
		return nil
	}
	return fmt.Errorf("the Jobs view does not include job %s for %s", world.lastFailedJobID, repo)
}

// stepGraphAndSearchQueriesStillReturnThePreviousIndex reuses
// stepGraphAndSearchReturnResults, which reads ingested_ref FRESH and
// requires both a graph and a search query to still name it -- exactly
// "the previous index" this Then names, since the failed ingest never
// advanced that column.
func (h *acceptanceHarness) stepGraphAndSearchQueriesStillReturnThePreviousIndex(ctx context.Context) error {
	return h.stepGraphAndSearchReturnResults(ctx)
}

// stepTheReportedIngestedCommitIsUnchanged compares the CURRENT
// ingested_ref against the value stepTheNextIngestionFails captured before
// deliberately breaking the mirror.
func (h *acceptanceHarness) stepTheReportedIngestedCommitIsUnchanged(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.ingestedRefBeforeFailure == "" {
		return fmt.Errorf("no pre-failure ingested ref was recorded for %s", world.repo())
	}
	ref, err := h.ingestedRef(ctx, world)
	if err != nil {
		return err
	}
	if ref != world.ingestedRefBeforeFailure {
		return fmt.Errorf("ingested_ref for %s changed to %s after the failed ingest, want it unchanged at %s", world.repo(), ref, world.ingestedRefBeforeFailure)
	}
	return nil
}

// stepTheJobIsRetried polls ingest_jobs.attempts for the specific job
// stepTheNextIngestionFails recorded until it reaches 2 or
// acceptanceRetryPollTimeout elapses. The mirror is still missing (this
// scenario never repairs it), so the automatic retry the production
// Pool's backoff schedules fails again for real, incrementing attempts a
// second time -- proof of an actual retry, not merely that the row still
// exists.
func (h *acceptanceHarness) stepTheJobIsRetried(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastFailedJobID == (uuid.UUID{}) {
		return fmt.Errorf("no ingest job has failed in this scenario yet")
	}
	deadline := time.Now().Add(acceptanceRetryPollTimeout)
	var attempts int
	for {
		if err := h.server.pool.QueryRow(ctx,
			`SELECT attempts FROM ingest_jobs WHERE id = $1`, world.lastFailedJobID).Scan(&attempts); err != nil {
			return fmt.Errorf("reading attempts for job %s: %w", world.lastFailedJobID, err)
		}
		if attempts >= 2 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("job %s for %s was not retried within %s: attempts is still %d", world.lastFailedJobID, world.repo(), acceptanceRetryPollTimeout, attempts)
		}
		time.Sleep(acceptanceRetryPollInterval)
	}
}

// assertEnvelopeRef checks every ingested entry names repo, target, and
// the expected commit. Every entry, not just the first: checking one
// would silently pass a fan-out in which some other repo in scope
// reported a stale ref.
func assertEnvelopeRef(envelope acceptanceEnvelope, repo, target, ref string) error {
	if len(envelope.Ingested) == 0 {
		return fmt.Errorf("the response named no commit its index was built from")
	}
	for i, in := range envelope.Ingested {
		if in.Repo != repo {
			return fmt.Errorf("ingested[%d].repo is %q, want %q", i, in.Repo, repo)
		}
		if in.Target != target {
			return fmt.Errorf("ingested[%d].target is %q, want %q", i, in.Target, target)
		}
		if in.Ref != ref {
			return fmt.Errorf("ingested[%d].ref is %q, want the recorded ingested commit %q", i, in.Ref, ref)
		}
		if in.At == "" {
			return fmt.Errorf("ingested[%d].at is empty, so the response reports no ingest time", i)
		}
	}
	return nil
}
