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
// It is a thin wrapper over IngestDrainer, which nothing in the tree
// implements yet -- see this package's doc comment and the
// implementation report for the seam being requested from loam-c94.1.
// Once that seam lands, wire its concrete pool type (or a small adapter
// over it) in as the drainer here.
type IngestHarness struct {
	drainer IngestDrainer
}

// NewIngestHarness builds an IngestHarness over drainer.
func NewIngestHarness(drainer IngestDrainer) *IngestHarness {
	return &IngestHarness{drainer: drainer}
}

// DrainIngestQueue blocks until repo has no queued or running ingest job
// left, or ctx is done.
func (h *IngestHarness) DrainIngestQueue(ctx context.Context, repo mirrorsync.RepoID) error {
	if err := h.drainer.DrainRepo(ctx, repo); err != nil {
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
