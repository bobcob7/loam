package mirrorpath

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDataDir_InvertsDirForATwoSegmentRepoName proves DataDir round-trips
// exactly what Dir produces, for the "<group>/<repo_name>" shape every
// enrolled repo in this tree actually has.
func TestDataDir_InvertsDirForATwoSegmentRepoName(t *testing.T) {
	t.Parallel()
	dataDir := "/var/lib/loam"
	mirrorDir := Dir(dataDir, "acme/widgets")
	got, err := DataDir(mirrorDir)
	require.NoError(t, err)
	assert.Equal(t, dataDir, got)
}

// TestDataDir_RelativeDataDirAlsoRoundTrips proves the inversion is not
// specific to an absolute dataDir -- Dir itself is a plain filepath.Join,
// and DataDir must invert whatever shape Dir actually produced.
func TestDataDir_RelativeDataDirAlsoRoundTrips(t *testing.T) {
	t.Parallel()
	dataDir := filepath.Join("relative", "data", "dir")
	mirrorDir := Dir(dataDir, "group/repo")
	got, err := DataDir(mirrorDir)
	require.NoError(t, err)
	assert.Equal(t, dataDir, got)
}

// TestDataDir_RejectsAPathNotEndingInGit proves a path that does not carry
// Dir's own ".git" suffix is rejected rather than silently misparsed.
func TestDataDir_RejectsAPathNotEndingInGit(t *testing.T) {
	t.Parallel()
	_, err := DataDir("/var/lib/loam/mirrors/acme/widgets")
	assert.Error(t, err)
}

// TestDataDir_RejectsAPathWhoseGrandparentIsNotMirrors proves DataDir does
// not blindly walk up three components regardless of what is actually
// there -- a mirror-shaped path planted somewhere else entirely (no
// "mirrors" directory in the chain) must be rejected, not silently
// misreport an unrelated ancestor as dataDir.
func TestDataDir_RejectsAPathWhoseGrandparentIsNotMirrors(t *testing.T) {
	t.Parallel()
	_, err := DataDir("/some/other/place/acme/widgets.git")
	assert.Error(t, err)
}

// TestDataDir_RejectsATooShallowPath proves a path with too few components
// to plausibly contain "<dataDir>/mirrors/<group>/<repo>.git" is rejected
// rather than returning a nonsensical root-ish directory.
func TestDataDir_RejectsATooShallowPath(t *testing.T) {
	t.Parallel()
	_, err := DataDir("/widgets.git")
	assert.Error(t, err)
}
