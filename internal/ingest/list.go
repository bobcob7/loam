package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// JobRecord is one ingest_jobs row joined with its repo's name, for
// RepoAdminService.ListIngestJobs (loam-ofg.12): the admin Jobs view
// reports jobs keyed by repo name (RepoID is repos.name, not repos.id --
// loam-54o.7 NOTES), not the bare FK id ingest_jobs itself stores, and
// needs the full set of columns docs/web-spec.md's IngestJob message
// carries (kind/status/attempts/error/queued_at/started_at/finished_at).
type JobRecord struct {
	ID           uuid.UUID
	Repo         string
	TargetBranch string
	Kind         Kind
	Status       string
	Attempts     int
	Error        string
	QueuedAt     time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// ListJobsFilter narrows ListJobs. Repo empty means no filter on repo
// name; Status empty means no filter on status -- neither column is ever
// legitimately empty, so empty is a safe "unset" sentinel, matching this
// codebase's other list filters (e.g. workbranchstore.ListFilter).
type ListJobsFilter struct {
	Repo   string
	Status string
}

// ListJobs returns one page of ingest_jobs rows (newest queued_at first),
// joined with repos for the name RepoAdminService.ListIngestJobs reports,
// plus the total matching count for PageInfo.total
// (docs/web-spec.md -> RepoAdminService "ListIngestJobs"). Like the rest
// of this package, this queries ingest_jobs directly by hand-written SQL
// rather than through sqlc -- Pool already owns every other read/write
// against this table (claim, succeed, fail, Enqueue, RequeueOrphaned), so
// a sqlc-generated read path here would be a second, inconsistent way of
// touching the same rows.
func (p *Pool) ListJobs(ctx context.Context, filter ListJobsFilter, limit, offset int32) ([]JobRecord, int64, error) {
	if limit <= 0 {
		limit = defaultListJobsLimit
	}
	var total int64
	if err := p.db.QueryRow(ctx,
		`SELECT count(*) FROM ingest_jobs j JOIN repos r ON r.id = j.repo_id
		 WHERE ($1 = '' OR r.name = $1) AND ($2 = '' OR j.status = $2)`,
		filter.Repo, filter.Status,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting ingest jobs: %w", err)
	}
	rows, err := p.db.Query(ctx,
		`SELECT j.id, r.name, j.target_branch, j.kind, j.status, j.attempts, coalesce(j.error, ''), j.queued_at, j.started_at, j.finished_at
		 FROM ingest_jobs j JOIN repos r ON r.id = j.repo_id
		 WHERE ($1 = '' OR r.name = $1) AND ($2 = '' OR j.status = $2)
		 ORDER BY j.queued_at DESC
		 LIMIT $3 OFFSET $4`,
		filter.Repo, filter.Status, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("listing ingest jobs: %w", err)
	}
	defer rows.Close()
	var jobs []JobRecord
	for rows.Next() {
		var (
			job        JobRecord
			kind       string
			startedAt  pgtype.Timestamptz
			finishedAt pgtype.Timestamptz
		)
		if err := rows.Scan(&job.ID, &job.Repo, &job.TargetBranch, &kind, &job.Status, &job.Attempts, &job.Error, &job.QueuedAt, &startedAt, &finishedAt); err != nil {
			return nil, 0, fmt.Errorf("scanning ingest job row: %w", err)
		}
		job.Kind = Kind(kind)
		if startedAt.Valid {
			job.StartedAt = &startedAt.Time
		}
		if finishedAt.Valid {
			job.FinishedAt = &finishedAt.Time
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating ingest job rows: %w", err)
	}
	return jobs, total, nil
}

// defaultListJobsLimit is the page size ListJobs uses when the caller's
// limit is non-positive, matching every other list method's "0 means use
// the server default" contract in this codebase.
const defaultListJobsLimit = 50
