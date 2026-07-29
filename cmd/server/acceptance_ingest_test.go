//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cucumber/godog"

	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/refnames"
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
