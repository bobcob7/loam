package repoadmin

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/credentialstore"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/reposstore"
)

func enrollReq(url string, targets []string, indexed string) *connect.Request[adminv1.EnrollRepoRequest] {
	return connect.NewRequest(&adminv1.EnrollRepoRequest{UpstreamUrl: url, TargetBranches: targets, IndexedBranch: indexed})
}

func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	return connectErr.Code()
}

// TestEnrollRepo_Success_ClonesAndReconcilesInOrderBeforeIdle is the
// acceptance-critical happy path: the mirror is actually cloned, then
// reconciled, then and only then does sync_state advance to Idle and the
// RPC return success -- proving EnrollRepo never reports enrolled before
// the mirror genuinely exists.
func TestEnrollRepo_Success_ClonesAndReconcilesInOrderBeforeIdle(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	var order []string
	var syncStates []string
	d.cloner.CloneFunc = func(_ context.Context, host, mirrorDir, upstreamURL string) ([]byte, error) {
		order = append(order, "clone")
		assert.Equal(t, "example.com", host)
		assert.Contains(t, mirrorDir, "acme/widgets.git")
		assert.Equal(t, "https://example.com/acme/widgets.git", upstreamURL)
		return nil, nil
	}
	d.reconcile = func(_ context.Context, repoPath string) error {
		order = append(order, "reconcile")
		assert.Contains(t, repoPath, "acme/widgets.git")
		return nil
	}
	baseUpdateSyncState := d.store.UpdateSyncStateFunc
	d.store.UpdateSyncStateFunc = func(ctx context.Context, id uuid.UUID, state reposstore.SyncState, lastSyncedAt *time.Time, syncErr *string) (reposstore.Repo, error) {
		syncStates = append(syncStates, string(state))
		return baseUpdateSyncState(ctx, id, state, lastSyncedAt, syncErr)
	}
	h := d.handler(t, "/data")
	resp, err := h.EnrollRepo(t.Context(), enrollReq("https://example.com/acme/widgets.git", []string{"main"}, "main"))
	require.NoError(t, err)
	assert.Equal(t, []string{"clone", "reconcile"}, order, "clone must happen, then reconcile -- never reconcile-before-clone or clone skipped")
	assert.Equal(t, []string{"syncing", "idle"}, syncStates, "sync_state must go syncing -> idle, in that order, around the clone+reconcile")
	assert.Equal(t, "acme/widgets", resp.Msg.GetRepo().GetRepo())
	require.Len(t, d.ingest.EnqueueCalls(), 1)
	assert.Equal(t, "main", d.ingest.EnqueueCalls()[0].TargetBranch)
}

// TestEnrollRepo_PlaintextHTTPForge_UsesSchemeQualifiedHostThroughout is
// the loam-4kz regression at EnrollRepo's own boundary: for a plain-HTTP
// upstream, every collaborator EnrollRepo hands a "host" string to --
// credential resolution, the upstream access check, the clone, and the
// persisted repos row -- must see the SAME scheme-qualified
// "http://host:port" form, not the bare "host:port" deriveRepoIdentity
// produced before this fix (which forge.Forgejo's apiBaseURL would have
// dialled over https, at a listener that never speaks TLS). Asserting
// this at all four call sites is deliberate: any one of them silently
// reverting to the bare host would resurrect the bug for that one
// collaborator even with the others fixed.
func TestEnrollRepo_PlaintextHTTPForge_UsesSchemeQualifiedHostThroughout(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	const wantHost = "http://127.0.0.1:13030"
	var sawCheckerHost, sawCloneHost, sawCredentialHost string
	d.credentials.GetByHostFunc = func(_ context.Context, host string) (credentialstore.Credential, error) {
		sawCredentialHost = host
		return credentialstore.Credential{Token: "tok"}, nil
	}
	d.checker.CheckRepoFunc = func(_ context.Context, host, _, _ string) error {
		sawCheckerHost = host
		return nil
	}
	d.cloner.CloneFunc = func(_ context.Context, host, _, _ string) ([]byte, error) {
		sawCloneHost = host
		return nil, nil
	}
	var sawCreateRepoHost string
	d.store.CreateRepoFunc = func(_ context.Context, params reposstore.CreateRepoParams) (reposstore.Repo, error) {
		sawCreateRepoHost = params.ForgeHost
		return reposstore.Repo{ID: uuid.New(), Name: params.Name, UpstreamURL: params.UpstreamURL, ForgeHost: params.ForgeHost, IndexedBranch: params.IndexedBranch, SyncState: "idle"}, nil
	}
	h := d.handler(t, "/data")
	_, err := h.EnrollRepo(t.Context(), enrollReq("http://127.0.0.1:13030/e2eadmin/e2e-repo.git", []string{"main"}, "main"))
	require.NoError(t, err)
	assert.Equal(t, wantHost, sawCredentialHost, "credential resolution (GetByHost) must use the scheme-qualified host")
	assert.Equal(t, wantHost, sawCheckerHost, "the upstream access check (CheckRepo) must use the scheme-qualified host")
	assert.Equal(t, wantHost, sawCloneHost, "the initial clone must use the scheme-qualified host")
	assert.Equal(t, wantHost, sawCreateRepoHost, "the persisted repos.forge_host must be scheme-qualified, so a later sync/PR call resolves the same credential")
}

// TestEnrollRepo_InvalidDerivedRepoName_Rejected is the repo-name
// validation mutation: an upstream_url whose path does not derive a
// valid two-segment "<group>/<repo_name>" identifier (loam-ofg.16's
// review: repos.name has no DB CHECK constraint, so this handler is the
// only write-path guard) must be rejected before any store call, not
// silently accepted or sanitized.
func TestEnrollRepo_InvalidDerivedRepoName_Rejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
	}{
		{"single segment, no group", "https://example.com/widgets"},
		{"traversal segment", "https://example.com/acme/../widgets"},
		{"empty segment from doubled slash", "https://example.com/acme//widgets"},
		{"three segments", "https://example.com/acme/sub/widgets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			h := d.handler(t, "/data")
			_, err := h.EnrollRepo(t.Context(), enrollReq(tc.url, []string{"main"}, "main"))
			require.Error(t, err)
			assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
			assert.Empty(t, d.store.CreateRepoCalls(), "an invalid derived name must never reach CreateRepo")
		})
	}
}

// TestEnrollRepo_UpstreamURLHasUserinfo_RejectedBeforeCredentialCheckOrClone
// is loam-ra1k's fail-fast half at EnrollRepo's own entry point: an
// upstream URL carrying embedded credentials (user:token@host, or the
// password-less PAT form "https://<token>@host/path") must be rejected as
// InvalidArgument before EnrollRepo resolves a credential, runs CheckRepo,
// or creates the repos row -- transport-level rejection (loam-ys1) is
// necessary but not sufficient, since EnrollRepo would otherwise persist
// UpstreamURL verbatim (CreateRepoParams) and %w-wrap the raw URL straight
// into the RPC error it hands back. The embedded credential must never
// appear in the returned error's message, in either form.
func TestEnrollRepo_UpstreamURLHasUserinfo_RejectedBeforeCredentialCheckOrClone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		upstreamURL string
	}{
		{"username and password", "https://user:leaked-token@example.com/acme/widgets.git"},
		{"username only, no password (PAT form)", "https://leaked-token@example.com/acme/widgets.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			h := d.handler(t, "/data")
			_, err := h.EnrollRepo(t.Context(), enrollReq(tt.upstreamURL, []string{"main"}, "main"))
			require.Error(t, err)
			assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
			assert.NotContains(t, err.Error(), "leaked-token", "the rejected URL's embedded credential must never appear in the returned error")
			assert.Empty(t, d.credentials.GetByHostCalls(), "no credential should be resolved for a URL rejected before host derivation")
			assert.Empty(t, d.checker.CheckRepoCalls(), "the upstream access check must never run for a URL rejected before it")
			assert.Empty(t, d.store.CreateRepoCalls(), "a rejected URL must never reach CreateRepo, let alone persist the credential")
		})
	}
}

// TestEnrollRepo_IndexedBranchNotInTargetBranches_Rejected proves the
// "indexed_branch must be one of target_branches" invariant
// (docs/web-spec.md -> RepoAdminService EnrollRepo) before any store call.
func TestEnrollRepo_IndexedBranchNotInTargetBranches_Rejected(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	h := d.handler(t, "/data")
	_, err := h.EnrollRepo(t.Context(), enrollReq("https://example.com/acme/widgets.git", []string{"main", "release"}, "docs"))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
	assert.Empty(t, d.store.CreateRepoCalls())
}

// TestEnrollRepo_MissingCredential_FailedPrecondition proves EnrollRepo
// refuses to attempt a clone at all when the URL's host has no usable
// credential -- CheckRepo/Clone must never run.
func TestEnrollRepo_MissingCredential_FailedPrecondition(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.credentials.GetByHostFunc = func(_ context.Context, _ string) (credentialstore.Credential, error) {
		return credentialstore.Credential{}, errors.New("no such host")
	}
	h := d.handler(t, "/data")
	_, err := h.EnrollRepo(t.Context(), enrollReq("https://example.com/acme/widgets.git", []string{"main"}, "main"))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
	assert.Empty(t, d.checker.CheckRepoCalls())
	assert.Empty(t, d.cloner.CloneCalls())
	assert.Empty(t, d.store.CreateRepoCalls())
}

// TestEnrollRepo_CheckRepoFails_FailedPreconditionAndNoClone proves the
// authoritative CheckRepo gate (docs/web-spec.md: "EnrollRepo's CheckRepo
// (read + write probes) remains the authoritative gate") actually blocks
// the clone, rather than being consulted and ignored.
func TestEnrollRepo_CheckRepoFails_FailedPreconditionAndNoClone(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.checker.CheckRepoFunc = func(_ context.Context, _, _, _ string) error {
		return errors.New("no write access")
	}
	h := d.handler(t, "/data")
	_, err := h.EnrollRepo(t.Context(), enrollReq("https://example.com/acme/widgets.git", []string{"main"}, "main"))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
	assert.Empty(t, d.cloner.CloneCalls(), "a failed CheckRepo must prevent the clone from ever running")
	assert.Empty(t, d.store.CreateRepoCalls())
}

// TestEnrollRepo_DuplicateName_AlreadyExists proves a name collision
// (repos_name_key unique violation) maps to CodeAlreadyExists, not a
// generic internal error.
func TestEnrollRepo_DuplicateName_AlreadyExists(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.CreateRepoFunc = func(_ context.Context, _ reposstore.CreateRepoParams) (reposstore.Repo, error) {
		return reposstore.Repo{}, &pgconn.PgError{Code: pgUniqueViolationCode}
	}
	h := d.handler(t, "/data")
	_, err := h.EnrollRepo(t.Context(), enrollReq("https://example.com/acme/widgets.git", []string{"main"}, "main"))
	require.Error(t, err)
	assert.Equal(t, connect.CodeAlreadyExists, connectCode(t, err))
	assert.Empty(t, d.cloner.CloneCalls())
}

// TestEnrollRepo_CloneFails_NeverReturnsSuccessAndMarksSyncError is the
// central mutation-kill for "report enrolled before the mirror exists"
// and "leave sync_state at its initial value on failure": a failing
// clone must (1) make EnrollRepo return an error, never a success
// response, (2) never call ReconcileMirror, (3) never advance sync_state
// to Idle, and (4) leave the row recorded as Error, not stuck on its
// initial Syncing value.
func TestEnrollRepo_CloneFails_NeverReturnsSuccessAndMarksSyncError(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	cloneErr := errors.New("git clone: exit status 128")
	d.cloner.CloneFunc = func(context.Context, string, string, string) ([]byte, error) { return nil, cloneErr }
	reconcileCalled := false
	d.reconcile = func(context.Context, string) error { reconcileCalled = true; return nil }
	var finalState reposstore.SyncState
	var finalSyncErr *string
	baseFunc := d.store.UpdateSyncStateFunc
	d.store.UpdateSyncStateFunc = func(ctx context.Context, id uuid.UUID, state reposstore.SyncState, lastSyncedAt *time.Time, syncErr *string) (reposstore.Repo, error) {
		finalState = state
		finalSyncErr = syncErr
		return baseFunc(ctx, id, state, lastSyncedAt, syncErr)
	}
	h := d.handler(t, "/data")
	resp, err := h.EnrollRepo(t.Context(), enrollReq("https://example.com/acme/widgets.git", []string{"main"}, "main"))
	require.Error(t, err, "a failed clone must never report enrollment success")
	assert.Nil(t, resp)
	assert.False(t, reconcileCalled, "ReconcileMirror must never run after a failed clone")
	assert.Equal(t, reposstore.SyncStateError, finalState, "sync_state must end at Error, not left at its initial Syncing value nor advanced to Idle")
	require.NotNil(t, finalSyncErr)
	assert.Contains(t, *finalSyncErr, cloneErr.Error())
	assert.Empty(t, d.ingest.EnqueueCalls(), "no ingest job should be enqueued for a repo whose mirror never landed")
}

// TestEnrollRepo_ReconcileFails_NeverReturnsSuccessAndMarksSyncError is
// the "skip ReconcileMirror after cloning" mutation's counterpart on the
// failure side: even though the clone itself succeeded, a failing
// reconcile must still block success and still end at sync_state=Error,
// not Idle.
func TestEnrollRepo_ReconcileFails_NeverReturnsSuccessAndMarksSyncError(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	reconcileErr := errors.New("reconcile: not a bare repository")
	d.reconcile = func(context.Context, string) error { return reconcileErr }
	var finalState reposstore.SyncState
	baseFunc := d.store.UpdateSyncStateFunc
	d.store.UpdateSyncStateFunc = func(ctx context.Context, id uuid.UUID, state reposstore.SyncState, lastSyncedAt *time.Time, syncErr *string) (reposstore.Repo, error) {
		finalState = state
		return baseFunc(ctx, id, state, lastSyncedAt, syncErr)
	}
	h := d.handler(t, "/data")
	resp, err := h.EnrollRepo(t.Context(), enrollReq("https://example.com/acme/widgets.git", []string{"main"}, "main"))
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, reposstore.SyncStateError, finalState)
	require.Len(t, d.cloner.CloneCalls(), 1, "the clone itself must still have run before reconcile")
}

// TestEnrollRepo_ReconcileCalledExactlyOnceWithClonedMirrorPath is the
// direct "skip ReconcileMirror after cloning" mutation-kill on the
// success path: ReconcileMirror must be called exactly once, against the
// same path the clone just populated.
func TestEnrollRepo_ReconcileCalledExactlyOnceWithClonedMirrorPath(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	var clonedPath string
	d.cloner.CloneFunc = func(_ context.Context, _, mirrorDir, _ string) ([]byte, error) {
		clonedPath = mirrorDir
		return nil, nil
	}
	reconcileCalls := 0
	var reconciledPath string
	d.reconcile = func(_ context.Context, repoPath string) error {
		reconcileCalls++
		reconciledPath = repoPath
		return nil
	}
	h := d.handler(t, "/data")
	_, err := h.EnrollRepo(t.Context(), enrollReq("https://example.com/acme/widgets.git", []string{"main"}, "main"))
	require.NoError(t, err)
	assert.Equal(t, 1, reconcileCalls, "ReconcileMirror must run exactly once per enrollment")
	assert.Equal(t, clonedPath, reconciledPath, "reconcile must run against the exact path the clone just populated")
}
