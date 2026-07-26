package testsched

import (
	"context"
	"fmt"
	"testing"

	"github.com/bobcob7/loam/internal/mirrorsync"
)

// IngestHarness backs docs/testing-spec.md's "after ingestion" step
// vocabulary entry: block until the async ingest worker pool (loam-c94.1)
// has no queued or running job left for a repo, without polling.
//
// It is a thin wrapper over IngestDrainer, matched by loam-c94.1's
// DrainRepo(ctx, name string) -- see IngestDrainer's doc comment for why
// that seam takes a plain string (repos.name) rather than
// mirrorsync.RepoID.
type IngestHarness struct {
	drainer IngestDrainer
}

// NewIngestHarness builds an IngestHarness over drainer.
func NewIngestHarness(drainer IngestDrainer) *IngestHarness {
	return &IngestHarness{drainer: drainer}
}

// DrainIngestQueue blocks until repo has no queued or running ingest job
// left, or ctx is done. repo is a mirrorsync.RepoID -- the right
// vocabulary for a godog step -- converted to IngestDrainer's plain
// string at this boundary.
func (h *IngestHarness) DrainIngestQueue(ctx context.Context, repo mirrorsync.RepoID) error {
	if err := h.drainer.DrainRepo(ctx, string(repo)); err != nil {
		return fmt.Errorf("draining ingest queue for repo %s: %w", repo, err)
	}
	return nil
}

// DrainIngestQueueT is DrainIngestQueue for callers with a testing.TB in
// scope: any failure fails tb directly instead of returning an error.
func (h *IngestHarness) DrainIngestQueueT(ctx context.Context, tb testing.TB, repo mirrorsync.RepoID) {
	tb.Helper()
	if err := h.DrainIngestQueue(ctx, repo); err != nil {
		tb.Fatalf("%v", err)
	}
}
