package repoadmin

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/reposstore"
)

// TestGetRepo_ReportsSyncStatusAndIngestedRef proves GetRepo's richer
// admin response includes sync status and the indexed branch's last
// ingested ref (docs/web-spec.md -> RepoAdminService "EnrolledRepo").
func TestGetRepo_ReportsSyncStatusAndIngestedRef(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	syncedAt := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	d.store.GetRepoByNameFunc = func(_ context.Context, name string) (reposstore.Repo, error) {
		return reposstore.Repo{ID: uuid.New(), Name: name, UpstreamURL: "https://example.com/acme/widgets.git", IndexedBranch: "main", SyncState: "idle", LastSyncedAt: &syncedAt}, nil
	}
	d.store.ListTargetBranchesFunc = func(_ context.Context, _ uuid.UUID) ([]reposstore.TargetBranch, error) {
		return []reposstore.TargetBranch{
			{Branch: "main", IngestedRef: reposstore.IngestedRef{Ref: "deadbeef", Ok: true}},
			{Branch: "release"},
		}, nil
	}
	h := d.handler(t, "/data")
	resp, err := h.GetRepo(t.Context(), connect.NewRequest(&adminv1.GetRepoRequest{Repo: "acme/widgets"}))
	require.NoError(t, err)
	got := resp.Msg.GetRepo()
	assert.Equal(t, "acme/widgets", got.GetRepo())
	assert.Equal(t, []string{"main", "release"}, got.GetTargetBranches())
	assert.Equal(t, "deadbeef", got.GetIngestedRef())
	assert.Equal(t, adminv1.SyncState_SYNC_STATE_IDLE, got.GetSync().GetState())
	assert.Equal(t, syncedAt.Format(time.RFC3339), got.GetSync().GetLastSyncedAt())
}

// TestGetRepo_UnenrolledRepo_NotFound mirrors loam.v1.RepoService's own
// acceptance-critical proof for the admin surface.
func TestGetRepo_UnenrolledRepo_NotFound(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.GetRepoByNameFunc = func(_ context.Context, name string) (reposstore.Repo, error) {
		return reposstore.Repo{}, reposstore.ErrNotFound
	}
	h := d.handler(t, "/data")
	_, err := h.GetRepo(t.Context(), connect.NewRequest(&adminv1.GetRepoRequest{Repo: "acme/ghost"}))
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Equal(t, connect.CodeNotFound, connErr.Code())
}

// TestListRepos_ReturnsPageInfoTotal proves ListRepos surfaces the
// store's total count for pagination, not just the returned page's
// length.
func TestListRepos_ReturnsPageInfoTotal(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.ListReposFunc = func(_ context.Context, page reposstore.Page) (reposstore.ListReposResult, error) {
		return reposstore.ListReposResult{
			Repos: []reposstore.Repo{{ID: uuid.New(), Name: "acme/one"}, {ID: uuid.New(), Name: "acme/two"}},
			Total: 5,
		}, nil
	}
	h := d.handler(t, "/data")
	resp, err := h.ListRepos(t.Context(), connect.NewRequest(&adminv1.ListReposRequest{}))
	require.NoError(t, err)
	assert.Len(t, resp.Msg.GetRepos(), 2)
	assert.Equal(t, uint32(5), resp.Msg.GetPageInfo().GetTotal())
}
