package ingestjobs

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Kind is an ingest_jobs.kind value, matching the column's CHECK
// constraint (ingest_jobs_kind_check, 0001_init.up.sql;
// docs/persistence-spec.md "ingest_jobs").
type Kind string

// The two values ingest_jobs.kind's CHECK constraint allows
// (docs/ingestion-spec.md "Incremental Build").
const (
	// KindIncremental re-parses/re-embeds only the files a ref advance
	// touched.
	KindIncremental Kind = "incremental"
	// KindFull re-parses/re-embeds the whole indexed tree.
	KindFull Kind = "full"
)

// Status is an ingest_jobs.status value, matching the column's CHECK
// constraint (ingest_jobs_status_check, 0001_init.up.sql).
type Status string

// The four values ingest_jobs.status's CHECK constraint allows
// (docs/ingestion-spec.md "Trigger & Scheduling": "queued -> running ->
// succeeded | failed"). A failed job with attempts still under the
// worker's retry ceiling returns to StatusQueued via Store.Requeue rather
// than gaining a fifth status of its own.
const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// Job is one ingest_jobs row (docs/persistence-spec.md "ingest_jobs"). It
// carries no old_ref/new_ref pair: the settled schema has no such columns
// (this bead's NOTES "SPEC CORRECTION") -- the diff base is
// repo_target_branches.ingested_ref and the new tip is the live mirror,
// both resolved by whatever orchestrates the pipeline from RepoID +
// TargetBranch, not by this store.
type Job struct {
	ID           uuid.UUID
	RepoID       uuid.UUID
	TargetBranch string
	Kind         Kind
	Status       Status
	// Attempts counts completed FAILED attempts (incremented by Fail, not
	// by Claim or Enqueue): a job that succeeds on its first claim never
	// touches this field, so it stays 0 through a clean run and only
	// climbs across retries.
	Attempts int32
	// Error is the last failure's detail (docs/persistence-spec.md
	// "ingest_jobs": "error (null)"), nil until Fail records one.
	Error *string
	// Stats is the success payload Complete persists -- files parsed,
	// chunks embedded (docs/ingestion-spec.md "Chunk -> Embed ->
	// Vectors") -- nil until a job succeeds. This package does not
	// interpret its contents; that is the caller's shape to define.
	Stats      json.RawMessage
	QueuedAt   time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// EnqueueParams is the input to Store.Enqueue.
type EnqueueParams struct {
	RepoID       uuid.UUID
	TargetBranch string
	Kind         Kind
}

// ListFilter narrows Store.List. RepoID nil and Status "" both mean "no
// filter on this column" -- neither repo_id nor status is ever
// legitimately absent/empty on a real row, so both are safe "unset"
// sentinels, matching internal/workbranchstore.ListFilter's convention.
type ListFilter struct {
	RepoID *uuid.UUID
	Status Status
}

// ListResult is one page of Store.List, plus the total matching count for
// PageInfo.total (docs/persistence-spec.md "Conventions"; the store-layer
// backing for RepoAdminService.ListIngestJobs, docs/web-spec.md).
type ListResult struct {
	Jobs  []Job
	Total int64
}
