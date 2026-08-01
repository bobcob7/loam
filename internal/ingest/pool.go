package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/ingest/embed/ollama"
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

// defaultMaxAttempts bounds how many times fail() will requeue a failed job
// before leaving it terminally 'failed' instead of scheduling another retry
// (see fail's doc comment). Before this ceiling existed, fail() scheduled a
// retry unconditionally, forever: a permanently-failing repo (bad
// credentials, an unparseable file, an embedder that is simply never coming
// back -- exactly the connection-refused case this codebase hit in every
// environment before loam-1dmg gave the e2e stack an embedder) churned
// ingest_jobs between 'failed' and 'queued' indefinitely, growing attempts
// without bound.
//
// 10 is chosen to match backoffDelay's own math against the default backoff
// bounds (defaultBackoffBase, defaultBackoffMax): backoffDelay(10,
// defaultBackoffBase, defaultBackoffMax) is the first attempt whose delay
// actually reaches the 5-minute cap (1,2,4,8,...,256s at attempt 9, then
// capped to 300s at attempt 10) -- see TestBackoffDelay_DoublesPerAttemptUpToCap
// and this bead's tests for the exact numbers. Past that point the backoff
// curve has fully ramped: every further attempt would wait the identical
// plateaued delay for no additional information, so continuing to retry
// stops buying anything a persistent failure couldn't already have shown in
// the first 9 attempts across roughly 17 minutes of accumulated backoff.
//
// This is a hardcoded default with an Option to override (WithMaxAttempts),
// not a LOAM_* environment variable: it follows this file's own precedent
// for defaultBackoffBase/defaultBackoffMax/defaultPollInterval, all three
// of which are also production-hardcoded and only ever overridden via an
// Option from a test harness, never wired to config.go/docs/server-spec.md's
// LOAM_* surface. That is a deliberate difference from LOAM_INGEST_WORKERS
// (a cross-repo parallelism sizing knob an operator genuinely needs to
// tune for their hardware): docs/ingestion-spec.md's own status line still
// marks "retry policy" as firming up during implementation, so exposing a
// fourth environment variable for a policy this package's own author does
// not yet consider settled would commit operators to an interface this
// bead is not the one deciding is stable.
const defaultMaxAttempts = 10

// minMaxAttempts is the floor WithMaxAttempts clamps a non-positive ceiling
// to. Zero or negative would mean fail() never retries at all -- turning
// every transient failure (a briefly-unreachable embedder, a lock
// contention) into a hard failure on its very first attempt, which is
// exactly the over-correction this bead must not introduce: the bug is
// that retry never stops, not that it retries at all. A caller that
// genuinely wants "try once, never retry" already has that at 1 -- attempts
// is never 0 by the time fail() consults it, since the UPDATE that reads it
// back has already incremented.
const minMaxAttempts = 1

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
	maxAttempts  int
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

// WithMaxAttempts overrides the default retry ceiling (defaultMaxAttempts)
// fail() enforces before leaving a job terminally failed instead of
// scheduling another retry. n below minMaxAttempts is clamped up to it --
// see minMaxAttempts's doc comment for why a caller cannot use a
// smaller-than-1 value to mean "never retry".
func WithMaxAttempts(n int) Option {
	if n < minMaxAttempts {
		n = minMaxAttempts
	}
	return func(p *Pool) {
		p.maxAttempts = n
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
		maxAttempts:  defaultMaxAttempts,
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
	for i := range p.workers {
		p.wg.Add(1)
		go p.work(ctx, i)
	}
	p.wg.Wait()
}

// work is one worker goroutine's loop: claim a job if one is available and
// not blocked by per-repo serialization, run it, and otherwise wait for a
// wake-up signal, the poll interval, or shutdown.
//
// This is the per-WORKER panic boundary (loam-jy0p), and it exists
// because run's per-job guard (loam-337) does not cover this whole
// function: the claim() call below -- a driver error mishandled scanning
// a claimed row, say -- runs BEFORE run() is ever invoked, so a panic
// there has no per-job boundary underneath it to be caught by. Without
// recoverWorkerPanic, that panic would unwind straight out of work(),
// which Run spawned directly as a goroutine root, and take down the
// entire process -- the HTTP listener, the policy socket git pushes
// depend on, and every other worker's in-flight job -- exactly the
// failure loam-337 already closed for the job-execution path but left
// open here (loam-lae's close notes flagged this exact gap).
//
// recoverWorkerPanic is deferred FIRST, ahead of wg.Done, purely to match
// this package's own established shape (run's recoverOutcomeRecording is
// registered ahead of release/notifyDrainWaiters for the same reason --
// see its doc comment). It does not change correctness here: wg.Done
// already runs during a panic's unwind regardless of defer order, so
// Pool.Run's wg.Wait() was never at risk from this particular panic (see
// recoverWorkerPanic's own doc comment for what IS at risk).
func (p *Pool) work(ctx context.Context, workerIndex int) {
	defer p.recoverWorkerPanic(ctx, workerIndex)
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

// recoverWorkerPanic is work's own outermost guard, matching
// recoverOutcomeRecording (below) and cmd/server/multirunner.go's
// recoverMember in shape: it never re-panics -- a silent recover would be
// strictly worse than the crash it replaces -- so every recovered panic is
// logged at ERROR with the worker's identity, the recovered value, and a
// stack trace.
//
// What dies with this worker is different from what dies with a job (see
// recoverOutcomeRecording's doc comment): no ingest_jobs row is left
// mid-flight, because the panic happened before any job was claimed (or
// while claiming one, before ownership of that row transferred out of
// claim's own transaction -- claim's deferred rollback still runs during
// the unwind, same as any other panic in this package). What is lost is
// this goroutine itself: one fewer worker draining claimQuery's SELECT ...
// FOR UPDATE SKIP LOCKED, so queued jobs simply accumulate in
// status='queued' with p.workers-1 workers left to service them --
// silently, with no metric or /readyz change (internal/health/health.go
// deliberately excludes the ingest pool from readiness), all the way down
// to zero if every worker eventually panics this way. Restarting the
// process is the only recovery; nothing in this package restarts a dead
// worker goroutine.
func (p *Pool) recoverWorkerPanic(ctx context.Context, workerIndex int) {
	r := recover()
	if r == nil {
		return
	}
	p.logger.ErrorContext(ctx, "recovered panic in ingest worker; this worker has permanently stopped claiming jobs",
		"worker_index", workerIndex, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
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

// This package is the SECOND writer of repos.sync_state; the first is
// internal/mirrorsync/state.Reporter, writing it from the Mirror Sync
// cycle. The two do not race, because the column has a single owner at
// any instant and ownership is handed over explicitly:
//
//   - The sync cycle owns it from ReportSyncing until its terminal
//     report. If step 4 of the cycle enqueued an ingest job,
//     ReportIdle/ReportError deliberately do NOT write (they take
//     enqueuedIngest and return nil) -- ownership of that tick's terminal
//     value passes here instead, and the row is left at 'syncing' for
//     this package to resolve.
//   - This package owns it from the moment a worker claims a job for the
//     repo (syncStateSyncingQuery, committed in the same transaction as
//     ingest_jobs.status='running') until that job resolves
//     (syncStateIdleQuery on success, syncStateErrorQuery on failure,
//     each committed in the same transaction as the job's terminal
//     status).
//
// That is why every one of the three statements below is issued inside
// the same transaction as the ingest_jobs status write it accompanies:
// docs/ingestion-spec.md ("Consistency & Failure": repos.sync_state
// "reflects the latest outcome") is only true if the two rows can never
// be observed disagreeing about which outcome that is. See
// internal/mirrorsync/state/reporter.go's type doc for the sync half of
// the same contract.
//
// Reports here key on repos.id, not repos.name: unlike the Reporter --
// whose callers only ever hold a mirrorsync.RepoID -- every job this
// package runs already carries the FK (ingest_jobs.repo_id), so there is
// no name to resolve and no lookup to spend.
const (
	syncStateSyncingQuery = `UPDATE repos SET sync_state = 'syncing', updated_at = now() WHERE id = $1`
	syncStateIdleQuery    = `UPDATE repos SET sync_state = 'idle', last_synced_at = now(), sync_error = NULL, updated_at = now() WHERE id = $1`
	syncStateErrorQuery   = `UPDATE repos SET sync_state = 'error', sync_error = $2, updated_at = now() WHERE id = $1`
)

// SyncErrorPrefix marks a repos.sync_error this package wrote, as opposed
// to one internal/mirrorsync/state.Reporter wrote from a failed fetch,
// mergeability check, or PR poll. sync_state has three values and no
// author column, so without this an admin (docs/web-spec.md's repo view,
// which surfaces sync_error verbatim) cannot tell "we could not reach
// your forge" apart from "we reached it fine and the index build blew
// up" -- two errors with completely different remedies. It is a message
// prefix rather than a new column because docs/persistence-spec.md's
// repos shape is fixed at three sync columns and the distinction is
// diagnostic, not something any query filters on.
//
// Exported solely so a reader of the column can classify by author
// without restating the literal: cmd/server's acceptance suite asserts
// "the sync cycle we just ran did not error" off sync_state, and now that
// this package writes the same column it needs to tell the two authors
// apart (see acceptance_steps_test.go's assertSyncNotErrored).
const SyncErrorPrefix = "ingest: "

// claim attempts to claim one queued job. It holds mu for the whole
// operation -- snapshotting the busy-repo set, querying, marking the row
// running (and its repo syncing, in the same transaction: see the
// syncStateSyncingQuery block), and recording the repo as busy -- so two
// goroutines in this
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
	if _, err := tx.Exec(ctx, syncStateSyncingQuery, job.RepoID); err != nil {
		return Job{}, false, fmt.Errorf("marking repo %s syncing for ingest job %s: %w", job.RepoID, job.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, false, fmt.Errorf("committing ingest job claim: %w", err)
	}
	p.busy[job.RepoID] = struct{}{}
	return job, true, nil
}

// errJobPanicked marks a job failure that came from a recovered panic
// rather than a returned error, so a reader of ingest_jobs.error (or
// repos.sync_error) can tell "the pipeline reported a problem" apart from
// "the pipeline had a defect". Unexported: nothing outside this package
// branches on it today; it exists to give the recorded message a stable,
// greppable prefix and to give runOrchestrator's test an identity to
// assert on that is not the panic value's own wording.
var errJobPanicked = errors.New("ingest job panicked")

// run executes job through the injected Orchestrator and records the
// outcome. It always releases the repo's per-repo serialization slot
// before returning, whether the job succeeded, failed, or panicked, so a
// coalesced follow-up (or any other queued job for the same repo) becomes
// claimable immediately.
//
// This is the per-job panic boundary (loam-337). Note that a job does NOT
// get a goroutine of its own -- work() calls this inline on the worker
// goroutine -- so before the guards below, a panic anywhere in the
// pipeline (chunking, tree-sitter's cgo boundary, a vector length that
// disagrees with its column dimension) unwound straight out of work(),
// past Run, and terminated the whole server process: the HTTP listener,
// the policy socket git pushes depend on, and every other in-flight job.
// The transaction is not the concern -- internal/chunkstore's transactor
// doc comment records that a panic unwinds PAST the deferred rollback,
// which therefore RUNS, closing the transaction correctly -- the process
// dying is.
//
// Two guards, deliberately, because they cannot be one:
//
//   - runOrchestrator wraps the pipeline call and converts a panic into an
//     ordinary error, so the recorded outcome goes through the SAME fail()
//     path as any other failure (status, error, attempts, sync_state,
//     sync_error, and the backoff/retry fail() already schedules). There is
//     no parallel failure path to keep in sync.
//   - recoverOutcomeRecording is the last-resort guard over succeed/fail
//     themselves. It cannot route into fail(): if fail() is what panicked,
//     calling it again would panic again and escape. So it only logs, and
//     leaves the row where it was -- startup's RequeueOrphaned is what
//     eventually recovers a row stranded in 'running' -- but the process,
//     and every other repo's ingestion, survives.
//
// Neither guard re-panics. That is the entire point: one poisoned repo
// must not be able to halt ingestion for every other repo.
//
// release and notifyDrainWaiters are deferred HERE rather than inside
// succeed and fail (where they used to live, one copy each). Both orders
// run them at the same instant -- succeed/fail are called from nowhere
// else and returning from either is the last thing run does -- but only
// this one reaches them on the panic path too, and reaches them exactly
// once: registered before any statement that can panic, so every exit,
// normal or unwinding, passes through both. That matters more than the
// tidiness: a recovered panic that leaves a repo marked busy forever
// (claim() skips it, so no future job for it is ever claimable) or leaves
// a Drain/Shutdown caller parked on a channel nobody closes has traded a
// crash for a wedge, which is barely an improvement.
func (p *Pool) run(ctx context.Context, job Job) {
	defer p.recoverOutcomeRecording(ctx, job)
	defer p.release(job.RepoID)
	defer p.notifyDrainWaiters(job.RepoID)
	stats, err := p.runOrchestrator(ctx, job)
	if err != nil {
		p.fail(ctx, job, err)
		return
	}
	p.succeed(ctx, job, stats)
}

// runOrchestrator calls the injected Orchestrator, turning a panic into a
// returned error so run's caller-side logic (and fail's bookkeeping) needs
// no panic-specific branch at all.
//
// The stack is captured and logged, not stored on the row: ingest_jobs.
// error is surfaced verbatim in the admin repo view and is copied into
// repos.sync_error under SyncErrorPrefix, and a multi-kilobyte goroutine
// dump in a status field is unreadable there. The recovered value plus
// job_id in the log gives an operator both halves: the row says what class
// of defect it was and which job, the log line keyed by the same job_id
// says where.
func (p *Pool) runOrchestrator(ctx context.Context, job Job) (stats Stats, err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		err = fmt.Errorf("%w: %v", errJobPanicked, r)
		p.logger.ErrorContext(ctx, "recovered panic running ingest job",
			"job_id", job.ID, "repo_id", job.RepoID, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
	}()
	return p.orchestrator.Run(ctx, job)
}

// recoverOutcomeRecording is run's outermost guard: see run's doc comment
// for why it only logs rather than routing into fail, and for why the
// serialization slot and drain waiters are already handled by run's own
// defers by the time this runs.
func (p *Pool) recoverOutcomeRecording(ctx context.Context, job Job) {
	r := recover()
	if r == nil {
		return
	}
	p.logger.ErrorContext(ctx, "recovered panic recording ingest job outcome",
		"job_id", job.ID, "repo_id", job.RepoID, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
}

// succeed records a successful ingest: status=succeeded, stats persisted
// as jsonb, finished_at set (docs/ingestion-spec.md "Chunk -> Embed ->
// Vectors"), and the repo returned to sync_state='idle' with
// last_synced_at advanced and any stale sync_error cleared -- both writes
// in one transaction, per the syncStateSyncingQuery block's ownership
// note.
//
// last_synced_at is advanced here, not only by the sync cycle's own
// ReportIdle: when a cycle enqueues an ingest job, ReportIdle no-ops and
// never touches the column, so a repo that ingests on every tick would
// otherwise report a last successful sync that only ever goes stale
// (docs/persistence-spec.md "repos"; features/enrollment.feature's
// "Enrolled repos report sync status" reads exactly this pair).
//
// Releasing the repo's serialization slot and waking its drain waiters is
// run's job, not this function's -- see run's doc comment for why they
// moved there.
func (p *Pool) succeed(ctx context.Context, job Job, stats Stats) {
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		p.logger.ErrorContext(ctx, "marshaling ingest job stats", "job_id", job.ID, "error", err)
		statsJSON = []byte(`{}`)
	}
	if err := p.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE ingest_jobs SET status = 'succeeded', stats = $2, finished_at = now() WHERE id = $1`,
			job.ID, statsJSON,
		); err != nil {
			return fmt.Errorf("marking ingest job %s succeeded: %w", job.ID, err)
		}
		if _, err := tx.Exec(ctx, syncStateIdleQuery, job.RepoID); err != nil {
			return fmt.Errorf("marking repo %s idle: %w", job.RepoID, err)
		}
		return nil
	}); err != nil {
		p.logger.ErrorContext(ctx, "recording ingest job success", "job_id", job.ID, "error", err)
	}
}

// inTx runs fn inside a single transaction, committing if it returns nil
// and rolling back otherwise. succeed and fail each write two rows
// (ingest_jobs and repos) that must never be observed disagreeing, and
// neither has any other reason to open a transaction by hand.
func (p *Pool) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// fail records a failed ingest -- status=failed, the error, attempts
// incremented, finished_at set -- then, unless abandonReason says
// otherwise, schedules a retry after bounded exponential backoff (bead
// DESCRIPTION: "On failure mark status=failed with the error recorded ...
// retry with exponential backoff (increment attempts)"). The repo moves to
// sync_state='error' carrying the same message under syncErrorPrefix, in
// the same transaction as the job's own terminal status (see the
// syncStateSyncingQuery block).
// The repo's serialization slot is released as soon as this returns (not
// held for the backoff wait -- the retry is a detached goroutine), so a
// coalesced follow-up for the same repo can run right away rather than
// waiting behind this job's retry timer. The release itself is run's
// deferred call, not one of this function's; see run's doc comment.
//
// last_synced_at is deliberately left where it was: the swap orchestrator
// rolled back, so the previous index is still live and still reflects the
// last commit this repo genuinely synced to (docs/ingestion-spec.md
// "Consistency & Failure"). Advancing it here would claim a success that
// did not happen.
//
// sync_state stays 'error' for the whole backoff wait and only returns to
// 'syncing' when a worker actually re-claims the retried job -- the
// bead's "a merely queued job leaves sync_state unchanged" rule, which
// falls out of claim being the only writer of 'syncing' rather than
// needing a rule of its own here.
//
// When abandonReason reports a job should not retry (loam-eean), the row is
// left exactly as the UPDATE above just left it: status='failed', attempts
// at its final count, error carrying the last failure. No new schema state
// is introduced for "permanently abandoned" (ingest_jobs.status's CHECK
// constraint admits only queued/running/succeeded/failed, and
// docs/ingestion-spec.md's DESIGN note prefers the convention over a
// migration): a row that will never retry again is simply a 'failed' row
// whose attempts has stopped climbing, which ListJobs/ListIngestJobs
// already surfaces verbatim (internal/ingest/list.go) -- an admin comparing
// attempts against the documented ceiling (or against a job that is still
// climbing) can already tell the two apart without a new column.
func (p *Pool) fail(ctx context.Context, job Job, runErr error) {
	var attempts int
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`UPDATE ingest_jobs SET status = 'failed', error = $2, attempts = attempts + 1, finished_at = now() WHERE id = $1 RETURNING attempts`,
			job.ID, runErr.Error(),
		).Scan(&attempts); err != nil {
			return fmt.Errorf("marking ingest job %s failed: %w", job.ID, err)
		}
		if _, err := tx.Exec(ctx, syncStateErrorQuery, job.RepoID, SyncErrorPrefix+runErr.Error()); err != nil {
			return fmt.Errorf("marking repo %s errored: %w", job.RepoID, err)
		}
		return nil
	})
	if err != nil {
		p.logger.ErrorContext(ctx, "recording ingest job failure", "job_id", job.ID, "run_error", runErr, "error", err)
		return
	}
	p.logger.ErrorContext(ctx, "ingest job failed", "job_id", job.ID, "repo_id", job.RepoID, "attempts", attempts, "error", runErr)
	if reason := p.abandonReason(attempts, runErr); reason != "" {
		p.logger.WarnContext(ctx, "ingest job abandoned, not scheduling a retry", "job_id", job.ID, "repo_id", job.RepoID, "attempts", attempts, "reason", reason)
		return
	}
	delay := backoffDelay(attempts, p.backoffBase, p.backoffMax)
	p.wg.Add(1)
	go p.scheduleRetry(ctx, job.ID, delay)
}

// abandonReason reports why fail should NOT schedule another retry for a
// job that just recorded its attempts-th failure, or "" if it should retry
// as usual. Two independent reasons stop a retry, checked in this order:
//
//   - ollama.IsPermanent(runErr) -- a failure this codebase's one embedder
//     backend already knows will recur unchanged (a 4xx rejection, which
//     subsumes ollama.IsContextLengthExceeded's "this input can never fit
//     the model's context window" case; a malformed response body; a
//     dimension mismatch). This fires even on attempts==1, before the
//     ceiling below would ever trigger, because burning the whole attempts
//     budget on a request that is provably going to fail identically every
//     time is pure waste, not caution -- IsContextLengthExceeded's own doc
//     comment makes exactly this point.
//   - attempts >= p.maxAttempts -- the general ceiling, which bounds every
//     OTHER kind of failure too: an unrecognized error (a git-mirror
//     failure, a lock-contention error, bad forge credentials) is not
//     provably permanent the way ollama.IsPermanent's cases are, so it is
//     given every chance up to the ceiling rather than abandoned on attempt
//     one -- but it is not retried forever either, which is the actual bug
//     this function exists to fix.
//
// ollama.IsPermanent is deliberately NOT read as `!ollama.IsRetryable`: see
// that function's own doc comment for why the negation is unsafe here --
// IsRetryable is false for any error this package did not produce at all,
// not only for the ones it knows are permanent, and treating "unrecognized"
// as "permanent" would abandon a transient failure from anywhere else in
// the pipeline (e.g. a lock-contention error) on its very first attempt,
// which the bead's ACCEPTANCE CRITERIA explicitly forbids.
//
// loam-c94.16 narrowed WHEN an ollama.IsContextLengthExceeded failure can
// even reach this function, without changing this function itself:
// internal/ingest/vectors' embedAll now catches that specific error at the
// embed call site, splits the offending chunk (reusing
// internal/ingest/chunk's own splitting), and retries -- recursively, down
// to a hard per-rune split -- before ever returning an error up through
// Prepare to the orchestrator and finally here. So an
// ollama.IsContextLengthExceeded that DOES reach abandonReason today means
// the split was already attempted and still failed (the one genuinely
// un-embeddable case: a single already-minimal piece the model's context
// window still rejects). Treating that as permanent on attempt one remains
// correct for the same reason as before -- a job-level retry re-runs the
// unchanged content through that same exhausted split-and-retry and fails
// identically -- so no change to the check itself was needed, only this
// note on why the precondition the old comment assumed ("this input can
// never fit") is now enforced one layer down instead of assumed here.
func (p *Pool) abandonReason(attempts int, runErr error) string {
	if ollama.IsPermanent(runErr) {
		return "permanently classified failure, not retryable"
	}
	if attempts >= p.maxAttempts {
		return fmt.Sprintf("retry ceiling reached (%d/%d attempts)", attempts, p.maxAttempts)
	}
	return ""
}

// scheduleRetry waits delay, then flips job jobID back to queued so a
// worker can claim it again. If ctx is canceled first (server shutdown),
// it exits without touching the row: the row stays failed, and the next
// trigger for that repo+branch+kind (e.g. the following sync tick) simply
// enqueues a fresh job via Enqueue, since Enqueue's coalescing only
// considers status='queued' rows.
//
// recoverScheduleRetryPanic is deferred FIRST, ahead of wg.Done, for the
// same reason work's recoverWorkerPanic is: fail() spawns this as its own
// goroutine (`go p.scheduleRetry(...)`), not something run() inline-calls
// under its own guard, so a panic in the UPDATE below -- a driver error,
// say -- has no per-job boundary above this frame to be caught by and
// would otherwise take down the whole process (loam-jy0p; loam-lae's
// close notes flagged this exact gap alongside work's).
func (p *Pool) scheduleRetry(ctx context.Context, jobID uuid.UUID, delay time.Duration) {
	defer p.recoverScheduleRetryPanic(ctx, jobID)
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

// recoverScheduleRetryPanic is scheduleRetry's own panic boundary,
// matching recoverWorkerPanic and recoverOutcomeRecording in shape: it
// never re-panics, so every recovered panic is logged at ERROR with the
// job's identity, the recovered value, and a stack trace.
//
// What dies here is narrower than a dead worker: exactly one job, the
// jobID this goroutine was scheduling a retry for, never returns from
// 'failed' to 'queued' -- no other job, and no worker, is affected. That
// row is left stuck in status='failed' indefinitely: unlike a deliberate
// abandonment (abandonReason), which is a terminal outcome recorded in the
// row's own attempts/error columns, this is a silent stall an admin
// reading the row cannot distinguish from "still waiting out its backoff"
// without also checking whether the process logged this line. The repo's
// serialization slot is unaffected -- fail() already released it before
// spawning this goroutine (run's own doc comment) -- so the repo itself
// is not blocked; only this one job's path back into the queue is lost.
func (p *Pool) recoverScheduleRetryPanic(ctx context.Context, jobID uuid.UUID) {
	r := recover()
	if r == nil {
		return
	}
	p.logger.ErrorContext(ctx, "recovered panic scheduling ingest job retry; this job will not return to queued",
		"job_id", jobID, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
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
// repoID so each re-checks real DB state. It is deferred once, in run, so
// it fires after every job outcome that can reduce repoID's
// queued-or-running count (succeed, fail, and a fail reached from a
// recovered orchestrator panic) and never after a transition that only
// holds or increases it.
//
// The one case where it wakes a waiter that cannot yet have changed is
// run's outermost guard: a panic in succeed/fail itself leaves the row in
// 'running', so a woken waiter re-checks, finds itself still un-drained,
// and re-registers -- a wasted round trip, not a wrong answer, and far
// better than the alternative of leaving a Drain/Shutdown caller parked on
// a channel nobody will ever close.
func (p *Pool) notifyDrainWaiters(repoID uuid.UUID) {
	p.mu.Lock()
	waiters := p.drainWaiters[repoID]
	delete(p.drainWaiters, repoID)
	p.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}
