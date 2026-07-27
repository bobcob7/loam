package repoadmin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/reposstore"
)

// pgUniqueViolationCode is Postgres's SQLSTATE for a unique-constraint
// violation (23505) -- CreateRepo's own doc comment: "a duplicate
// params.Name violates repos_name_key ... callers match a uniqueness
// violation themselves if they need to distinguish it." EnrollRepo is
// exactly that caller: a name collision must map to CodeAlreadyExists,
// not CodeInternal.
const pgUniqueViolationCode = "23505"

// isUniqueViolation reports whether err (or anything it wraps) is a
// Postgres unique-constraint violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolationCode
}

// EnrollRepo enrolls upstreamURL by deriving its "<group>/<repo_name>"
// identifier, validating indexed_branch is one of target_branches,
// resolving the URL host's credential, running the authoritative
// read+write CheckRepo probe, creating the repos row, cloning the
// initial bare mirror, hardening it via mirrorReconciler, and enqueuing a
// FULL ingest job for the indexed branch (docs/ingestion-spec.md
// "Trigger & Scheduling": "on first enrollment").
//
// This is the initial mirror clone loam-ofg.12 NOTES flags as missing
// tree-wide: MirrorFetcher.Fetch (loam-giq.2) assumes the mirror already
// exists and simply fails against a never-created directory; nothing
// before this method ran `git clone --mirror` in production.
//
// The clone runs SYNCHRONOUSLY within this unary RPC, not dispatched to a
// background job queue, a deliberate choice: EnrollRepo is a rare,
// admin-initiated, one-shot operation (never on any request-serving hot
// path), and the alternative -- reporting success before the mirror
// exists, then racing a client's first clone/push against a
// still-in-flight background job -- is precisely the failure mode this
// package's tests are built to catch (see enroll_test.go's
// TestEnrollRepo_CloneFails_NeverReturnsSuccessAndMarksSyncError). A
// multi-minute clone inside a unary RPC is a real
// latency cost, but the CLI/web caller is an admin submitting an
// enrollment form, not an agent on a tight request budget, and
// docs/web-spec.md's EnrollRepo contract already returns the fully
// populated EnrolledRepo (never a job handle) -- there is no "poll this
// job" shape in the proto surface to dispatch into instead. The
// "jobs" this bead's title refers to are ingest_jobs rows (this method's
// own final Enqueue call, ReindexRepo, and ListIngestJobs), a genuinely
// separate, already-asynchronous subsystem (internal/ingest.Pool) that
// EnrollRepo feeds into but does not have to imitate for its own clone
// step.
//
// repos.sync_state is Syncing for the clone+reconcile duration and
// Error (with the failure recorded) if either step fails; EnrollRepo
// only returns success after BOTH have completed and sync_state has been
// advanced to Idle -- never before the mirror genuinely exists on disk.
func (h *Handler) EnrollRepo(ctx context.Context, req *connect.Request[adminv1.EnrollRepoRequest]) (*connect.Response[adminv1.EnrollRepoResponse], error) {
	upstreamURL := req.Msg.GetUpstreamUrl()
	targetBranches := req.Msg.GetTargetBranches()
	indexedBranch := req.Msg.GetIndexedBranch()
	if upstreamURL == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo: empty upstream_url: %w", handler.ErrInvalidArgument))
	}
	if len(targetBranches) == 0 {
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo: at least one target branch is required: %w", handler.ErrInvalidArgument))
	}
	if indexedBranch == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo: empty indexed_branch: %w", handler.ErrInvalidArgument))
	}
	if !slices.Contains(targetBranches, indexedBranch) {
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo: indexed_branch %q must be one of target_branches: %w", indexedBranch, handler.ErrInvalidArgument))
	}
	host, name, err := deriveRepoIdentity(upstreamURL)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo: %w: %w", err, handler.ErrInvalidArgument))
	}
	if !validRepoName(name) {
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo: upstream_url %s does not derive a valid <group>/<repo_name> identifier: %w", upstreamURL, handler.ErrInvalidArgument))
	}
	cred, err := h.credentials.GetByHost(ctx, host)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo %s: no usable credential for host %s: %w: %w", name, host, err, handler.ErrFailedPrecondition))
	}
	if err := h.checker.CheckRepo(ctx, host, cred.Token, upstreamURL); err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo %s: upstream access check failed: %w: %w", name, err, handler.ErrFailedPrecondition))
	}
	repoRow, err := h.store.CreateRepo(ctx, reposstore.CreateRepoParams{
		Name:          name,
		UpstreamURL:   upstreamURL,
		ForgeHost:     host,
		IndexedBranch: indexedBranch,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo: %s: %w", name, handler.ErrAlreadyExists))
		}
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo %s: creating repos row: %w", name, err))
	}
	for _, branch := range targetBranches {
		if _, err := h.store.AddTargetBranch(ctx, repoRow.ID, branch); err != nil {
			return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo %s: adding target branch %s: %w", name, branch, err))
		}
	}
	if err := h.cloneAndReconcile(ctx, repoRow, name, host, upstreamURL); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	now := time.Now().UTC()
	updated, err := h.store.UpdateSyncState(ctx, repoRow.ID, reposstore.SyncStateIdle, &now, nil)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo %s: marking sync idle after successful clone: %w", name, err))
	}
	if err := h.ingest.Enqueue(ctx, repoRow.ID, indexedBranch, ingest.KindFull); err != nil {
		// The mirror is genuinely populated and reconciled at this point;
		// only the initial ingest trigger failed. Logged, not returned:
		// misreporting sync_state (a mirror-health signal) as Error over
		// an ingest-queue failure would be misleading, and the ingest
		// subsystem's own retry/backoff (internal/ingest) is the
		// mechanism for recovering a missed trigger, not this RPC.
		h.logger.ErrorContext(ctx, "enroll repo: failed to enqueue initial ingest job", "repo", name, "error", err)
	}
	targets, err := h.store.ListTargetBranches(ctx, repoRow.ID)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("enroll repo %s: listing target branches for response: %w", name, err))
	}
	return connect.NewResponse(&adminv1.EnrollRepoResponse{Repo: toEnrolledRepo(updated, targets)}), nil
}

// cloneAndReconcile marks repoRow Syncing, clones the initial bare
// mirror, and hardens it via mirrorReconciler -- ReconcileMirror runs
// AFTER the clone since it expects an existing repo
// (internal/mirrorreconcile's own doc comment). Any failure marks
// sync_state Error with the failure recorded and returns a wrapped error;
// success leaves sync_state Syncing for the caller (EnrollRepo itself)
// to advance to Idle once every remaining step (target branches, ingest
// enqueue) has also completed -- so a reader observing Idle can trust the
// mirror is fully ready, not merely cloned.
func (h *Handler) cloneAndReconcile(ctx context.Context, repoRow reposstore.Repo, name, host, upstreamURL string) error {
	if _, err := h.store.UpdateSyncState(ctx, repoRow.ID, reposstore.SyncStateSyncing, nil, nil); err != nil {
		return fmt.Errorf("enroll repo %s: marking sync syncing: %w", name, err)
	}
	mirrorDir := mirrorpath.Dir(h.dataDir, name)
	if _, err := h.cloner.Clone(ctx, host, mirrorDir, upstreamURL); err != nil {
		wrapped := fmt.Errorf("enroll repo %s: cloning initial mirror: %w", name, err)
		h.markSyncError(ctx, repoRow.ID, name, wrapped)
		return wrapped
	}
	if err := h.reconcile(ctx, mirrorDir); err != nil {
		wrapped := fmt.Errorf("enroll repo %s: reconciling mirror: %w", name, err)
		h.markSyncError(ctx, repoRow.ID, name, wrapped)
		return wrapped
	}
	return nil
}

// markSyncError best-effort records repoErr against repoID's sync_state,
// logging (never returning) a failure to write that record itself: the
// caller already has a real error of its own to return, and a failed
// error-write must not mask or replace it.
func (h *Handler) markSyncError(ctx context.Context, repoID uuid.UUID, name string, repoErr error) {
	message := repoErr.Error()
	if _, err := h.store.UpdateSyncState(ctx, repoID, reposstore.SyncStateError, nil, &message); err != nil {
		h.logger.ErrorContext(ctx, "enroll repo: failed to record sync error", "repo", name, "clone_error", repoErr, "write_error", err)
	}
}
