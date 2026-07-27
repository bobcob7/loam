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
// code path panics deliberately: internal/ingest.Pool runs each job on its
// own goroutine with no recover(), so a panic in this function takes down
// the whole server process rather than failing one job (loam-337, open and
// deliberately not fixed here).
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
	o.logger.InfoContext(ctx, "planned ingest",
		"repo", repo.Name, "target_branch", job.TargetBranch, "job_id", job.ID,
		"kind", plan.Kind, "escalation_reason", plan.Reason,
		"drop_files", len(plan.DropFiles), "reparse_files", len(plan.ReparseFiles))
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
		written, err = o.writeSwap(ctx, tx, job, plan, newRef, work)
		return err
	}); err != nil {
		return ingest.Stats{}, err
	}
	stats := ingest.Stats{FilesParsed: work.graphStats.FilesExtracted, ChunksEmbedded: written.chunkStats.ChunksWritten}
	o.logger.InfoContext(ctx, "ingest committed",
		"repo", repo.Name, "target_branch", job.TargetBranch, "job_id", job.ID, "kind", plan.Kind,
		"ingested_ref", newRef, "files_parsed", stats.FilesParsed, "chunks_embedded", stats.ChunksEmbedded,
		"edges_recomputed", written.graphStats.EdgesRecomputed)
	return stats, nil
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
func (o *Orchestrator) writeSwap(ctx context.Context, tx pgx.Tx, job ingest.Job, plan diffplan.Plan, newRef string, c computeResult) (writeResult, error) {
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
	versions, err := json.Marshal(o.versions)
	if err != nil {
		return out, fmt.Errorf("encoding ingested versions for %s@%s: %w", job.RepoID, job.TargetBranch, err)
	}
	if err := o.refs.AdvanceIngestedRef(ctx, tx, job.RepoID, job.TargetBranch, newRef, o.nowFunc(), versions); err != nil {
		return out, fmt.Errorf("recording ingested ref %s for %s@%s: %w", newRef, job.RepoID, job.TargetBranch, err)
	}
	return out, nil
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
