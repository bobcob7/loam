package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedStat builds a stat func returning err for every path -- nil means
// "the file exists" (fixedStatOK below is the shorthand for that).
func fixedStat(err error) func(string) (os.FileInfo, error) {
	return func(string) (os.FileInfo, error) { return nil, err }
}

// TestLoamhookBinaryPath_SiblingOfOwnExecutable proves the hook binary is
// resolved as a sibling of the running server's own executable -- e.g.
// "/opt/loam/loam-server" resolves "/opt/loam/loamhook" -- with no
// environment variable involved, when the resolved path actually exists.
func TestLoamhookBinaryPath_SiblingOfOwnExecutable(t *testing.T) {
	t.Parallel()
	executable := func() (string, error) { return filepath.Join("/opt/loam", "loam-server"), nil }
	got, err := loamhookBinaryPath(executable, fixedStat(nil))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/opt/loam", "loamhook"), got)
}

// TestLoamhookBinaryPath_ExecutableLookupFailurePropagates proves a
// failure resolving this process's own executable path (os.Executable
// can fail, e.g. under an unusual sandboxing setup) is reported rather
// than silently producing a bogus path.
func TestLoamhookBinaryPath_ExecutableLookupFailurePropagates(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("os.Executable: not supported")
	executable := func() (string, error) { return "", wantErr }
	_, err := loamhookBinaryPath(executable, fixedStat(nil))
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

// TestLoamhookBinaryPath_MissingSiblingIsAHardErrorEvenWithNoReposEnrolled
// is the MUST-fix a review of this bead caught: a missing "loamhook"
// sibling binary must abort startup UNCONDITIONALLY -- not only be
// discovered later, per-repo, by mirrorreconcile.ReconcileMirror, which
// never even reads this path for a mirror that is not yet cloned (a
// legitimate no-op on its own terms). Without this stat, a fresh install
// with zero enrolled repos would start up cleanly with no hook binary
// present at all and say nothing.
func TestLoamhookBinaryPath_MissingSiblingIsAHardErrorEvenWithNoReposEnrolled(t *testing.T) {
	t.Parallel()
	executable := func() (string, error) { return filepath.Join("/opt/loam", "loam-server"), nil }
	statErr := os.ErrNotExist
	_, err := loamhookBinaryPath(executable, fixedStat(statErr))
	require.Error(t, err, "a missing loamhook sibling must fail startup even when no repo is enrolled yet")
	assert.ErrorIs(t, err, statErr)
	assert.Contains(t, err.Error(), "not found", "the one stat failure that genuinely means the file is absent should say so")
}

// TestLoamhookBinaryPath_UnstatableSiblingIsNotReportedAsMissing is the
// same distinction from the other side. stat can fail without the file
// being absent -- a directory component this process cannot traverse, a
// symlink loop, an I/O error, a dead network mount -- and every one of
// those used to be reported as "loamhook binary not found", sending an
// operator to install a binary that is already sitting there. Startup must
// still fail; it just must not name a cause it did not observe.
func TestLoamhookBinaryPath_UnstatableSiblingIsNotReportedAsMissing(t *testing.T) {
	t.Parallel()
	executable := func() (string, error) { return filepath.Join("/opt/loam", "loam-server"), nil }
	statErr := os.ErrPermission
	_, err := loamhookBinaryPath(executable, fixedStat(statErr))
	require.Error(t, err, "an unstatable path is still a startup failure: this process cannot show the hook binary is there")
	assert.ErrorIs(t, err, statErr)
	assert.NotContains(t, err.Error(), "not found",
		"stat failing for a reason other than absence is not evidence the file is absent")
	assert.Contains(t, err.Error(), filepath.Join("/opt/loam", "loamhook"), "the operator still needs the path that could not be checked")
}
