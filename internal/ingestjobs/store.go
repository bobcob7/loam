package ingestjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bobcob7/loam/internal/db/gen"
)

// defaultListLimit is the page size Store.List uses when the caller's
// limit is non-positive, matching this codebase's other list methods'
// "0 means use the server default" contract (e.g.
// reposstore.defaultListLimit).
const defaultListLimit = 50

// Store is the ingest_jobs store (package doc comment). Construct with
// NewStore, passing a *pgxpool.Pool in production (it satisfies querier
// directly) or a querier mock in tests.
type Store struct {
	q      *gen.Queries
	logger *slog.Logger
}

// NewStore builds a Store over db, typically a *pgxpool.Pool.
func NewStore(db querier, logger *slog.Logger) *Store {
	return &Store{q: gen.New(db), logger: logger}
}

// Enqueue inserts a new ingest_jobs row with a fresh UUIDv7 id, status
// 'queued', and attempts 0 (docs/persistence-spec.md "ingest_jobs";
// this bead's DESCRIPTION). It performs no coalescing/deduplication
// against an already-queued job for the same repo/branch/kind -- that is
// the ingest worker's concern (epic loam-c94; see this package's doc
// comment and the bead's DESIGN note), not this store's.
func (s *Store) Enqueue(ctx context.Context, params EnqueueParams) (Job, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Job{}, fmt.Errorf("generating ingest job id: %w", err)
	}
	row, err := s.q.EnqueueIngestJob(ctx, gen.EnqueueIngestJobParams{
		ID:           pgUUID(id),
		RepoID:       pgUUID(params.RepoID),
		TargetBranch: params.TargetBranch,
		Kind:         string(params.Kind),
	})
	if err != nil {
		return Job{}, fmt.Errorf("enqueueing ingest job for repo %s: %w", params.RepoID, err)
	}
	job := fromGenIngestJob(row)
	s.logger.InfoContext(ctx, "enqueued ingest job", "job_id", job.ID, "repo_id", job.RepoID, "target_branch", job.TargetBranch, "kind", job.Kind)
	return job, nil
}

// Get returns the ingest job identified by id, or a wrapped errNotFound if
// none exists.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Job, error) {
	row, err := s.q.GetIngestJob(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Job{}, fmt.Errorf("getting ingest job %s: %w", id, errNotFound)
		}
		return Job{}, fmt.Errorf("getting ingest job %s: %w", id, err)
	}
	return fromGenIngestJob(row), nil
}

// maxClaimAttempts bounds how many times Claim retries ClaimIngestJob
// after losing a race against ingest_jobs_one_running_per_repo (the
// partial unique index that is the actual source of the
// single-running-per-repo guarantee -- see ClaimIngestJob's doc comment,
// internal/db/queries/ingest_jobs.sql, for why the statement itself
// cannot establish that guarantee alone). Each retry is a fresh,
// independent statement against Postgres with negligible cost, and the
// condition it retries on is self-resolving (a fresh attempt's snapshot
// sees whatever committed claim it previously missed and picks a
// different candidate), so this bound exists only to keep a pathological
// burst of contention from looping forever, not because a well-behaved
// caller is expected to need more than one or two attempts in practice.
const maxClaimAttempts = 25

// Claim atomically picks the oldest queued job belonging to a repo with no
// currently running job, and flips it to running with started_at set --
// the single-running-per-repo guard the bead's DESIGN calls for. The
// guarantee itself comes from ingest_jobs_one_running_per_repo (migration
// 0008_ingest_jobs_running_guard), a database constraint, not from
// ClaimIngestJob's own locking (internal/db/queries/ingest_jobs.sql; see
// its doc comment for why one SQL statement under READ COMMITTED cannot
// establish this alone, proven by this package's own concurrency
// integration tests under repetition). Claim retries, up to
// maxClaimAttempts times, whenever an attempt loses a race against that
// constraint -- an ordinary "someone beat me to it" outcome, not a
// failure -- so a caller sees this internal contention only as slightly
// higher latency, never as a spurious error.
//
// Returns a wrapped errNoJobAvailable when nothing is claimable right
// now, which is Claim's ordinary "nothing to do" outcome, not a failure a
// caller should log as one.
func (s *Store) Claim(ctx context.Context) (Job, error) {
	for attempt := 0; attempt < maxClaimAttempts; attempt++ {
		row, err := s.q.ClaimIngestJob(ctx)
		if err == nil {
			job := fromGenIngestJob(row)
			s.logger.InfoContext(ctx, "claimed ingest job", "job_id", job.ID, "repo_id", job.RepoID, "target_branch", job.TargetBranch, "kind", job.Kind, "attempts", job.Attempts, "claim_attempt", attempt+1)
			return job, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return Job{}, fmt.Errorf("claiming an ingest job: %w", errNoJobAvailable)
		}
		if isRunningPerRepoViolation(err) {
			s.logger.DebugContext(ctx, "claim lost a race against the single-running-per-repo constraint, retrying", "claim_attempt", attempt+1)
			continue
		}
		return Job{}, fmt.Errorf("claiming an ingest job: %w", err)
	}
	return Job{}, fmt.Errorf("claiming an ingest job: exhausted %d attempts against %s: %w", maxClaimAttempts, runningPerRepoConstraint, errNoJobAvailable)
}

// Complete records a successful ingest for id: status succeeded, stats
// persisted as jsonb, finished_at set (docs/ingestion-spec.md
// "Consistency & Failure"). stats may be nil, written as SQL NULL. Only
// legal from status running; any other current status returns a wrapped
// errIllegalTransition, and a nonexistent id returns a wrapped
// errNotFound.
func (s *Store) Complete(ctx context.Context, id uuid.UUID, stats []byte) (Job, error) {
	row, err := s.q.CompleteIngestJob(ctx, gen.CompleteIngestJobParams{ID: pgUUID(id), Stats: stats})
	if err != nil {
		return Job{}, s.transitionErr(ctx, id, err, "completing")
	}
	job := fromGenIngestJob(row)
	s.logger.InfoContext(ctx, "completed ingest job", "job_id", job.ID, "repo_id", job.RepoID)
	return job, nil
}

// Fail records a failed ingest for id: status failed, error recorded,
// attempts incremented, finished_at set (docs/ingestion-spec.md
// "Consistency & Failure": "the job is marked failed with the error
// recorded"). Only legal from status running; any other current status
// returns a wrapped errIllegalTransition, and a nonexistent id returns a
// wrapped errNotFound.
func (s *Store) Fail(ctx context.Context, id uuid.UUID, cause string) (Job, error) {
	row, err := s.q.FailIngestJob(ctx, gen.FailIngestJobParams{ID: pgUUID(id), Error: pgText(&cause)})
	if err != nil {
		return Job{}, s.transitionErr(ctx, id, err, "failing")
	}
	job := fromGenIngestJob(row)
	s.logger.InfoContext(ctx, "failed ingest job", "job_id", job.ID, "repo_id", job.RepoID, "attempts", job.Attempts, "error", cause)
	return job, nil
}

// Requeue returns a failed job to queued for another Claim attempt --
// the retry path a worker's backoff timer drives (docs/ingestion-spec.md
// "Trigger & Scheduling": "Failures retry with backoff"). queued_at is
// re-stamped to now, so the job re-enters Claim's oldest-first ordering
// as of the retry. Only legal from status failed; any other current
// status returns a wrapped errIllegalTransition, and a nonexistent id
// returns a wrapped errNotFound.
func (s *Store) Requeue(ctx context.Context, id uuid.UUID) (Job, error) {
	row, err := s.q.RequeueIngestJob(ctx, pgUUID(id))
	if err != nil {
		return Job{}, s.transitionErr(ctx, id, err, "requeuing")
	}
	job := fromGenIngestJob(row)
	s.logger.InfoContext(ctx, "requeued ingest job", "job_id", job.ID, "repo_id", job.RepoID)
	return job, nil
}

// List returns filter's matching ingest jobs, newest-queued first, with
// LIMIT/OFFSET pagination, plus the total count matching the same filter,
// unpaginated -- for PageInfo.total (docs/persistence-spec.md
// "Conventions"). A non-positive limit is replaced with defaultListLimit.
func (s *Store) List(ctx context.Context, filter ListFilter, limit, offset int32) (ListResult, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	repoID, status := filterColumns(filter)
	rows, err := s.q.ListIngestJobs(ctx, gen.ListIngestJobsParams{Column1: repoID, Column2: status, Limit: limit, Offset: offset})
	if err != nil {
		return ListResult{}, fmt.Errorf("listing ingest jobs: %w", err)
	}
	total, err := s.q.CountIngestJobs(ctx, gen.CountIngestJobsParams{Column1: repoID, Column2: status})
	if err != nil {
		return ListResult{}, fmt.Errorf("counting ingest jobs: %w", err)
	}
	jobs := make([]Job, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, fromGenIngestJob(row))
	}
	return ListResult{Jobs: jobs, Total: total}, nil
}

// filterColumns converts a ListFilter to the two positional filter columns
// ListIngestJobs/CountIngestJobs share: repoID is left Valid: false (SQL
// NULL, "no filter") when filter.RepoID is nil, and status is passed
// through as-is (empty means "no filter" at the SQL layer,
// internal/db/queries/ingest_jobs.sql).
func filterColumns(filter ListFilter) (repoID pgtype.UUID, status string) {
	if filter.RepoID != nil {
		repoID = pgUUID(*filter.RepoID)
	}
	return repoID, string(filter.Status)
}

// transitionErr classifies a guarded transition's failure (Complete, Fail,
// Requeue): a wrapped errNotFound if id names no row at all, a wrapped
// errIllegalTransition if the row exists but its current status
// disqualified the guarded UPDATE (zero rows matched, not a transport
// failure), or the raw error wrapped with context for anything else.
// verb names the attempted action for the error message. This mirrors
// internal/workbranchstore.Store.transitionErr's classification exactly.
func (s *Store) transitionErr(ctx context.Context, id uuid.UUID, err error, verb string) error {
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s ingest job %s: %w", verb, id, err)
	}
	_, getErr := s.q.GetIngestJob(ctx, pgUUID(id))
	if getErr == nil {
		return fmt.Errorf("%s ingest job %s: %w", verb, id, errIllegalTransition)
	}
	if errors.Is(getErr, pgx.ErrNoRows) {
		return fmt.Errorf("%s ingest job %s: %w", verb, id, errNotFound)
	}
	return fmt.Errorf("%s ingest job %s: classifying failed transition: %w", verb, id, getErr)
}
