package reposstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bobcob7/loam/internal/db/gen"
)

// CreateRepo enrolls a new repo, assigning it a fresh UUIDv7 id. A
// duplicate params.Name violates repos_name_key and is returned wrapped,
// unmapped to errNotFound (that sentinel is reserved for absence, not
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

// GetRepoByID returns the repo with id, or a wrapped errNotFound if none
// exists.
func (s *Store) GetRepoByID(ctx context.Context, id uuid.UUID) (Repo, error) {
	row, err := s.db.GetRepoByID(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Repo{}, fmt.Errorf("getting repo %s: %w", id, errNotFound)
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
// wrapped errNotFound if name is not enrolled.
func (s *Store) GetRepoByName(ctx context.Context, name string) (Repo, error) {
	row, err := s.db.GetRepoByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Repo{}, fmt.Errorf("getting repo %s: %w", name, errNotFound)
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

// UpdateRepo updates the enrollment-config fields of the repo with id
// (upstream_url, forge_host, indexed_branch -- e.g.
// RepoAdminService.SetTargetBranches re-pointing indexed_branch). Returns
// a wrapped errNotFound if id does not exist.
func (s *Store) UpdateRepo(ctx context.Context, id uuid.UUID, params UpdateRepoParams) (Repo, error) {
	row, err := s.db.UpdateRepo(ctx, gen.UpdateRepoParams{
		ID:            pgUUID(id),
		UpstreamUrl:   params.UpstreamURL,
		ForgeHost:     params.ForgeHost,
		IndexedBranch: params.IndexedBranch,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Repo{}, fmt.Errorf("updating repo %s: %w", id, errNotFound)
		}
		return Repo{}, fmt.Errorf("updating repo %s: %w", id, err)
	}
	return fromGenRepo(row), nil
}
