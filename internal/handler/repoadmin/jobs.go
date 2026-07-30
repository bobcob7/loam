package repoadmin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/reposstore"
)

// ReindexRepo forces a full rebuild of repo's derived indexes by
// enqueuing a FULL ingest job for its indexed branch
// (docs/web-spec.md -> RepoAdminService "ReindexRepo": "force a full
// rebuild of the repo's derived indexes"; docs/ingestion-spec.md's
// "Incremental Build" -> "Full rebuild" describes what that job kind
// does once claimed).
func (h *Handler) ReindexRepo(ctx context.Context, req *connect.Request[adminv1.ReindexRepoRequest]) (*connect.Response[adminv1.ReindexRepoResponse], error) {
	name := req.Msg.GetRepo()
	if name == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("reindex repo: empty repo identifier: %w", handler.ErrInvalidArgument))
	}
	repoRow, err := h.store.GetRepoByName(ctx, name)
	if err != nil {
		if errors.Is(err, reposstore.ErrNotFound) {
			return nil, h.errors.ToConnectErr(fmt.Errorf("repo %s: %w", name, handler.ErrNotFound))
		}
		return nil, h.errors.ToConnectErr(fmt.Errorf("resolving repo %s: %w", name, err))
	}
	if err := h.ingest.Enqueue(ctx, repoRow.ID, repoRow.IndexedBranch, ingest.KindFull); err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("reindex repo %s: enqueuing full ingest job: %w", name, err))
	}
	return connect.NewResponse(&adminv1.ReindexRepoResponse{
		Job: &adminv1.IngestJob{
			Repo:         name,
			TargetBranch: repoRow.IndexedBranch,
			Kind:         adminv1.IngestKind_INGEST_KIND_FULL,
			Status:       adminv1.IngestStatus_INGEST_STATUS_QUEUED,
		},
	}), nil
}

// ListIngestJobs lists recent and active ingest jobs, optionally scoped
// to one repo and/or one status (docs/web-spec.md -> RepoAdminService
// "ListIngestJobs"), backing the web Jobs view.
func (h *Handler) ListIngestJobs(ctx context.Context, req *connect.Request[adminv1.ListIngestJobsRequest]) (*connect.Response[adminv1.ListIngestJobsResponse], error) {
	limit, offset := pageParams(req.Msg.GetPage())
	filter := ingest.ListJobsFilter{
		Repo:   req.Msg.GetRepo(),
		Status: ingestStatusToStoreString(req.Msg.GetStatus()),
	}
	records, total, err := h.jobs.ListJobs(ctx, filter, limit, offset)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("listing ingest jobs: %w", err))
	}
	jobs := make([]*adminv1.IngestJob, len(records))
	for i, record := range records {
		jobs[i] = toIngestJobProto(record)
	}
	return connect.NewResponse(&adminv1.ListIngestJobsResponse{
		Jobs:     jobs,
		PageInfo: &loamv1.PageInfo{Total: uint32(total)},
	}), nil
}

// ingestStatusToStoreString converts the optional proto IngestStatus
// filter to the plain string ingest.ListJobsFilter/ingest_jobs.status
// use, "" (no filter) for the zero/unspecified value.
func ingestStatusToStoreString(s adminv1.IngestStatus) string {
	switch s {
	case adminv1.IngestStatus_INGEST_STATUS_QUEUED:
		return "queued"
	case adminv1.IngestStatus_INGEST_STATUS_RUNNING:
		return "running"
	case adminv1.IngestStatus_INGEST_STATUS_SUCCEEDED:
		return "succeeded"
	case adminv1.IngestStatus_INGEST_STATUS_FAILED:
		return "failed"
	default:
		return ""
	}
}

// ingestKindToProto converts ingest.Kind to its proto enum.
func ingestKindToProto(k ingest.Kind) adminv1.IngestKind {
	switch k {
	case ingest.KindIncremental:
		return adminv1.IngestKind_INGEST_KIND_INCREMENTAL
	case ingest.KindFull:
		return adminv1.IngestKind_INGEST_KIND_FULL
	default:
		return adminv1.IngestKind_INGEST_KIND_UNSPECIFIED
	}
}

// ingestStatusToProto converts ingest_jobs.status's plain string to its
// proto enum.
func ingestStatusToProto(status string) adminv1.IngestStatus {
	switch status {
	case "queued":
		return adminv1.IngestStatus_INGEST_STATUS_QUEUED
	case "running":
		return adminv1.IngestStatus_INGEST_STATUS_RUNNING
	case "succeeded":
		return adminv1.IngestStatus_INGEST_STATUS_SUCCEEDED
	case "failed":
		return adminv1.IngestStatus_INGEST_STATUS_FAILED
	default:
		return adminv1.IngestStatus_INGEST_STATUS_UNSPECIFIED
	}
}

// toIngestJobProto converts an ingest.JobRecord to the admin API's
// IngestJob message.
func toIngestJobProto(record ingest.JobRecord) *adminv1.IngestJob {
	return &adminv1.IngestJob{
		Id:           record.ID.String(),
		Repo:         record.Repo,
		TargetBranch: record.TargetBranch,
		Kind:         ingestKindToProto(record.Kind),
		Status:       ingestStatusToProto(record.Status),
		Attempts:     uint32(record.Attempts),
		Error:        record.Error,
		QueuedAt:     record.QueuedAt.Format(time.RFC3339),
		StartedAt:    timeOrEmpty(record.StartedAt),
		FinishedAt:   timeOrEmpty(record.FinishedAt),
	}
}
