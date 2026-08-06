package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/diffplan"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/ingest/chunker"
	"github.com/bobcob7/loam/internal/ingest/graph"
	"github.com/bobcob7/loam/internal/ingest/vectors"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/reposstore"
)

// GrammarVersion identifies the Tree-sitter grammar set and the extraction
// query sources this binary indexes with. It is recorded in
// repo_target_branches.ingested_versions and compared against on the next
// ingest: a change here makes internal/diffplan escalate that repo's next
// incremental job to a full rebuild, because symbols extracted by a
// different grammar/query set cannot be mixed with symbols extracted by
// this one (docs/ingestion-spec.md "Incremental Build" -> "Full rebuild":
// "a Tree-sitter grammar / pipeline version bump").
//
// Bump this whenever internal/parser's registered grammars change (a
// grammar module version, a language added or removed) or
// internal/ingest/graph's query sources change. It is a deliberate manual
// version, not a hash of anything: the point is that a human decides an
// index built by the old code is no longer reusable, and a hash would
// force a full rebuild of every enrolled repo on changes that do not
// affect extraction at all.
const GrammarVersion = "1"

// PipelineVersion identifies the ingest pipeline's own shape -- chunking
// strategy, budget enforcement, what a chunk's text contains, what
// symbol/reference kinds mean. Same recorded-and-compared mechanism as
// GrammarVersion, and the same "bump deliberately" rule: change it when an
// index built by the previous version could no longer be read correctly
// alongside one built by this one.
const PipelineVersion = "1"

// errNoSuchTargetBranch means job.TargetBranch is not enrolled for
// job.RepoID at all, so there is no repo_target_branches row to read a
// diff base from or write an ingested_ref back to. That is a configuration
// fault, not a transient one: an ingest job exists for a branch nothing
// enrolled.
var errNoSuchTargetBranch = errors.New("orchestrator: target branch not enrolled for this repo")

// Orchestrator implements internal/ingest.Orchestrator: it runs one
// claimed ingest job end to end and reports the stats the worker persists
// on success. Construct with New. A single Orchestrator is safe to reuse
// across jobs and across the worker pool's goroutines -- every
// collaborator it holds is either stateless or internally synchronized,
// and each Run call owns its own compute and its own transaction.
type Orchestrator struct {
	logger   *slog.Logger
	dataDir  string
	planner  planner
	repos    repoReader
	content  contentReader
	graph    graphTrack
	chunker  fileChunker
	vectors  vectorTrack
	dropper  dropper
	refs     refWriter
	ledger   rejectionLedger
	tx       transactor
	budgeter chunker.Budgeter
	versions diffplan.Versions
	nowFunc  func() time.Time
}

// newOrchestrator is New's seam-taking core: every collaborator as an
// interface, so this package's unit tests drive the whole Run sequence
// against moq mocks -- including the transaction boundary itself -- with
// no database, no git mirror, and no embedder. New wires the production
// implementations into it.
func newOrchestrator(
	logger *slog.Logger,
	dataDir string,
	p planner,
	repos repoReader,
	content contentReader,
	g graphTrack,
	ch fileChunker,
	v vectorTrack,
	d dropper,
	refs refWriter,
	ledger rejectionLedger,
	tx transactor,
	budgeter chunker.Budgeter,
	versions diffplan.Versions,
) *Orchestrator {
	return &Orchestrator{
		logger:   logger,
		dataDir:  dataDir,
		planner:  p,
		repos:    repos,
		content:  content,
		graph:    g,
		chunker:  ch,
		vectors:  v,
		dropper:  d,
		refs:     refs,
		ledger:   ledger,
		tx:       tx,
		budgeter: budgeter,
		versions: versions,
		nowFunc:  time.Now,
	}
}

// Run implements internal/ingest.Orchestrator for one claimed job. See the
// package doc comment for the phase split and the write order; this
// function is deliberately a short sequence of named steps so that order
// is readable in one screen.
//
// It resolves the job's refs itself, as internal/ingest.Job's own doc
// comment requires: the diff base is repo_target_branches.ingested_ref and
// the new tip is whatever the bare mirror currently has for the target
// branch. Neither is carried on the job row.
//
// Every failure returns an error and rolls back, leaving the previous
// index and the previous ingested_ref exactly as they were. Nothing here
// indexes a slice positionally without having checked its length, and no
// code path panics deliberately -- but that is now a cleanliness property,
// not a survival one: loam-337 gave internal/ingest.Pool a per-job recover
// (Pool.runOrchestrator), so a panic escaping this function fails that one
// job through the ordinary fail() path -- status, error, attempts,
// sync_state, sync_error, backoff/retry -- instead of terminating the
// server process. Keep returning errors rather than panicking anyway: an
// error carries its own context, a recovered panic only carries a stack in
// the log.
func (o *Orchestrator) Run(ctx context.Context, job ingest.Job) (ingest.Stats, error) {
	repo, err := o.repos.GetRepoByID(ctx, job.RepoID)
	if err != nil {
		return ingest.Stats{}, fmt.Errorf("resolving repo %s for ingest: %w", job.RepoID, err)
	}
	mirrorDir := mirrorpath.Dir(o.dataDir, repo.Name)
	target, err := o.targetBranch(ctx, job)
	if err != nil {
		return ingest.Stats{}, err
	}
	newRef, err := o.content.ResolveRef(ctx, mirrorDir, job.TargetBranch)
	if err != nil {
		return ingest.Stats{}, fmt.Errorf("resolving %s in mirror %s: %w", job.TargetBranch, mirrorDir, err)
	}
	plan, err := o.planner.Plan(ctx, mirrorDir, o.planRequest(job, target, newRef))
	if err != nil {
		return ingest.Stats{}, fmt.Errorf("planning ingest for %s@%s: %w", repo.Name, job.TargetBranch, err)
	}
	ledgered, err := o.ledger.List(ctx, job.RepoID, job.TargetBranch)
	if err != nil {
		return ingest.Stats{}, fmt.Errorf("reading the rejection ledger for %s@%s: %w", repo.Name, job.TargetBranch, err)
	}
	retry := chunkstore.PendingPaths(ledgered)
	plan = plan.WithRetryPaths(retry)
	o.logger.InfoContext(ctx, "planned ingest",
		"repo", repo.Name, "target_branch", job.TargetBranch, "job_id", job.ID,
		"kind", plan.Kind, "escalation_reason", plan.Reason,
		"drop_files", len(plan.DropFiles), "reparse_files", len(plan.ReparseFiles),
		"ledgered_files", len(ledgered), "retried_files", len(retry))
	o.warnIfExhausted(ctx, repo.Name, job, ledgered)
	files, err := o.content.ReadFiles(ctx, mirrorDir, newRef, plan.ReparseFiles)
	if err != nil {
		return ingest.Stats{}, fmt.Errorf("reading %d file(s) at %s from mirror %s: %w", len(plan.ReparseFiles), newRef, mirrorDir, err)
	}
	work, err := o.compute(ctx, job, files)
	if err != nil {
		return ingest.Stats{}, err
	}
	var written writeResult
	if err := o.tx.withinTx(ctx, func(tx pgx.Tx) error {
		var err error
		written, err = o.writeSwap(ctx, tx, job, plan, newRef, work, ledgered)
		return err
	}); err != nil {
		return ingest.Stats{}, err
	}
	stats := ingest.Stats{
		FilesParsed:    work.graphStats.FilesExtracted,
		ChunksEmbedded: written.chunkStats.ChunksWritten,
		FilesRejected:  written.chunkStats.FilesRejected,
	}
	o.logger.InfoContext(ctx, "ingest committed",
		"repo", repo.Name, "target_branch", job.TargetBranch, "job_id", job.ID, "kind", plan.Kind,
		"ingested_ref", newRef, "files_parsed", stats.FilesParsed, "chunks_embedded", stats.ChunksEmbedded,
		"files_rejected", stats.FilesRejected,
		"edges_recomputed", written.graphStats.EdgesRecomputed)
	o.warnIfPartial(ctx, repo.Name, job, newRef, stats, written.chunkStats.Rejected)
	return stats, nil
}

// warnIfPartial emits the one line that says a SUCCESSFUL ingest was
// incomplete. The count is already a field on "ingest committed" above, but
// that line is logged at INFO and its message says the job worked, so a
// non-zero field on it is only findable by someone who already suspects
// something and knows which field to look at. Operators alert on LEVEL, so
// the incompleteness needs a level of its own or it is not reachable by
// anything except a query written after the fact.
//
// It is a separate line rather than a variable level on "ingest committed"
// deliberately: that message is what an operator greps to enumerate
// completed ingests, and moving the partial ones to WARN under the same
// message would quietly drop them out of an INFO-level grep -- hiding
// exactly the ingests this bead exists to surface.
//
// It now names the FILES as well as the count (loam-qj21). Before the
// ledger existed, the per-file ERROR lines carried no job id and this line
// carried no paths, so the count was joinable to a job and the filenames
// were not -- the paths were recoverable only by correlating two log
// streams by timestamp. The ledger is the durable answer (its rows carry
// job_id); this attribute is the one an operator reading logs already has
// in front of them.
//
// The message says "will be retried", and that is a claim this change had
// to earn: it is true only because the same transaction ledgered these
// paths and the next ingest unions them into its plan. Each path is
// tagged with what a SEARCHER sees for it right now, which is not the same
// question and not the same urgency -- 'stale' means the file's prior
// chunks survived the rollback and it still answers searches from an older
// commit, 'absent' means there were none to survive and it is out of the
// index entirely.
//
// This is the loudest surface a rejection gets, and that is a decision,
// not a limit reached: see internal/ingest.Stats.FilesRejected for why
// repos.sync_state stays 'idle'.
func (o *Orchestrator) warnIfPartial(ctx context.Context, repoName string, job ingest.Job, newRef string, stats ingest.Stats, rejected []vectors.Rejection) {
	if stats.FilesRejected == 0 {
		return
	}
	files := make([]string, 0, len(rejected))
	stale := make([]string, 0, len(rejected))
	absent := make([]string, 0, len(rejected))
	for _, r := range rejected {
		files = append(files, r.Path)
		if r.ChunksState == chunkstore.ChunksStale {
			stale = append(stale, r.Path)
			continue
		}
		absent = append(absent, r.Path)
	}
	o.logger.WarnContext(ctx, "ingest committed with rejected files; they are recorded in the rejection ledger and will be retried by the next ingest",
		"repo", repoName, "target_branch", job.TargetBranch, "job_id", job.ID,
		"ingested_ref", newRef, "files_rejected", stats.FilesRejected, "files_parsed", stats.FilesParsed,
		"files", files, "stale_chunks", stale, "absent_chunks", absent)
}

// targetBranch finds job.TargetBranch's repo_target_branches row, which
// carries the diff base (ingested_ref) and the versions the current index
// was built with. A branch with no row at all is errNoSuchTargetBranch,
// distinct from an enrolled branch that has simply never been ingested --
// the latter is the ordinary first-ingest case the planner escalates to a
// full rebuild, not an error.
func (o *Orchestrator) targetBranch(ctx context.Context, job ingest.Job) (reposstore.TargetBranch, error) {
	branches, err := o.repos.ListTargetBranches(ctx, job.RepoID)
	if err != nil {
		return reposstore.TargetBranch{}, fmt.Errorf("listing target branches for repo %s: %w", job.RepoID, err)
	}
	for _, b := range branches {
		if b.Branch == job.TargetBranch {
			return b, nil
		}
	}
	return reposstore.TargetBranch{}, fmt.Errorf("ingesting %s for repo %s: %w", job.TargetBranch, job.RepoID, errNoSuchTargetBranch)
}

// planRequest assembles the planner's input. OldRef is left empty when the
// branch has never been ingested (IngestedRef.Ok false), which
// internal/diffplan reads as "first ingest"; the stored version triple is
// nil when nothing was ever recorded, which it treats as a mismatch. Both
// of those escalations belong to the planner, not here -- this function
// only reports state, it never decides a Kind.
func (o *Orchestrator) planRequest(job ingest.Job, target reposstore.TargetBranch, newRef string) diffplan.Request {
	req := diffplan.Request{
		NewRef:          newRef,
		RequestedKind:   job.Kind,
		CurrentVersions: o.versions,
		StoredVersions:  decodeVersions(o.logger, target.IngestedVersions),
	}
	if target.IngestedRef.Ok {
		req.OldRef = target.IngestedRef.Ref
	}
	return req
}

// decodeVersions parses a repo_target_branches.ingested_versions payload
// back into a diffplan.Versions. Anything unparseable (a column written by
// an older or newer shape, or hand-edited) returns nil, which the planner
// treats exactly as "never recorded": a full rebuild. That is the safe
// direction -- guessing at a partially understood version triple could
// certify an incremental reuse that is not actually safe.
func decodeVersions(logger *slog.Logger, raw json.RawMessage) *diffplan.Versions {
	if len(raw) == 0 {
		return nil
	}
	var v diffplan.Versions
	if err := json.Unmarshal(raw, &v); err != nil {
		logger.Warn("ignoring unparseable ingested_versions; treating it as a version mismatch", "error", err)
		return nil
	}
	return &v
}

// computeResult is everything the compute phase produced, ready for the
// write phase. It holds no database handle and no open transaction.
type computeResult struct {
	extracted  graph.Extracted
	graphStats graph.Stats
	prepared   vectors.Prepared
	embedStats vectors.Stats
	chunkStats chunker.Stats
	// attempted is every path the chunk track actually tried to write, in
	// plan order: the plan's reparse set MINUS whatever the mirror could
	// not hand back (contentReader.ReadFiles skips a path that no longer
	// resolves to a blob). writeSwap needs exactly this set, not the
	// plan's, to decide which ledger rows this ingest is entitled to
	// clear -- a ledgered path the reader silently skipped was never
	// retried, and clearing it would delete the only record that its
	// chunks are still missing.
	attempted []string
}

// writeResult is what the write phase reported back, for the job's stats
// and the commit log line. Deliberately separate from computeResult: these
// counters are only true if the transaction actually committed.
type writeResult struct {
	graphStats graph.Stats
	chunkStats vectors.Stats
}

// compute runs both tracks concurrently and returns once both have
// finished. Neither track touches the database, so there is no shared
// transaction to serialize and nothing here can race on one; the only
// shared state is the files slice, which both tracks read and neither
// writes, and the two disjoint sets of computeResult fields each track
// assigns before g.Wait() returns (which is the happens-before edge that
// makes reading them here safe).
//
// errgroup's derived context cancels the sibling track the moment either
// one fails, so a failed embed does not leave a whole-repo parse running
// to completion for a transaction that will never open.
func (o *Orchestrator) compute(ctx context.Context, job ingest.Job, files []File) (computeResult, error) {
	var out computeResult
	out.attempted = make([]string, len(files))
	for i, f := range files {
		out.attempted[i] = f.Path
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		inputs := make([]graph.FileInput, len(files))
		for i, f := range files {
			inputs[i] = graph.FileInput{Path: f.Path, Content: f.Content}
		}
		extracted, stats, err := o.graph.Extract(gctx, inputs)
		if err != nil {
			return fmt.Errorf("extracting symbols for %s@%s: %w", job.RepoID, job.TargetBranch, err)
		}
		out.extracted, out.graphStats = extracted, stats
		return nil
	})
	g.Go(func() error {
		inputs := make([]chunker.FileInput, len(files))
		for i, f := range files {
			inputs[i] = chunker.FileInput{Path: f.Path, Content: f.Content}
		}
		chunked, stats, err := o.chunker.ChunkFiles(gctx, inputs, o.budgeter)
		if err != nil {
			return fmt.Errorf("chunking files for %s@%s: %w", job.RepoID, job.TargetBranch, err)
		}
		out.chunkStats = stats
		prepared, embedStats, err := o.vectors.Prepare(gctx, job.RepoID, job.TargetBranch, chunked)
		if err != nil {
			return fmt.Errorf("embedding chunks for %s@%s: %w", job.RepoID, job.TargetBranch, err)
		}
		out.prepared, out.embedStats = prepared, embedStats
		return nil
	})
	if err := g.Wait(); err != nil {
		return computeResult{}, err
	}
	return out, nil
}

// writeSwap is the entire write phase, run inside the one transaction the
// transactor opened. Every statement below is issued sequentially on this
// goroutine: a pgx.Tx is not goroutine-safe, and nothing here starts a
// goroutine or makes a network call.
//
// The step order is the package doc comment's, and each step is its own
// named call so the order is asserted directly by this package's tests
// rather than inferred from a single fused write.
func (o *Orchestrator) writeSwap(ctx context.Context, tx pgx.Tx, job ingest.Job, plan diffplan.Plan, newRef string, c computeResult, ledgered []chunkstore.Rejection) (writeResult, error) {
	var out writeResult
	if err := o.applyDrops(ctx, tx, job, plan); err != nil {
		return out, err
	}
	graphStats, err := o.graph.Persist(ctx, tx, job.RepoID, job.TargetBranch, c.extracted)
	if err != nil {
		return out, fmt.Errorf("persisting symbols and graph edges for %s@%s: %w", job.RepoID, job.TargetBranch, err)
	}
	out.graphStats = graphStats
	// loam-c94.7's symbol_history write belongs exactly here: after the
	// symbols whose ids its rows carry a FK to have landed, and before the
	// chunk writes. It needs no other change to this function's shape.
	chunkStats, err := o.vectors.Persist(ctx, tx, job.RepoID, job.TargetBranch, c.prepared)
	if err != nil {
		return out, fmt.Errorf("persisting chunks for %s@%s: %w", job.RepoID, job.TargetBranch, err)
	}
	out.chunkStats = chunkStats
	if err := o.updateLedger(ctx, tx, job, plan, newRef, c, ledgered, chunkStats); err != nil {
		return out, err
	}
	versions, err := json.Marshal(o.versions)
	if err != nil {
		return out, fmt.Errorf("encoding ingested versions for %s@%s: %w", job.RepoID, job.TargetBranch, err)
	}
	if err := o.refs.AdvanceIngestedRef(ctx, tx, job.RepoID, job.TargetBranch, newRef, o.nowFunc(), versions); err != nil {
		return out, fmt.Errorf("recording ingested ref %s for %s@%s: %w", newRef, job.RepoID, job.TargetBranch, err)
	}
	return out, nil
}

// updateLedger reconciles the per-path rejection ledger with what this
// ingest just did, inside the swap's own transaction and BEFORE
// AdvanceIngestedRef (loam-qj21).
//
// The position is the whole point. The defect this ledger closes is that
// the ingested ref advances past a rejected file, after which no diff can
// ever name that path again. Writing the ledger in the same transaction,
// one statement before the ref moves, is what makes "the ref advanced past
// this path" and "this path is recorded as owed" a single atomic fact
// instead of two that can disagree.
//
// CLEARING, and the two ways a path earns it:
//
//   - A full rebuild empties the ledger wholesale, then re-records
//     whatever rejects during it. It has to be wholesale: KindFull exists
//     precisely for the cases with no usable diff, so the ledger may hold
//     paths the new tree does not contain at all and no per-path clear
//     keyed on what was attempted could name them. This is the same
//     argument that makes chunks' own full-rebuild drop repo-scoped.
//   - An incremental ingest clears a ledgered path only if it was
//     ATTEMPTED (or dropped) and did not reject again. "Attempted" is
//     c.attempted, not plan.ReparseFiles: contentReader.ReadFiles silently
//     skips a path that no longer resolves to a blob, and clearing a
//     ledger row for a file nothing actually re-chunked would delete the
//     only record that its chunks are missing. A path in plan.DropFiles is
//     cleared too -- the file was deleted or renamed away, so nothing is
//     owed for it any more.
func (o *Orchestrator) updateLedger(
	ctx context.Context,
	tx pgx.Tx,
	job ingest.Job,
	plan diffplan.Plan,
	newRef string,
	c computeResult,
	ledgered []chunkstore.Rejection,
	chunkStats vectors.Stats,
) error {
	if plan.Kind == ingest.KindFull {
		if err := o.ledger.ClearAll(ctx, tx, job.RepoID, job.TargetBranch); err != nil {
			return fmt.Errorf("clearing the rejection ledger for the full rebuild of %s@%s: %w", job.RepoID, job.TargetBranch, err)
		}
	} else if cleared := clearablePaths(ledgered, c.attempted, plan.DropFiles, chunkStats.Rejected); len(cleared) > 0 {
		if err := o.ledger.Clear(ctx, tx, job.RepoID, job.TargetBranch, cleared); err != nil {
			return fmt.Errorf("clearing %d resolved rejection(s) for %s@%s: %w", len(cleared), job.RepoID, job.TargetBranch, err)
		}
	}
	for _, r := range chunkStats.Rejected {
		if err := o.ledger.Record(ctx, tx, job.RepoID, job.TargetBranch, chunkstore.RejectionInput{
			File:        r.Path,
			ChunksState: r.ChunksState,
			SQLState:    r.SQLState,
			Error:       r.Err.Error(),
			JobID:       job.ID,
			RejectedRef: newRef,
		}); err != nil {
			return fmt.Errorf("recording the rejection of %s for %s@%s: %w", r.Path, job.RepoID, job.TargetBranch, err)
		}
	}
	return nil
}

// clearablePaths is the incremental clear set: every ledgered path this
// ingest either wrote successfully or dropped. Returned in the ledger's
// own order (path order, as ListChunkRejections sorts) so the statement is
// deterministic and a test can assert on it directly.
func clearablePaths(ledgered []chunkstore.Rejection, attempted, dropped []string, rejected []vectors.Rejection) []string {
	if len(ledgered) == 0 {
		return nil
	}
	resolved := make(map[string]struct{}, len(attempted)+len(dropped))
	for _, p := range attempted {
		resolved[p] = struct{}{}
	}
	for _, p := range dropped {
		resolved[p] = struct{}{}
	}
	for _, r := range rejected {
		delete(resolved, r.Path)
	}
	var out []string
	for _, l := range ledgered {
		if _, ok := resolved[l.File]; ok {
			out = append(out, l.File)
		}
	}
	return out
}

// warnIfExhausted names the ledgered paths that have spent their whole
// attempt budget (chunkstore.MaxRejectionAttempts) and are therefore no
// longer retried automatically. It runs once per ingest of the affected
// repo, at planning time, because that is the only recurring surface such
// a path has left: it is by definition not in the diff, so it produces no
// per-file log line of its own on the ingests that skip it, and the
// "ingest committed with rejected files" WARN below fires only for files
// rejected by THIS job.
//
// A recurring WARN is exactly right here and is not the "warning that is
// always present" anti-pattern: it fires only while an unresolved problem
// exists, it names the specific paths, and it stops the moment the ledger
// clears. See chunkstore.MaxRejectionAttempts for what an operator does
// about one.
func (o *Orchestrator) warnIfExhausted(ctx context.Context, repoName string, job ingest.Job, ledgered []chunkstore.Rejection) {
	var paths []string
	for _, l := range ledgered {
		if l.State == chunkstore.RejectionExhausted {
			paths = append(paths, l.File)
		}
	}
	if len(paths) == 0 {
		return
	}
	o.logger.WarnContext(ctx, "files have exhausted their chunk-write attempts and are no longer retried automatically; their chunks stay stale or absent until the file changes or a full rebuild runs",
		"repo", repoName, "target_branch", job.TargetBranch, "job_id", job.ID,
		"files", paths, "max_attempts", chunkstore.MaxRejectionAttempts)
}

// applyDrops runs step 1 of the write order. A full rebuild drops every
// derived row for the repo+branch; an incremental one drops only the
// plan's named paths. diffplan.Plan guarantees DropFiles is nil for a full
// plan (a full rebuild's drop is repo-scoped, not file-scoped), so these
// two branches are genuinely exclusive rather than merely usually so.
func (o *Orchestrator) applyDrops(ctx context.Context, tx pgx.Tx, job ingest.Job, plan diffplan.Plan) error {
	if plan.Kind == ingest.KindFull {
		if err := o.dropper.DropRepoBranch(ctx, tx, job.RepoID, job.TargetBranch); err != nil {
			return fmt.Errorf("dropping derived rows for full rebuild of %s@%s: %w", job.RepoID, job.TargetBranch, err)
		}
		return nil
	}
	if err := o.dropper.DropPaths(ctx, tx, job.RepoID, job.TargetBranch, plan.DropFiles); err != nil {
		return fmt.Errorf("dropping derived rows for %d removed path(s) of %s@%s: %w", len(plan.DropFiles), job.RepoID, job.TargetBranch, err)
	}
	return nil
}
