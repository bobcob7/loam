package testsched

import (
	"context"
	"errors"
	"testing"

	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIngestHarness_DrainIngestQueueDelegatesToDrainer(t *testing.T) {
	t.Parallel()
	drainer := &IngestDrainerMock{
		DrainRepoFunc: func(ctx context.Context, repo mirrorsync.RepoID) error { return nil },
	}
	h := NewIngestHarness(drainer)
	err := h.DrainIngestQueue(t.Context(), "repoA")
	require.NoError(t, err)
	require.Len(t, drainer.DrainRepoCalls(), 1)
	assert.Equal(t, mirrorsync.RepoID("repoA"), drainer.DrainRepoCalls()[0].Repo)
}

func TestIngestHarness_DrainIngestQueueWrapsDrainerError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("worker pool unreachable")
	drainer := &IngestDrainerMock{
		DrainRepoFunc: func(ctx context.Context, repo mirrorsync.RepoID) error { return wantErr },
	}
	h := NewIngestHarness(drainer)
	err := h.DrainIngestQueue(t.Context(), "repoA")
	require.ErrorIs(t, err, wantErr)
}

func TestIngestHarness_DrainIngestQueueTFailsTBOnError(t *testing.T) {
	t.Parallel()
	drainer := &IngestDrainerMock{
		DrainRepoFunc: func(ctx context.Context, repo mirrorsync.RepoID) error { return errors.New("boom") },
	}
	h := NewIngestHarness(drainer)
	stub := &fatalRecordingTB{TB: t}
	h.DrainIngestQueueT(t.Context(), stub, "repoA")
	assert.True(t, stub.fataled, "DrainIngestQueueT must fail tb when DrainRepo errors")
}

// fatalRecordingTB wraps a real testing.TB, intercepting Fatalf so a test
// can assert the *T sugar reports failure without actually failing the
// outer test.
type fatalRecordingTB struct {
	testing.TB
	fataled bool
}

func (f *fatalRecordingTB) Helper() {}

func (f *fatalRecordingTB) Fatalf(format string, args ...any) {
	f.fataled = true
}
