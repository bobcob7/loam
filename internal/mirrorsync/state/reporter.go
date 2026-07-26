package state

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/mirrorsync"
)

// errRepoNotFound is returned when a report targets a RepoID with no
// matching repos row, so a caller can tell "nothing to update" apart from a
// transport failure rather than have it pass silently as success.
var errRepoNotFound = errors.New("repo not found")

// Reporter persists Mirror Sync cycle outcomes to repos.sync_state,
// implementing mirrorsync.SyncStateReporter (docs/sync-spec.md -> Mirror
// Sync, :85). It writes syncing unconditionally at cycle start, but treats
// idle/error as terminal writes it only owns when the cycle did not enqueue
// an ingest job: once step 4 enqueues a job, ownership of sync_state for
// that tick passes to the ingest worker (loam-c94.13) until the job
// resolves, so ReportIdle/ReportError become no-ops whenever enqueuedIngest
// is true — writing anyway would race the ingest-side writer over the same
// column and tick (loam-giq.9 DESIGN).
type Reporter struct {
	db execer
}

// New builds a Reporter backed by db, typically a *pgxpool.Pool.
func New(db execer) *Reporter {
	return &Reporter{db: db}
}

var _ mirrorsync.SyncStateReporter = (*Reporter)(nil)

// ReportSyncing sets repos.sync_state = 'syncing' at the start of repo's
// cycle. It always writes: the running state is never contested by another
// writer, so there is no ownership hand-off to honor here.
func (r *Reporter) ReportSyncing(ctx context.Context, repo mirrorsync.RepoID) error {
	id, err := parseRepoID(repo)
	if err != nil {
		return fmt.Errorf("reporting sync syncing for repo %s: %w", repo, err)
	}
	tag, err := r.db.Exec(ctx, `UPDATE repos SET sync_state = 'syncing', updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("reporting sync syncing for repo %s: %w", repo, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("reporting sync syncing for repo %s: %w", repo, errRepoNotFound)
	}
	return nil
}

// ReportIdle sets repos.sync_state = 'idle' with last_synced_at = now() and
// clears any stale sync_error, once all non-ingest steps (1,2,3,5) completed
// without error. When enqueuedIngest is true, ownership of this tick's
// terminal write has already passed to the ingest worker (loam-c94.13):
// ReportIdle does nothing and returns nil rather than clobber whatever the
// ingest worker writes when the job resolves.
func (r *Reporter) ReportIdle(ctx context.Context, repo mirrorsync.RepoID, enqueuedIngest bool) error {
	if enqueuedIngest {
		return nil
	}
	id, err := parseRepoID(repo)
	if err != nil {
		return fmt.Errorf("reporting sync idle for repo %s: %w", repo, err)
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE repos SET sync_state = 'idle', last_synced_at = now(), sync_error = NULL, updated_at = now() WHERE id = $1`,
		id)
	if err != nil {
		return fmt.Errorf("reporting sync idle for repo %s: %w", repo, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("reporting sync idle for repo %s: %w", repo, errRepoNotFound)
	}
	return nil
}

// ReportError sets repos.sync_state = 'error' with sync_error = syncErr's
// message, when a non-ingest step (1,2,3,5) aborted the cycle. As with
// ReportIdle, enqueuedIngest true means step 4 already handed ownership of
// this tick's terminal write to the ingest worker — the case where step 4
// succeeds but step 5 (PR polling) subsequently fails — so ReportError does
// nothing and returns nil.
func (r *Reporter) ReportError(ctx context.Context, repo mirrorsync.RepoID, syncErr error, enqueuedIngest bool) error {
	if enqueuedIngest {
		return nil
	}
	id, err := parseRepoID(repo)
	if err != nil {
		return fmt.Errorf("reporting sync error for repo %s: %w", repo, err)
	}
	message := ""
	if syncErr != nil {
		message = syncErr.Error()
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE repos SET sync_state = 'error', sync_error = $1, updated_at = now() WHERE id = $2`,
		message, id)
	if err != nil {
		return fmt.Errorf("reporting sync error for repo %s: %w", repo, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("reporting sync error for repo %s: %w", repo, errRepoNotFound)
	}
	return nil
}

// parseRepoID validates repo as UUID text before it reaches Postgres,
// surfacing a clear error rather than a driver-level "invalid input syntax
// for type uuid". repos.id is a UUID(v7) primary key (docs/persistence-spec.md
// -> "Conventions"); RepoID's producer (repo enrollment, loam-54o.7) is not
// implemented yet, so this package's operating assumption is that RepoID
// carries that id's text form. See this bead's ASSUMPTIONS in the final
// report.
func parseRepoID(repo mirrorsync.RepoID) (uuid.UUID, error) {
	id, err := uuid.Parse(string(repo))
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("invalid repo id: %w", err)
	}
	return id, nil
}
