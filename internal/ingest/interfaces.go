// Package ingest implements the ingest_jobs queue and an in-process worker
// pool that runs ingest jobs serialized per repo (docs/ingestion-spec.md
// "Trigger & Scheduling"; docs/server-spec.md "Process Model" / "Startup"
// step 4). This package owns the queue, coalescing, claiming, and retry
// scheduling only -- the parse/chunk/embed/swap pipeline itself is injected
// as an Orchestrator (loam-c94.12) so the worker never runs pipeline logic.
package ingest

import (
	"context"

	"github.com/google/uuid"
)

//go:generate go tool moq -out moq_test.go . Enqueuer Orchestrator

// Kind is an ingest job's build strategy, matching ingest_jobs.kind's CHECK
// constraint (docs/persistence-spec.md "ingest_jobs").
type Kind string

const (
	// KindIncremental re-parses/re-embeds only the files a ref advance
	// touched (docs/ingestion-spec.md "Incremental Build").
	KindIncremental Kind = "incremental"
	// KindFull reparses/re-embeds the whole indexed tree
	// (docs/ingestion-spec.md "Incremental Build" -> "Full rebuild").
	KindFull Kind = "full"
)

// Job is one ingest_jobs row handed to the injected Orchestrator. It
// intentionally carries no old/new ref pair: the ingest_jobs table has no
// such columns (docs/persistence-spec.md / loam-54o.3's migration). The
// diff base is repo_target_branches.ingested_ref, and the new tip is the
// live mirror -- both are the Orchestrator/planner's (loam-c94.3,
// loam-c94.12) responsibility to resolve from RepoID+TargetBranch, not this
// package's or the Enqueuer caller's.
type Job struct {
	ID           uuid.UUID
	RepoID       uuid.UUID
	TargetBranch string
	Kind         Kind
	// Attempts is the number of prior attempts recorded for this job
	// before this claim (0 on a job's first run).
	Attempts int
}

// Stats is what a successful ingest reports back, persisted into
// ingest_jobs.stats (docs/ingestion-spec.md "Chunk -> Embed -> Vectors";
// bead DESCRIPTION: "Persist stats (files parsed, chunks embedded) on
// success").
type Stats struct {
	FilesParsed    int `json:"files_parsed"`
	ChunksEmbedded int `json:"chunks_embedded"`
}

// Enqueuer is the seam other components use to request an ingest job: the
// mirror-sync scheduler on an indexed-branch advance or first enrollment
// (loam-c94.2), and a manual admin reindex (loam-c94.15). Enqueue
// coalesces -- if a queued job already exists for (repoID, targetBranch,
// kind), this call is a no-op, so a trigger arriving while a repo's job
// runs (or while a matching job is already queued) yields exactly one
// follow-up queued job rather than stacking duplicates. Enqueue never
// takes a ref pair: this package's claim/coalesce keys are repo+branch+
// kind only (see Job's doc comment).
type Enqueuer interface {
	Enqueue(ctx context.Context, repoID uuid.UUID, targetBranch string, kind Kind) error
}

// Orchestrator runs one ingest job's parse -> chunk -> embed -> swap
// pipeline (loam-c94.12) inside its own transaction, resolving old/new
// refs itself from job.RepoID/job.TargetBranch, and reports the stats to
// persist on success. The worker pool calls this and only this to do the
// actual ingest work; it never runs parse/chunk logic itself.
type Orchestrator interface {
	Run(ctx context.Context, job Job) (Stats, error)
}
