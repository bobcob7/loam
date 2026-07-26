package reposstore

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bobcob7/loam/internal/db/gen"
)

// pgUUID converts a uuid.UUID to the pgtype.UUID sqlc's pgx/v5 codegen
// expects as a query parameter.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// uuidFromPG converts a valid pgtype.UUID (as returned by a successful
// query) back to uuid.UUID.
func uuidFromPG(id pgtype.UUID) uuid.UUID {
	return uuid.UUID(id.Bytes)
}

// timeFromPG converts a NOT NULL timestamptz column to time.Time.
func timeFromPG(t pgtype.Timestamptz) time.Time {
	return t.Time
}

// ptrTimeFromPG converts a nullable timestamptz column (repos.last_synced_at,
// repo_target_branches.ingested_at) to *time.Time, nil when the column is
// NULL.
func ptrTimeFromPG(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	value := t.Time
	return &value
}

// ptrStringFromPG converts a nullable text column (repos.sync_error) to
// *string, nil when the column is NULL.
func ptrStringFromPG(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	value := t.String
	return &value
}

// ingestedRefFromText converts repo_target_branches.ingested_ref to
// IngestedRef. A NULL column (Valid: false) becomes IngestedRef{}, whose
// zero value Ok=false is exactly the "no valid diff base" signal --
// callers cannot read Ref without also seeing Ok is false.
func ingestedRefFromText(t pgtype.Text) IngestedRef {
	if !t.Valid {
		return IngestedRef{}
	}
	return IngestedRef{Ref: t.String, Ok: true}
}

// fromGenRepo converts a sqlc-generated gen.Repo row to this package's
// Repo domain type.
func fromGenRepo(r gen.Repo) Repo {
	return Repo{
		ID:            uuidFromPG(r.ID),
		Name:          r.Name,
		UpstreamURL:   r.UpstreamUrl,
		ForgeHost:     r.ForgeHost,
		IndexedBranch: r.IndexedBranch,
		SyncState:     r.SyncState,
		LastSyncedAt:  ptrTimeFromPG(r.LastSyncedAt),
		SyncError:     ptrStringFromPG(r.SyncError),
		CreatedAt:     timeFromPG(r.CreatedAt),
		UpdatedAt:     timeFromPG(r.UpdatedAt),
	}
}

// fromGenTargetBranch converts a sqlc-generated gen.RepoTargetBranch row
// to this package's TargetBranch domain type.
func fromGenTargetBranch(r gen.RepoTargetBranch) TargetBranch {
	return TargetBranch{
		RepoID:           uuidFromPG(r.RepoID),
		Branch:           r.Branch,
		IngestedRef:      ingestedRefFromText(r.IngestedRef),
		IngestedAt:       ptrTimeFromPG(r.IngestedAt),
		IngestedVersions: json.RawMessage(r.IngestedVersions),
	}
}
