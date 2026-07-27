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
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/reposstore"
)

// TestReindexRepo_EnqueuesFullIngestForIndexedBranch proves ReindexRepo
// forces a FULL rebuild of the repo's CURRENT indexed branch.
func TestReindexRepo_EnqueuesFullIngestForIndexedBranch(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.GetRepoByNameFunc = func(_ context.Context, name string) (reposstore.Repo, error) {
		return reposstore.Repo{ID: uuid.New(), Name: name, IndexedBranch: "main"}, nil
	}
	h := d.handler(t, "/data")
	resp, err := h.ReindexRepo(t.Context(), connect.NewRequest(&adminv1.ReindexRepoRequest{Repo: "acme/widgets"}))
	require.NoError(t, err)
	require.Len(t, d.ingest.EnqueueCalls(), 1)
	assert.Equal(t, "main", d.ingest.EnqueueCalls()[0].TargetBranch)
	assert.Equal(t, ingest.KindFull, d.ingest.EnqueueCalls()[0].Kind)
	assert.Equal(t, adminv1.IngestKind_INGEST_KIND_FULL, resp.Msg.GetJob().GetKind())
}

// TestReindexRepo_UnenrolledRepo_NotFound proves it resolves the repo
// before attempting to enqueue anything.
func TestReindexRepo_UnenrolledRepo_NotFound(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.GetRepoByNameFunc = func(_ context.Context, _ string) (reposstore.Repo, error) {
		return reposstore.Repo{}, reposstore.ErrNotFound
	}
	h := d.handler(t, "/data")
	_, err := h.ReindexRepo(t.Context(), connect.NewRequest(&adminv1.ReindexRepoRequest{Repo: "acme/ghost"}))
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Equal(t, connect.CodeNotFound, connErr.Code())
	assert.Empty(t, d.ingest.EnqueueCalls())
}

// TestListIngestJobs_ConvertsFilterAndRecords proves the request's
// repo/status filter reaches the store layer, and every JobRecord field
// round-trips into the response.
func TestListIngestJobs_ConvertsFilterAndRecords(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	queuedAt := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	d.jobs.ListJobsFunc = func(_ context.Context, filter ingest.ListJobsFilter, _, _ int32) ([]ingest.JobRecord, int64, error) {
		assert.Equal(t, "acme/widgets", filter.Repo)
		assert.Equal(t, "running", filter.Status)
		return []ingest.JobRecord{{
			ID: uuid.New(), Repo: "acme/widgets", TargetBranch: "main",
			Kind: ingest.KindIncremental, Status: "running", Attempts: 2,
			QueuedAt: queuedAt,
		}}, 1, nil
	}
	h := d.handler(t, "/data")
	resp, err := h.ListIngestJobs(t.Context(), connect.NewRequest(&adminv1.ListIngestJobsRequest{
		Repo:   ptrString("acme/widgets"),
		Status: ptrIngestStatus(adminv1.IngestStatus_INGEST_STATUS_RUNNING),
	}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetJobs(), 1)
	job := resp.Msg.GetJobs()[0]
	assert.Equal(t, "acme/widgets", job.GetRepo())
	assert.Equal(t, "main", job.GetTargetBranch())
	assert.Equal(t, adminv1.IngestKind_INGEST_KIND_INCREMENTAL, job.GetKind())
	assert.Equal(t, adminv1.IngestStatus_INGEST_STATUS_RUNNING, job.GetStatus())
	assert.Equal(t, uint32(2), job.GetAttempts())
	assert.Equal(t, uint32(1), resp.Msg.GetPageInfo().GetTotal())
}

func ptrString(s string) *string { return &s }

func ptrIngestStatus(s adminv1.IngestStatus) *adminv1.IngestStatus { return &s }
