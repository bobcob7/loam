package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingReconciler builds a mirrorReconcilerFunc that records every
// (repoPath, hookBinaryPath) pair it was called with, thread-safely, and
// returns err verbatim -- so a test can prove which paths reconcileMirrors
// dispatched to without touching real git.
func recordingReconciler(err error) (fn mirrorReconcilerFunc, calls func() []string, hookBinaryPaths func() []string) {
	var mu sync.Mutex
	var paths []string
	var hookPaths []string
	fn = func(_ context.Context, repoPath, hookBinaryPath string) error {
		mu.Lock()
		paths = append(paths, repoPath)
		hookPaths = append(hookPaths, hookBinaryPath)
		mu.Unlock()
		return err
	}
	calls = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
	hookBinaryPaths = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), hookPaths...)
	}
	return fn, calls, hookBinaryPaths
}

// TestReconcileMirrors_ReconcilesEveryEnrolledRepoAtItsDerivedMirrorPath
// proves reconcileMirrors lists every enrolled repo and dispatches each one
// to the reconciler at the path docs/server-spec.md's LOAM_DATA_DIR row
// pins: "<dir>/mirrors/<group>/<repo_name>.git".
func TestReconcileMirrors_ReconcilesEveryEnrolledRepoAtItsDerivedMirrorPath(t *testing.T) {
	t.Parallel()
	lister := &repoNameListerMock{
		ListAllRepoNamesFunc: func(context.Context) ([]string, error) {
			return []string{"acme/widgets", "acme/gadgets"}, nil
		},
	}
	reconcile, calls, hookBinaryPaths := recordingReconciler(nil)
	err := reconcileMirrors(t.Context(), testLogger(), "/data", "/data/loamhook", lister, reconcile)
	require.NoError(t, err)
	assert.Equal(t, []string{
		filepath.Join("/data", "mirrors", "acme/widgets.git"),
		filepath.Join("/data", "mirrors", "acme/gadgets.git"),
	}, calls())
	assert.Equal(t, []string{"/data/loamhook", "/data/loamhook"}, hookBinaryPaths(), "the same hook binary path must be threaded to every reconcile call")
}

// TestReconcileMirrors_NoEnrolledReposCallsReconcilerZeroTimes proves an
// empty enrollment is a legitimate no-op, not an error.
func TestReconcileMirrors_NoEnrolledReposCallsReconcilerZeroTimes(t *testing.T) {
	t.Parallel()
	lister := &repoNameListerMock{
		ListAllRepoNamesFunc: func(context.Context) ([]string, error) { return nil, nil },
	}
	reconcile, calls, _ := recordingReconciler(nil)
	err := reconcileMirrors(t.Context(), testLogger(), "/data", "/data/loamhook", lister, reconcile)
	require.NoError(t, err)
	assert.Empty(t, calls())
}

// TestReconcileMirrors_ListingFailurePropagatesAndReconcilerNeverRuns
// proves a store error aborts before any reconcile call, rather than
// running with a partial or empty enrollment list.
func TestReconcileMirrors_ListingFailurePropagatesAndReconcilerNeverRuns(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("database unreachable")
	lister := &repoNameListerMock{
		ListAllRepoNamesFunc: func(context.Context) ([]string, error) { return nil, wantErr },
	}
	reconcile, calls, _ := recordingReconciler(nil)
	err := reconcileMirrors(t.Context(), testLogger(), "/data", "/data/loamhook", lister, reconcile)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Empty(t, calls(), "reconcile must never run once listing enrolled repos has failed")
}

// TestReconcileMirrors_PerRepoFailureAbortsTheLoop proves a real
// reconciliation error (as opposed to mirrorreconcile.ReconcileMirror's
// own "missing mirror" nil case) stops the loop immediately -- matching
// docs/server-spec.md Startup's "failing fast at each step" -- rather than
// silently continuing past a broken mirror.
func TestReconcileMirrors_PerRepoFailureAbortsTheLoop(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("permission denied")
	lister := &repoNameListerMock{
		ListAllRepoNamesFunc: func(context.Context) ([]string, error) {
			return []string{"acme/first", "acme/second"}, nil
		},
	}
	reconcile, calls, _ := recordingReconciler(wantErr)
	err := reconcileMirrors(t.Context(), testLogger(), "/data", "/data/loamhook", lister, reconcile)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, []string{filepath.Join("/data", "mirrors", "acme/first.git")}, calls(), "the loop must stop at the first failing repo, never reaching the second")
}

// TestMirrorPath_JoinsDataDirMirrorsAndRepoNameWithGitSuffix pins
// docs/server-spec.md's exact path convention against a regression: "bare
// mirrors under <dir>/mirrors/<group>/<repo_name>.git".
func TestMirrorPath_JoinsDataDirMirrorsAndRepoNameWithGitSuffix(t *testing.T) {
	t.Parallel()
	got := mirrorPath("/var/lib/loam", "acme/widgets")
	assert.Equal(t, filepath.Join("/var/lib/loam", "mirrors", "acme/widgets.git"), got)
}
