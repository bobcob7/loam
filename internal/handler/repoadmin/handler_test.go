package repoadmin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// testDeps bundles every moq mock Handler needs, each pre-configured with
// a harmless, fully-specified default so a test that forgets to override
// an irrelevant collaborator gets a clean, informative failure rather
// than a nil-func panic (the "incomplete mock trap" this bead's brief
// warns about) -- every mutation test below overrides only the
// collaborator(s) its scenario needs to touch, leaving the rest on these
// defaults.
type testDeps struct {
	store       *repoStoreMock
	workBranch  *workBranchListerMock
	credentials *credentialResolverMock
	checker     *upstreamCheckerMock
	cloner      *clonerMock
	reconcile   mirrorReconciler
	ingest      *ingestEnqueuerMock
	jobs        *jobListerMock
	deleter     *repoDeleterMock
	buf         bytes.Buffer
}

// newTestDeps builds a testDeps with harmless defaults: GetRepoByName
// returns a fixed repo, credential/check/clone/reconcile/enqueue all
// succeed, ListTargetBranches/List return empty, DeleteRepo succeeds.
// Individual tests override exactly the Func fields their scenario needs.
func newTestDeps() *testDeps {
	fixedRepo := reposstore.Repo{ID: uuid.New(), Name: "acme/widgets", UpstreamURL: "https://example.com/acme/widgets.git", ForgeHost: "example.com", IndexedBranch: "main", SyncState: "idle"}
	d := &testDeps{
		store: &repoStoreMock{
			CreateRepoFunc: func(_ context.Context, params reposstore.CreateRepoParams) (reposstore.Repo, error) {
				return reposstore.Repo{ID: uuid.New(), Name: params.Name, UpstreamURL: params.UpstreamURL, ForgeHost: params.ForgeHost, IndexedBranch: params.IndexedBranch, SyncState: "idle"}, nil
			},
			GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
				fixedRepo.Name = name
				return fixedRepo, nil
			},
			ListReposFunc: func(_ context.Context, _ reposstore.Page) (reposstore.ListReposResult, error) {
				return reposstore.ListReposResult{}, nil
			},
			UpdateRepoFunc: func(_ context.Context, _ uuid.UUID, params reposstore.UpdateRepoParams) (reposstore.Repo, error) {
				fixedRepo.IndexedBranch = params.IndexedBranch
				return fixedRepo, nil
			},
			UpdateSyncStateFunc: func(_ context.Context, id uuid.UUID, state reposstore.SyncState, lastSyncedAt *time.Time, syncErr *string) (reposstore.Repo, error) {
				fixedRepo.SyncState = string(state)
				fixedRepo.LastSyncedAt = lastSyncedAt
				fixedRepo.SyncError = syncErr
				return fixedRepo, nil
			},
			AddTargetBranchFunc: func(_ context.Context, repoID uuid.UUID, branch string) (reposstore.TargetBranch, error) {
				return reposstore.TargetBranch{RepoID: repoID, Branch: branch}, nil
			},
			ListTargetBranchesFunc: func(_ context.Context, _ uuid.UUID) ([]reposstore.TargetBranch, error) {
				return nil, nil
			},
			RemoveTargetBranchFunc: func(_ context.Context, _ uuid.UUID, _ string) error {
				return nil
			},
		},
		workBranch: &workBranchListerMock{
			ListFunc: func(_ context.Context, _ workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
				return nil, 0, nil
			},
		},
		credentials: &credentialResolverMock{
			GetByHostFunc: func(_ context.Context, host string) (credentialstore.Credential, error) {
				return credentialstore.Credential{Token: "tok-" + host}, nil
			},
		},
		checker: &upstreamCheckerMock{
			CheckRepoFunc: func(_ context.Context, _, _, _ string) error { return nil },
		},
		cloner: &clonerMock{
			CloneFunc: func(_ context.Context, _, _, _ string) ([]byte, error) { return nil, nil },
			LsRemoteFunc: func(_ context.Context, _, _ string) ([]byte, error) {
				return []byte("ref: refs/heads/main\tHEAD\nabc\trefs/heads/main\n"), nil
			},
		},
		reconcile: func(_ context.Context, _ string) error { return nil },
		ingest: &ingestEnqueuerMock{
			EnqueueFunc: func(_ context.Context, _ uuid.UUID, _ string, _ ingest.Kind) error { return nil },
		},
		jobs: &jobListerMock{
			ListJobsFunc: func(_ context.Context, _ ingest.ListJobsFilter, _, _ int32) ([]ingest.JobRecord, int64, error) {
				return nil, 0, nil
			},
		},
		deleter: &repoDeleterMock{
			DeleteRepoFunc: func(_ context.Context, _ uuid.UUID) error { return nil },
		},
	}
	return d
}

// handler builds a Handler over d's mocks, with dataDir as the mirror
// root and errors logging to d.buf so a test can assert on the logged
// line for unmapped errors.
func (d *testDeps) handler(t *testing.T, dataDir string) *Handler {
	t.Helper()
	mapper := handler.NewErrorMapper(slog.New(slog.NewJSONHandler(&d.buf, nil)))
	return New(dataDir, d.store, d.workBranch, d.credentials, d.checker, d.cloner, d.reconcile, d.ingest, d.jobs, d.deleter, mapper, testLogger())
}
