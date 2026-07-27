package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultPollInterval is the fallback cadence a worker re-checks for
// queued jobs on, in case a wake-up signal (Enqueue, a finished job, a
// fired retry) is ever missed. Enqueue and job completion both wake
// idle workers directly, so this is a safety net, not the primary
// dispatch path.
const defaultPollInterval = 5 * time.Second

// defaultBackoffBase and defaultBackoffMax bound the exponential backoff
// between a failed job's attempts. docs/ingestion-spec.md's status line
// notes retry-policy tuning "firms up during implementation" -- these are
// this bead's chosen starting values, not a value settled elsewhere.
const (
	defaultBackoffBase = 1 * time.Second
	defaultBackoffMax  = 5 * time.Minute
)

// minBackoffBase is the floor WithBackoff clamps a non-positive base to.
// A zero or negative base means time.NewTimer fires immediately every
// time (see scheduleRetry), so a permanently-failing job becomes an
// unbounded hot loop against Postgres instead of a backoff; a negative
// base also runs backoffDelay's doubling backwards. It is small enough
// that a test harness driving features/ingestion.feature's "the job is
// retried" step can still use it without a real wall-clock wait.
const minBackoffBase = 1 * time.Millisecond

// minPollInterval is the floor WithPollInterval clamps a non-positive
// interval to. time.NewTicker panics on a non-positive duration ("PANIC:
// non-positive interval for NewTicker"), so WithPollInterval(0) --
// plausibly reached for by a caller who wants "never poll, rely purely
// on the wake channel" -- would crash Run the moment it started, rather
// than degrade into a hot loop the way a too-small backoff does. Unlike
// backoff, this ticker fires continuously for the pool's entire
// lifetime, not just around a failed job's retries, so the floor is set
// meaningfully higher than minBackoffBase's 1ms: 100ms is still fast
// enough for a godog step to observe a poll-driven claim well within a
// test's timeout, but does not become a busy-poll against Postgres if a
// caller passes 0 expecting the wake channel to carry everything.
const minPollInterval = 100 * time.Millisecond

// Pool is an in-process worker pool that claims queued ingest_jobs rows
// and runs them through an injected Orchestrator, serialized per repo: at
// most one job per repo_id runs at a time, regardless of pool size
// (docs/server-spec.md "Process Model" / "Configuration" ->
// LOAM_INGEST_WORKERS). It also implements Enqueuer, coalescing triggers
// into a single queued follow-up per (repo, branch, kind), and provides
// RequeueOrphaned for startup crash recovery. Construct with NewPool; wire
// workers via cmd/server/main.go.
type Pool struct {
	logger       *slog.Logger
	db           *pgxpool.Pool
	orchestrator Orchestrator
	workers      int
	pollInterval time.Duration
	backoffBase  time.Duration
	backoffMax   time.Duration
	wake         chan struct{}
	mu           sync.Mutex
	busy         map[uuid.UUID]struct{}
	drainWaiters map[uuid.UUID][]chan struct{}
	wg           sync.WaitGroup
}

// Option configures a Pool at construction, beyond the required
// constructor arguments. docs/testing-spec.md's "Manual scheduler"
// principle -- "the components already take their trigger as an input;
// tests just own it" -- otherwise has no seam here: the retry backoff and
// poll-interval timers are unexported fields with no other way for a test
// in a different package (e.g. a harness driving features/ingestion.
// feature's "the job is retried" step) to shrink them down from
// production-scale defaults.
type Option func(*Pool)

// WithBackoff overrides the default exponential backoff bounds
// (defaultBackoffBase, defaultBackoffMax) between a failed job's retry
// attempts. base below minBackoffBase is clamped up to it -- a
// zero-or-negative base is not "retry immediately with a tiny delay", it
// is an unbounded hot loop against Postgres for a permanently-failing job
// (minBackoffBase's doc comment), and a negative base makes backoffDelay's
// growth run backwards. max below base is clamped up to base, since a cap
// smaller than the base delay is not a cap at all -- it would make every
// attempt wait base regardless of what max says, silently ignoring the
// caller's max. This mirrors NewPool's existing convention of clamping
// workers < 1 to 1 rather than accepting a pool that cannot make progress.
func WithBackoff(base, max time.Duration) Option {
	if base < minBackoffBase {
		base = minBackoffBase
	}
	if max < base {
		max = base
	}
	return func(p *Pool) {
		p.backoffBase = base
		p.backoffMax = max
	}
}

// WithPollInterval overrides the default fallback poll cadence
// (defaultPollInterval's doc comment: a safety net behind the wake-up
// channel, not the primary dispatch path). d below minPollInterval is
// clamped up to it -- see minPollInterval's doc comment for why zero or
// negative must not reach time.NewTicker directly.
func WithPollInterval(d time.Duration) Option {
	if d < minPollInterval {
		d = minPollInterval
	}
	return func(p *Pool) {
		p.pollInterval = d
	}
}

// NewPool builds a Pool. workers is LOAM_INGEST_WORKERS (a server-wide
// cross-repo parallelism cap, read by internal/config); values below 1 are
// clamped to 1 so the pool always makes progress. logger and db must be
// non-nil; orchestrator is the injected pipeline (loam-c94.12). opts
// applies after the defaults, so WithBackoff/WithPollInterval override
// them.
func NewPool(logger *slog.Logger, db *pgxpool.Pool, orchestrator Orchestrator, workers int, opts ...Option) *Pool {
	if workers < 1 {
		workers = 1
	}
	p := &Pool{
		logger:       logger,
		db:           db,
		orchestrator: orchestrator,
		workers:      workers,
		pollInterval: defaultPollInterval,
		backoffBase:  defaultBackoffBase,
		backoffMax:   defaultBackoffMax,
		wake:         make(chan struct{}, 1),
		busy:         make(map[uuid.UUID]struct{}),
		drainWaiters: make(map[uuid.UUID][]chan struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Run starts workers worker goroutines and blocks until ctx is canceled
// and every in-flight job (and any pending retry timer) has drained. Call
// RequeueOrphaned before Run, never while jobs could already be claimed
// (docs/server-spec.md "Startup" step 4 / step 5 ordering).
func (p *Pool) Run(ctx context.Context) {
	for range p.workers {
		p.wg.Add(1)
		go p.work(ctx)
	}
	p.wg.Wait()
}

// work is one worker goroutine's loop: claim a job if one is available and
// not blocked by per-repo serialization, run it, and otherwise wait for a
// wake-up signal, the poll interval, or shutdown.
func (p *Pool) work(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()
	for {
		job, claimed, err := p.claim(ctx)
		if err != nil {
			p.logger.ErrorContext(ctx, "claiming ingest job", "error", err)
		}
		if claimed {
			p.run(ctx, job)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

// claimQuery selects the oldest queued job whose repo is not already
// running in this process, locking the row FOR UPDATE SKIP LOCKED so
// concurrent claimers (in this process or, defensively, another) never
// double-claim the same row.
const claimQuery = `
SELECT id, repo_id, target_branch, kind, attempts
FROM ingest_jobs
WHERE status = 'queued' AND NOT (repo_id = ANY($1::uuid[]))
ORDER BY queued_at
LIMIT 1
FOR UPDATE SKIP LOCKED
`

// claim attempts to claim one queued job. It holds mu for the whole
// operation -- snapshotting the busy-repo set, querying, marking the row
// running, and recording the repo as busy -- so two goroutines in this
// process can never observe the same repo as free and both claim a job
// for it (the property docs/server-spec.md's per-repo serialization
// depends on, since the DB row lock alone only prevents the same *row*
// from being claimed twice, not two different queued rows for the same
// repo). The DB round trip happens while holding mu; claiming is cheap
// relative to running a job, so this does not become a bottleneck.
func (p *Pool) claim(ctx context.Context) (Job, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	busy := make([]uuid.UUID, 0, len(p.busy))
	for id := range p.busy {
		busy = append(busy, id)
	}
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return Job{}, false, fmt.Errorf("beginning claim transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var job Job
	err = tx.QueryRow(ctx, claimQuery, busy).Scan(&job.ID, &job.RepoID, &job.TargetBranch, &job.Kind, &job.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("selecting a queued ingest job: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE ingest_jobs SET status = 'running', started_at = now() WHERE id = $1`, job.ID); err != nil {
		return Job{}, false, fmt.Errorf("marking ingest job %s running: %w", job.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, false, fmt.Errorf("committing ingest job claim: %w", err)
	}
	p.busy[job.RepoID] = struct{}{}
	return job, true, nil
}

// run executes job through the injected Orchestrator and records the
// outcome. It always releases the repo's per-repo serialization slot
// before returning, whether the job succeeded or failed, so a coalesced
// follow-up (or any other queued job for the same repo) becomes claimable
// immediately.
func (p *Pool) run(ctx context.Context, job Job) {
	stats, err := p.orchestrator.Run(ctx, job)
	if err != nil {
		p.fail(ctx, job, err)
		return
	}
	p.succeed(ctx, job, stats)
}

// succeed records a successful ingest: status=succeeded, stats persisted
// as jsonb, finished_at set (docs/ingestion-spec.md "Chunk -> Embed ->
// Vectors").
func (p *Pool) succeed(ctx context.Context, job Job, stats Stats) {
	defer p.release(job.RepoID)
	defer p.notifyDrainWaiters(job.RepoID)
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		p.logger.ErrorContext(ctx, "marshaling ingest job stats", "job_id", job.ID, "error", err)
		statsJSON = []byte(`{}`)
	}
	if _, err := p.db.Exec(ctx,
		`UPDATE ingest_jobs SET status = 'succeeded', stats = $2, finished_at = now() WHERE id = $1`,
		job.ID, statsJSON,
	); err != nil {
		p.logger.ErrorContext(ctx, "recording ingest job success", "job_id", job.ID, "error", err)
	}
}

// fail records a failed ingest -- status=failed, the error, attempts
// incremented, finished_at set -- then schedules a retry after bounded
// exponential backoff (bead DESCRIPTION: "On failure mark status=failed
// with the error recorded ... retry with exponential backoff (increment
// attempts)"). The repo's serialization slot is released immediately (not
// held for the backoff wait), so a coalesced follow-up for the same repo
// can run right away rather than waiting behind this job's retry timer.
func (p *Pool) fail(ctx context.Context, job Job, runErr error) {
	defer p.release(job.RepoID)
	defer p.notifyDrainWaiters(job.RepoID)
	var attempts int
	err := p.db.QueryRow(ctx,
		`UPDATE ingest_jobs SET status = 'failed', error = $2, attempts = attempts + 1, finished_at = now() WHERE id = $1 RETURNING attempts`,
		job.ID, runErr.Error(),
	).Scan(&attempts)
	if err != nil {
		p.logger.ErrorContext(ctx, "recording ingest job failure", "job_id", job.ID, "run_error", runErr, "error", err)
		return
	}
	p.logger.ErrorContext(ctx, "ingest job failed", "job_id", job.ID, "repo_id", job.RepoID, "attempts", attempts, "error", runErr)
	delay := backoffDelay(attempts, p.backoffBase, p.backoffMax)
	p.wg.Add(1)
	go p.scheduleRetry(ctx, job.ID, delay)
}

// scheduleRetry waits delay, then flips job jobID back to queued so a
// worker can claim it again. If ctx is canceled first (server shutdown),
// it exits without touching the row: the row stays failed, and the next
// trigger for that repo+branch+kind (e.g. the following sync tick) simply
// enqueues a fresh job via Enqueue, since Enqueue's coalescing only
// considers status='queued' rows.
func (p *Pool) scheduleRetry(ctx context.Context, jobID uuid.UUID, delay time.Duration) {
	defer p.wg.Done()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	if _, err := p.db.Exec(ctx, `UPDATE ingest_jobs SET status = 'queued', queued_at = now() WHERE id = $1 AND status = 'failed'`, jobID); err != nil {
		p.logger.ErrorContext(ctx, "requeueing failed ingest job for retry", "job_id", jobID, "error", err)
		return
	}
	p.wakeUp()
}

// backoffDelay doubles base once per prior attempt (attempts is the
// post-increment count, so the first failure yields base itself), capped
// at max.
func backoffDelay(attempts int, base, max time.Duration) time.Duration {
	delay := base
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	if delay > max {
		return max
	}
	return delay
}

// release frees repoID's per-repo serialization slot and wakes idle
// workers so a queued follow-up can be claimed without waiting for the
// poll interval.
func (p *Pool) release(repoID uuid.UUID) {
	p.mu.Lock()
	delete(p.busy, repoID)
	p.mu.Unlock()
	p.wakeUp()
}

// wakeUp signals an idle worker to re-check for claimable jobs, without
// blocking if one is already pending.
func (p *Pool) wakeUp() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// enqueueLockKey returns the pg_advisory_xact_lock key Enqueue serializes
// on for a given (repoID, targetBranch, kind) triple. Exposed as its own
// function (rather than inlined) so a test can take the identical lock
// Enqueue contends on, instead of duplicating the format string and
// risking silent drift between the two.
func enqueueLockKey(repoID uuid.UUID, targetBranch string, kind Kind) string {
	return fmt.Sprintf("ingest-enqueue:%s:%s:%s", repoID, targetBranch, kind)
}

// Enqueue implements Enqueuer. It coalesces on (repoID, targetBranch,
// kind): a pg_advisory_xact_lock keyed on that triple serializes
// concurrent callers so a burst of triggers for the same repo commits at
// most one new queued row, whether or not a job for that repo is
// currently running.
//
// Deliberately not coalesced: a trigger arriving while a same-key job is
// in status 'failed' (mid-backoff, see scheduleRetry) inserts a new
// queued row alongside the eventual retry, rather than being absorbed by
// it -- the dedup predicate below only matches status='queued'.
// docs/ingestion-spec.md's coalescing clause ("Trigger & Scheduling")
// scopes to "a new trigger while one runs", not one that is failed and
// waiting to retry, so this is not a spec violation, just one wasted
// (idempotent) unit of work in an uncommon window. Do NOT widen the
// predicate to include 'failed': that would suppress a legitimate
// re-trigger arriving during the backoff wait, and the per-repo claim
// filter already serializes the duplicate against the retry so they
// never run concurrently (mutation-tested in
// TestPool_PerRepoSerializationHoldsUnderConcurrentClaim's mutation, see
// this bead's final report).
func (p *Pool) Enqueue(ctx context.Context, repoID uuid.UUID, targetBranch string, kind Kind) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning enqueue transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockKey := enqueueLockKey(repoID, targetBranch, kind)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return fmt.Errorf("acquiring ingest enqueue lock: %w", err)
	}
	var existing uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT id FROM ingest_jobs WHERE repo_id = $1 AND target_branch = $2 AND kind = $3 AND status = 'queued' LIMIT 1`,
		repoID, targetBranch, kind,
	).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("checking for an existing queued ingest job: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generating ingest job id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ingest_jobs (id, repo_id, target_branch, kind, status, attempts, queued_at) VALUES ($1, $2, $3, $4, 'queued', 0, now())`,
		id, repoID, targetBranch, kind,
	); err != nil {
		return fmt.Errorf("inserting queued ingest job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing ingest job enqueue: %w", err)
	}
	p.wakeUp()
	return nil
}

// RequeueOrphaned resets every ingest_jobs row stuck in status=running --
// left behind by a prior crash -- back to queued, leaving attempts
// unchanged: ingest is transactional, so a crashed job committed no
// partial index and this is not a failed attempt (docs/server-spec.md
// "Startup" step 4). Call this once at startup before Run, never while
// jobs could already be claimed.
func (p *Pool) RequeueOrphaned(ctx context.Context) error {
	tag, err := p.db.Exec(ctx, `UPDATE ingest_jobs SET status = 'queued' WHERE status = 'running'`)
	if err != nil {
		return fmt.Errorf("requeueing orphaned ingest jobs: %w", err)
	}
	p.logger.InfoContext(ctx, "requeued orphaned ingest jobs", "count", tag.RowsAffected())
	return nil
}

// DrainRepoID blocks until repoID has zero ingest_jobs rows in status
// 'queued' or 'running' -- including any coalesced follow-up job, not
// merely whichever job is running right now, since a follow-up queued
// while the current job runs keeps the repo un-drained. It exists for
// test harnesses that need a deterministic, non-polling way to wait for a
// repo's ingest activity to settle (docs/testing-spec.md "Manual
// scheduler": no time.Sleep, no Eventually) -- the same shape
// internal/mirrorsync's own harness gets by decorating RepoLister /
// SyncStateReporter, which this package has no equivalent observable seam
// for, so this method is that seam.
//
// The check queries real Postgres state (not this Pool's own in-memory
// bookkeeping), so it is correct even for a row this Pool never itself
// enqueued or claimed -- e.g. one seeded directly by a test, or one
// RequeueOrphaned just reset. The check and the wait registration happen
// under the same lock a completing job's notification also takes, so a
// job finishing between this call's check and its wait can never be
// missed (the classic check-then-wait lost-wakeup race) -- but the
// *notification* is this Pool instance's own in-process signal, not a
// Postgres one: if some other process (another server replica, a manual
// UPDATE) is the one that finishes the job, this call's waiter is never
// woken and blocks until ctx's deadline. That is fine for the
// single-replica MVP this package targets (docs/server-spec.md "Process
// Model"), but is not a general cross-process primitive.
//
// Semantics note for callers: this is a literal "zero queued-or-running"
// check. A job that just failed and is waiting out its retry backoff is,
// for that window, in status 'failed' -- neither queued nor running -- so
// DrainRepoID can return while a retry is about to re-queue it. Callers
// that need "settled including all retries" need a different contract;
// this one intentionally matches only what was asked.
func (p *Pool) DrainRepoID(ctx context.Context, repoID uuid.UUID) error {
	for {
		wait, drained, err := p.checkOrRegisterDrainWaiter(ctx, repoID)
		if err != nil {
			return err
		}
		if drained {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
	}
}

// DrainRepo is DrainRepoID keyed by repo name instead of id: it resolves
// repo (repos.name, the `<group>/<repo_name>` form -- docs/persistence-
// spec.md "repos") to its id via the unique, indexed repos_name_key
// lookup, then delegates. This is the harness-facing seam: production
// callers of Enqueue already hold repos.id (loam-c94.2's adapter keys
// repo_target_branches by (repo_id, branch); loam-c94.14/loam-c94.15
// receive a repo name off the proto surface and must resolve it to an id
// regardless of this package, so resolving here too would just be a
// second lookup, not a saved one) -- but a test harness driving the
// system by name only ever holds the name, never the id, which is why
// this asymmetry with Enqueue/Job/DrainRepoID (all uuid.UUID) is
// deliberate rather than an inconsistency to "fix" by unifying types.
//
// This package takes repo as a plain string, not mirrorsync.RepoID:
// internal/ingest does not import internal/mirrorsync. A caller keyed by
// mirrorsync.RepoID needs a one-line string(repo) conversion (RepoID's
// underlying type is string) to call this method, or its own adapter
// implementing whatever consumer interface it declares.
func (p *Pool) DrainRepo(ctx context.Context, repo string) error {
	var repoID uuid.UUID
	if err := p.db.QueryRow(ctx, `SELECT id FROM repos WHERE name = $1`, repo).Scan(&repoID); err != nil {
		return fmt.Errorf("resolving repo %q to an id: %w", repo, err)
	}
	return p.DrainRepoID(ctx, repoID)
}

// checkOrRegisterDrainWaiter queries whether repoID currently has any
// queued-or-running ingest_jobs row and, if it does, registers a waiter
// channel for it -- atomically under mu, so a notifyDrainWaiters call
// from a concurrently finishing job can never land in the gap between the
// query and the registration.
func (p *Pool) checkOrRegisterDrainWaiter(ctx context.Context, repoID uuid.UUID) (<-chan struct{}, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var count int
	err := p.db.QueryRow(ctx,
		`SELECT count(*) FROM ingest_jobs WHERE repo_id = $1 AND status IN ('queued', 'running')`, repoID,
	).Scan(&count)
	if err != nil {
		return nil, false, fmt.Errorf("checking whether repo %s has drained: %w", repoID, err)
	}
	if count == 0 {
		return nil, true, nil
	}
	ch := make(chan struct{})
	p.drainWaiters[repoID] = append(p.drainWaiters[repoID], ch)
	return ch, false, nil
}

// notifyDrainWaiters wakes every DrainRepo call currently blocked on
// repoID so each re-checks real DB state; it is called after every
// transition that can reduce repoID's queued-or-running count (succeed,
// fail), never after a transition that only holds or increases it, so a
// waiter is never woken to do a check that could not have changed.
func (p *Pool) notifyDrainWaiters(repoID uuid.UUID) {
	p.mu.Lock()
	waiters := p.drainWaiters[repoID]
	delete(p.drainWaiters, repoID)
	p.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}
