// Package repoadmin implements loam.admin.v1.RepoAdminService
// (docs/web-spec.md -> "RepoAdminService"): enroll, list, inspect, remove,
// and reindex enrolled repos, plus the admin Jobs view. Unlike every
// loam.v1.* handler in internal/handler's sibling packages, this
// package's methods gate on NOTHING beyond admin basic auth: the whole
// /loam.admin.v1.* path group is wrapped by httpauth.Auth.AdminOnly
// before any request reaches a handler (docs/web-spec.md -> Auth: "Basic-
// auth middleware wraps the admin RPC paths"), so there is no per-RPC
// handler.CapabilityChecker call here the way loam.v1.RepoService.GetRepo
// has one. RemoveRepo is the single exception, and re-checks admin itself
// as defence in depth -- see requireAdmin (remove.go) for the narrow line
// that draws.
//
// EnrollRepo owns the initial bare-mirror clone (loam-ofg.12 NOTES,
// confirmed missing tree-wide by loam-giq.2's review: the only
// `git clone --mirror`/`git init --bare` hits anywhere were test-only,
// internal/fakeforge/seed.go and internal/testfixture/fixture.go).
// RemoveRepo is the inverse and is split the same way EnrollRepo is not:
// this package owns the guard (enumerate blocking non-terminal work
// branches, typed RemovalBlocked detail) and delegates the cross-table
// repos-row delete plus the mirror removal to internal/reporemove
// (loam-cwb), behind the repoDeleter interface below.
package repoadmin

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

//go:generate go tool moq -out moq_test.go . repoStore workBranchLister credentialResolver upstreamChecker cloner ingestEnqueuer jobLister repoDeleter

// repoStore is the internal/reposstore.Store surface this package's
// Handler needs, defined here at the consumer per repo convention.
// *reposstore.Store satisfies it structurally.
type repoStore interface {
	CreateRepo(ctx context.Context, params reposstore.CreateRepoParams) (reposstore.Repo, error)
	GetRepoByName(ctx context.Context, name string) (reposstore.Repo, error)
	ListRepos(ctx context.Context, page reposstore.Page) (reposstore.ListReposResult, error)
	UpdateRepo(ctx context.Context, id uuid.UUID, params reposstore.UpdateRepoParams) (reposstore.Repo, error)
	UpdateSyncState(ctx context.Context, id uuid.UUID, state reposstore.SyncState, lastSyncedAt *time.Time, syncErr *string) (reposstore.Repo, error)
	AddTargetBranch(ctx context.Context, repoID uuid.UUID, branch string) (reposstore.TargetBranch, error)
	ListTargetBranches(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error)
	RemoveTargetBranch(ctx context.Context, repoID uuid.UUID, branch string) error
}

// workBranchLister is the internal/workbranchstore.Store surface
// RemoveRepo's guard check needs, defined here at the consumer.
// *workbranchstore.Store satisfies it structurally.
type workBranchLister interface {
	List(ctx context.Context, filter workbranchstore.ListFilter, limit, offset int32) ([]workbranchstore.WorkBranch, int64, error)
}

// credentialResolver resolves a forge host's decrypted credential,
// defined here at the consumer. *credentialstore.Store satisfies it
// structurally (loam-54o.8).
type credentialResolver interface {
	GetByHost(ctx context.Context, host string) (credentialstore.Credential, error)
}

// upstreamChecker confirms a resolved credential can read AND write
// upstreamURL -- EnrollRepo's authoritative gate before it attempts the
// (potentially expensive) initial clone (docs/web-spec.md ->
// RepoAdminService: "EnrollRepo's CheckRepo (read + write probes) remains
// the authoritative gate"). Defined here at the consumer so this package
// never constructs a *forge.Forgejo itself or imports net/http; the
// production adapter, ForgeChecker (checker.go, this package), builds a
// fresh, single-use *forge.Forgejo bound to host+token per call.
type upstreamChecker interface {
	CheckRepo(ctx context.Context, host, token, upstreamURL string) error
}

// cloner runs the authenticated git subprocesses EnrollRepo (the initial
// mirror clone) and ProbeRepo (a pre-enrollment ls-remote) need, defined
// here at the consumer. *gittransport.Transport satisfies it structurally
// (loam-giq.3): every credential injection, host-config isolation, and
// secret-scrubbing property lives there, not here.
type cloner interface {
	Clone(ctx context.Context, host, mirrorDir, upstreamURL string) ([]byte, error)
	LsRemote(ctx context.Context, host, upstreamURL string) ([]byte, error)
}

// mirrorReconciler idempotently hardens a freshly cloned bare mirror
// (receive.deny* config + the pre-receive hook stub), defined here at the
// consumer as a plain function type so this package depends only on
// internal/mirrorreconcile's signature, never its implementation. The
// production value is mirrorreconcile.ReconcileMirror itself
// (loam-ofg.19): call AFTER the clone, since it expects an existing repo
// (that package's own doc comment).
type mirrorReconciler func(ctx context.Context, repoPath string) error

// ingestEnqueuer requests an ingest job, defined here at the consumer.
// *ingest.Pool satisfies it structurally (loam-c94.1): EnrollRepo enqueues
// a FULL job for the indexed branch on first enrollment
// (docs/ingestion-spec.md "Trigger & Scheduling": "on first enrollment --
// it enqueues an ingest job"), SetTargetBranches enqueues one when
// indexed_branch changes, and ReindexRepo enqueues one on demand.
type ingestEnqueuer interface {
	Enqueue(ctx context.Context, repoID uuid.UUID, targetBranch string, kind ingest.Kind) error
}

// jobLister lists ingest_jobs rows for the web Jobs view, defined here at
// the consumer. *ingest.Pool satisfies it structurally via its ListJobs
// method (this bead).
type jobLister interface {
	ListJobs(ctx context.Context, filter ingest.ListJobsFilter, limit, offset int32) ([]ingest.JobRecord, int64, error)
}

// repoDeleter drops a repos row and everything that must disappear with
// it -- the mirror on disk, repo_target_branches (ON DELETE CASCADE,
// 0001_init.up.sql), work branch history, and derived graph/vector
// indexes (docs/web-spec.md -> RepoAdminService "RemoveRepo": "drops
// mirror + derived + metadata incl. history and deletes ingest jobs").
// Defined here at the consumer; *reporemove.Remover (loam-cwb) is the
// production implementation and owns every ordering and partial-failure
// decision behind this one method. This package's own RemoveRepo scope
// stops at the guard (enumerate blocking non-terminal work branches,
// return a typed RemovalBlocked detail) -- see that method's doc comment.
//
// The signature is one error, not a partial-success report, deliberately:
// RemoveRepo has exactly one thing to tell its caller ("the repo is
// unenrolled" or "it is not"), and the one asymmetric case behind this
// seam -- a mirror moved aside but not fully deleted, which leaves the
// canonical path free and the contract met -- is logged there rather than
// dressed up as a third outcome this RPC's proto response (an empty
// RemoveRepoResponse) has nowhere to put.
type repoDeleter interface {
	DeleteRepo(ctx context.Context, id uuid.UUID) error
}
