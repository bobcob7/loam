package reposstore

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Repo is one enrolled repo (docs/persistence-spec.md "repos"). Name is
// the "<group>/<repo_name>" identifier callers hold and the settled RepoID
// (loam-54o.7 NOTES); ID is the FK other tables reference, resolved from
// Name via GetRepoByName's single indexed lookup.
type Repo struct {
	ID            uuid.UUID
	Name          string
	UpstreamURL   string
	ForgeHost     string
	IndexedBranch string
	SyncState     string
	LastSyncedAt  *time.Time
	SyncError     *string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CreateRepoParams is the input to Store.CreateRepo. The store assigns a
// fresh UUIDv7 id (docs/persistence-spec.md "Conventions"); callers never
// supply one.
type CreateRepoParams struct {
	Name          string
	UpstreamURL   string
	ForgeHost     string
	IndexedBranch string
}

// UpdateRepoParams is the input to Store.UpdateRepo. Name is deliberately
// absent: loam-54o.7 NOTES settled that no rename path exists anywhere in
// the proto surface, so repos.name is immutable once created.
type UpdateRepoParams struct {
	UpstreamURL   string
	ForgeHost     string
	IndexedBranch string
}

// SyncState is one of repos.sync_state's three CHECK-constrained values
// (docs/persistence-spec.md "repos"; 0001_init.up.sql's
// repos_sync_state_check). RepoAdminService.EnrollRepo (loam-ofg.12) is
// this type's first caller: it marks a repo Syncing for the duration of
// the initial mirror clone, then Idle on success or Error on failure --
// the same three states the (not yet wired) mirror-sync scheduler's own
// SyncStateReporter reports on every later cycle.
type SyncState string

// The three values repos.sync_state's CHECK constraint allows.
const (
	SyncStateIdle    SyncState = "idle"
	SyncStateSyncing SyncState = "syncing"
	SyncStateError   SyncState = "error"
)

// Page is an offset-pagination request (docs/persistence-spec.md
// "Conventions"; mirrors proto's loam.v1.Page). A non-positive Limit is
// replaced with defaultListLimit by Store.ListRepos, matching the proto
// contract that 0 means "use the server default."
type Page struct {
	Limit  int
	Offset int
}

// ListReposResult is a page of repos plus the total matching count
// (docs/persistence-spec.md "Conventions": "offset pagination via
// LIMIT/OFFSET plus a COUNT(*) for PageInfo.total").
type ListReposResult struct {
	Repos []Repo
	Total int
}

// IngestedRef is the incremental-ingest diff base recorded for one target
// branch (repo_target_branches.ingested_ref). Ok is false when the column
// is NULL -- no ingest has completed for this branch yet, which
// loam-c94.2's planner reads as "no valid diff base, do a full rebuild."
// Callers must check Ok before using Ref: there is no zero-value string
// standing in for "no ref" here, so a caller cannot mistake an unchecked
// zero value for a real (empty) ref.
type IngestedRef struct {
	Ref string
	Ok  bool
}

// TargetBranch is one row of repo_target_branches
// (docs/persistence-spec.md "repo_target_branches"): a branch eligible as
// a work-branch target, plus the incremental-ingest state for it.
type TargetBranch struct {
	RepoID           uuid.UUID
	Branch           string
	IngestedRef      IngestedRef
	IngestedAt       *time.Time
	IngestedVersions json.RawMessage
}
