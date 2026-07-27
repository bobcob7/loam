package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoamhookBinaryPath_SiblingOfOwnExecutable proves the hook binary is
// resolved as a sibling of the running server's own executable -- e.g.
// "/opt/loam/loam-server" resolves "/opt/loam/loamhook" -- with no
// environment variable involved.
func TestLoamhookBinaryPath_SiblingOfOwnExecutable(t *testing.T) {
	t.Parallel()
	executable := func() (string, error) { return filepath.Join("/opt/loam", "loam-server"), nil }
	got, err := loamhookBinaryPath(executable)
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
	_, err := loamhookBinaryPath(executable)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}
