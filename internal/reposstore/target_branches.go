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

// errEmptyRef guards Store.AdvanceIngestedRef against writing an empty
// string as the diff base. AdvanceIngestedRefParams.IngestedRef is always
// written Valid: true (see AdvanceIngestedRef below), so an empty ref
// would silently persist as a non-NULL, meaningless diff base rather than
// the NULL that means "no valid diff base" -- this sentinel is the only
// place that distinction is enforced.
var errEmptyRef = errors.New("reposstore: ingested ref must not be empty")

// AddTargetBranch enrolls branch as a target for repoID, idempotently: an
// already-enrolled branch is returned unchanged, its ingested_ref/
// ingested_at/ingested_versions untouched (see repo_target_branches.sql's
// AddTargetBranch query comment).
func (s *Store) AddTargetBranch(ctx context.Context, repoID uuid.UUID, branch string) (TargetBranch, error) {
	row, err := s.db.AddTargetBranch(ctx, gen.AddTargetBranchParams{RepoID: pgUUID(repoID), Branch: branch})
	if err != nil {
		return TargetBranch{}, fmt.Errorf("adding target branch %s for repo %s: %w", branch, repoID, err)
	}
	return fromGenTargetBranch(row), nil
}

// ListTargetBranches returns every branch enrolled as a target for
// repoID, ordered by branch name.
func (s *Store) ListTargetBranches(ctx context.Context, repoID uuid.UUID) ([]TargetBranch, error) {
	rows, err := s.db.ListTargetBranches(ctx, pgUUID(repoID))
	if err != nil {
		return nil, fmt.Errorf("listing target branches for repo %s: %w", repoID, err)
	}
	branches := make([]TargetBranch, len(rows))
	for i, row := range rows {
		branches[i] = fromGenTargetBranch(row)
	}
	return branches, nil
}

// RemoveTargetBranch un-enrolls branch as a target for repoID. Returns a
// wrapped ErrNotFound if branch was not enrolled (RowsAffected == 0),
// rather than silently succeeding on a no-op delete.
func (s *Store) RemoveTargetBranch(ctx context.Context, repoID uuid.UUID, branch string) error {
	affected, err := s.db.RemoveTargetBranch(ctx, gen.RemoveTargetBranchParams{RepoID: pgUUID(repoID), Branch: branch})
	if err != nil {
		return fmt.Errorf("removing target branch %s for repo %s: %w", branch, repoID, err)
	}
	if affected == 0 {
		return fmt.Errorf("removing target branch %s for repo %s: %w", branch, repoID, ErrNotFound)
	}
	return nil
}

// IngestedRef returns the incremental-ingest diff base recorded for
// repoID+branch. Ok is false when repo_target_branches.ingested_ref IS
// NULL -- no ingest has completed for this branch, which loam-c94.2's
// planner reads as "no valid diff base, do a full rebuild" -- so callers
// must check Ok before treating Ref as a real ref. Returns a wrapped
// ErrNotFound if branch is not enrolled as a target for repoID at all,
// distinct from an IngestedRef with Ok false: the row can exist with a
// NULL ingested_ref, or not exist at all, and only the latter is
// ErrNotFound.
func (s *Store) IngestedRef(ctx context.Context, repoID uuid.UUID, branch string) (IngestedRef, error) {
	row, err := s.db.GetTargetBranch(ctx, gen.GetTargetBranchParams{RepoID: pgUUID(repoID), Branch: branch})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IngestedRef{}, fmt.Errorf("getting ingested ref for repo %s branch %s: %w", repoID, branch, ErrNotFound)
		}
		return IngestedRef{}, fmt.Errorf("getting ingested ref for repo %s branch %s: %w", repoID, branch, err)
	}
	return ingestedRefFromText(row.IngestedRef), nil
}

// AdvanceIngestedRef records a successful ingest's diff base for
// repoID+branch: ref becomes the new base for the next incremental diff,
// ingestedAt is when the ingest completed, and versions is the
// grammar/pipeline/embedding-model versions it was built with
// (docs/persistence-spec.md "repo_target_branches"). ref must be
// non-empty: this method has no path that writes ingested_ref back to
// NULL, so the NULL state is only ever the branch's initial,
// never-ingested state from AddTargetBranch -- advancing to an empty
// string would otherwise persist a non-NULL, meaningless diff base
// instead. Returns a wrapped ErrNotFound if branch is not enrolled as a
// target for repoID.
func (s *Store) AdvanceIngestedRef(ctx context.Context, repoID uuid.UUID, branch, ref string, ingestedAt time.Time, versions []byte) (TargetBranch, error) {
	if ref == "" {
		return TargetBranch{}, fmt.Errorf("advancing ingested ref for repo %s branch %s: %w", repoID, branch, errEmptyRef)
	}
	row, err := s.db.AdvanceIngestedRef(ctx, gen.AdvanceIngestedRefParams{
		RepoID:           pgUUID(repoID),
		Branch:           branch,
		IngestedRef:      pgtype.Text{String: ref, Valid: true},
		IngestedAt:       pgtype.Timestamptz{Time: ingestedAt, Valid: true},
		IngestedVersions: versions,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TargetBranch{}, fmt.Errorf("advancing ingested ref for repo %s branch %s: %w", repoID, branch, ErrNotFound)
		}
		return TargetBranch{}, fmt.Errorf("advancing ingested ref for repo %s branch %s: %w", repoID, branch, err)
	}
	return fromGenTargetBranch(row), nil
}
