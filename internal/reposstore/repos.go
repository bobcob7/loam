package reposstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bobcob7/loam/internal/db/gen"
)

// CreateRepo enrolls a new repo, assigning it a fresh UUIDv7 id. A
// duplicate params.Name violates repos_name_key and is returned wrapped,
// unmapped to ErrNotFound (that sentinel is reserved for absence, not
// conflict); callers match a uniqueness violation themselves if they need
// to distinguish it.
func (s *Store) CreateRepo(ctx context.Context, params CreateRepoParams) (Repo, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Repo{}, fmt.Errorf("generating repo id: %w", err)
	}
	row, err := s.db.CreateRepo(ctx, gen.CreateRepoParams{
		ID:            pgUUID(id),
		Name:          params.Name,
		UpstreamUrl:   params.UpstreamURL,
		ForgeHost:     params.ForgeHost,
		IndexedBranch: params.IndexedBranch,
	})
	if err != nil {
		return Repo{}, fmt.Errorf("creating repo %s: %w", params.Name, err)
	}
	return fromGenRepo(row), nil
}

// GetRepoByID returns the repo with id, or a wrapped ErrNotFound if none
// exists.
func (s *Store) GetRepoByID(ctx context.Context, id uuid.UUID) (Repo, error) {
	row, err := s.db.GetRepoByID(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Repo{}, fmt.Errorf("getting repo %s: %w", id, ErrNotFound)
		}
		return Repo{}, fmt.Errorf("getting repo %s: %w", id, err)
	}
	return fromGenRepo(row), nil
}

// GetRepoByName resolves name -- the natural key every caller outside this
// package holds (loam-54o.7 NOTES on the settled RepoID decision) -- to its
// full Repo row, including the id other tables' FKs reference. This is a
// single indexed lookup via repos_name_key UNIQUE (name)
// (0001_init.up.sql), not a table scan: it is the intended path for
// resolving a held repo name to the id a downstream query needs. Returns a
// wrapped ErrNotFound if name is not enrolled.
func (s *Store) GetRepoByName(ctx context.Context, name string) (Repo, error) {
	row, err := s.db.GetRepoByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Repo{}, fmt.Errorf("getting repo %s: %w", name, ErrNotFound)
		}
		return Repo{}, fmt.Errorf("getting repo %s: %w", name, err)
	}
	return fromGenRepo(row), nil
}

// ListRepos returns one page of repos ordered by name, plus the total
// count across all pages (docs/persistence-spec.md "Conventions"). A
// non-positive page.Limit is replaced with defaultListLimit.
func (s *Store) ListRepos(ctx context.Context, page Page) (ListReposResult, error) {
	limit := page.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.db.ListRepos(ctx, gen.ListReposParams{Limit: int32(limit), Offset: int32(page.Offset)})
	if err != nil {
		return ListReposResult{}, fmt.Errorf("listing repos: %w", err)
	}
	total, err := s.db.CountRepos(ctx)
	if err != nil {
		return ListReposResult{}, fmt.Errorf("counting repos: %w", err)
	}
	repos := make([]Repo, len(rows))
	for i, row := range rows {
		repos[i] = fromGenRepo(row)
	}
	return ListReposResult{Repos: repos, Total: int(total)}, nil
}

// ListAllRepoNames returns every enrolled repo's name, unpaginated and
// ordered by name (loam-13z). This is deliberately not ListRepos: that
// method's LIMIT/OFFSET pagination and full Repo rows exist for the admin
// API's list view, a human paging through a bounded screen -- a caller
// that re-enumerates every enrolled repo on a fixed schedule (e.g.
// mirrorsync.Scheduler, via mirrorsync.StoreRepoLister) wants the entire
// enrollment in one call, not one page of it, and only the bare name
// (mirrorsync.RepoID is repos.name, not repos.id -- loam-54o.7 NOTES), not
// the full row.
func (s *Store) ListAllRepoNames(ctx context.Context) ([]string, error) {
	names, err := s.db.ListRepoNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing repo names: %w", err)
	}
	return names, nil
}

// UpdateRepo updates the enrollment-config fields of the repo with id
// (upstream_url, forge_host, indexed_branch -- e.g.
// RepoAdminService.SetTargetBranches re-pointing indexed_branch). Returns
// a wrapped ErrNotFound if id does not exist.
func (s *Store) UpdateRepo(ctx context.Context, id uuid.UUID, params UpdateRepoParams) (Repo, error) {
	row, err := s.db.UpdateRepo(ctx, gen.UpdateRepoParams{
		ID:            pgUUID(id),
		UpstreamUrl:   params.UpstreamURL,
		ForgeHost:     params.ForgeHost,
		IndexedBranch: params.IndexedBranch,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Repo{}, fmt.Errorf("updating repo %s: %w", id, ErrNotFound)
		}
		return Repo{}, fmt.Errorf("updating repo %s: %w", id, err)
	}
	return fromGenRepo(row), nil
}

// UpdateSyncState sets the repos row's sync_state (+ last_synced_at, +
// sync_error) for id -- the write RepoAdminService.EnrollRepo (loam-ofg.12)
// uses to mark Syncing for the duration of the initial mirror clone, then
// Idle (lastSyncedAt non-nil, syncErr nil) on success or Error (syncErr
// non-nil) on failure; the same three-state contract the mirror-sync
// scheduler's own SyncStateReporter will report on every later cycle, once
// wired. lastSyncedAt nil clears the column to NULL (never touched yet, or
// deliberately not advanced on this call, e.g. a Syncing/Error write);
// syncErr nil clears sync_error to NULL (the normal case outside an Error
// write). Returns a wrapped ErrNotFound if id does not exist.
func (s *Store) UpdateSyncState(ctx context.Context, id uuid.UUID, state SyncState, lastSyncedAt *time.Time, syncErr *string) (Repo, error) {
	row, err := s.db.UpdateRepoSyncState(ctx, gen.UpdateRepoSyncStateParams{
		ID:           pgUUID(id),
		SyncState:    string(state),
		LastSyncedAt: pgTimestamptz(lastSyncedAt),
		SyncError:    pgText(syncErr),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Repo{}, fmt.Errorf("updating sync state for repo %s: %w", id, ErrNotFound)
		}
		return Repo{}, fmt.Errorf("updating sync state for repo %s: %w", id, err)
	}
	return fromGenRepo(row), nil
}

// pgTimestamptz converts a nullable *time.Time to pgtype.Timestamptz,
// NULL (Valid: false) when t is nil.
func pgTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// pgText converts a nullable *string to pgtype.Text, NULL (Valid: false)
// when s is nil.
func pgText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}
