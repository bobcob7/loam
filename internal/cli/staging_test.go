package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realTempDir returns a fresh temp directory with every symlink in its own
// path already resolved. On macOS t.TempDir() sits under /var/folders/...,
// and /var is itself a symlink to /private/var — so a path computed from
// t.TempDir() and a path observed from the filesystem can name the same
// directory yet compare unequal. Resolving up front means a containment
// assertion below fails only when containment actually failed, never
// because the two spellings disagreed.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// stagingWorkspace builds a workspace rooted at workspaceRoot, by pointing
// inference at a clone directory nested directly inside it (newWorkspace
// always takes the clone's parent as the workspace root).
func stagingWorkspace(workspaceRoot, agentIdentifier string) *workspace {
	cloneRoot := filepath.Join(workspaceRoot, "doc-server")
	lookup := fixedLookup(cloneRoot, "https://loam.example/git/bobcob7/doc-server.git", "wb-9c2f1a")
	return newWorkspace(cloneRoot, agentIdentifier, lookup)
}

// mkdirAllUnderStaging creates rel under <workspaceRoot>/.loam/staging and
// returns the absolute path, so a test can pre-plant filesystem shapes at
// exactly the components OpenStaging will walk.
func mkdirAllUnderStaging(t *testing.T, workspaceRoot, rel string) string {
	t.Helper()
	path := filepath.Join(workspaceRoot, loamDirName, stagingDirName, rel)
	require.NoError(t, os.MkdirAll(path, 0o755))
	return path
}

// plantSymlink creates a symlink at linkPath pointing at target, failing
// the test if it cannot — a planted symlink that silently did not exist
// would make an escape test pass for the wrong reason.
func plantSymlink(t *testing.T, target, linkPath string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(linkPath), 0o755))
	require.NoError(t, os.Symlink(target, linkPath))
	info, err := os.Lstat(linkPath)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink, "precondition: %s must actually be a symlink", linkPath)
}

// TestStagingArea_WriteReadRemove_RoundTripsInsideTheWorkspace is the
// positive control for every escape test below: with no symlink planted
// anywhere, opening the staging area creates the directory tree and a
// write lands at exactly the path stagingPath composes, owner-only.
//
// Without this, an escape test asserting only "nothing appeared outside"
// would pass just as happily against a write path that is simply broken —
// e.g. one that never creates the parent directory and therefore never
// writes anything anywhere. This test is what makes the escape tests'
// silence meaningful.
func TestStagingArea_WriteReadRemove_RoundTripsInsideTheWorkspace(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	ws := stagingWorkspace(workspaceRoot, "ada-lovelace-7-reviewer")
	area, err := ws.OpenStaging("bobcob7/doc-server", "wb-9c2f1a")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, area.Close()) })
	require.NoError(t, area.WriteFile("staged.json", []byte(`{"staged":true}`)))
	expected, err := ws.stagingPath("bobcob7/doc-server", "wb-9c2f1a")
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(expected, "staged.json"), "the write must land under the workspace's own staging tree")
	info, err := os.Stat(filepath.Join(expected, "staged.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(stagingFilePerm), info.Mode().Perm(), "staged comments are the caller's unpublished notes: owner-only")
	data, err := area.ReadFile("staged.json")
	require.NoError(t, err)
	assert.Equal(t, `{"staged":true}`, string(data))
	require.NoError(t, area.Remove("staged.json"))
	assert.NoFileExists(t, filepath.Join(expected, "staged.json"))
}

// TestStagingArea_RefusesSymlinkedIntermediateComponent is the bead's
// demonstrated bypass, as a regression test. With .loam/staging/<group>
// pre-planted as a symlink pointing outside the workspace, an ENTIRELY
// VALID key (repo "g/r", work branch "wb") passes both of the lexical
// guards — an allowlist and filepath.IsLocal, neither of which can see a
// symlink — and the mkdir-p + write a naive writer would do lands the file
// outside the workspace. Resolving through os.Root refuses the traversal
// instead.
func TestStagingArea_RefusesSymlinkedIntermediateComponent(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	outside := realTempDir(t)
	mkdirAllUnderStaging(t, workspaceRoot, ".")
	plantSymlink(t, outside, filepath.Join(workspaceRoot, loamDirName, stagingDirName, "g"))
	ws := stagingWorkspace(workspaceRoot, "ada-lovelace-7-reviewer")
	_, keyErr := ws.stagingRel("g/r", "wb")
	require.NoError(t, keyErr, "precondition: the key itself is entirely valid — both lexical layers accept it")
	area, err := ws.OpenStaging("g/r", "wb")
	require.Error(t, err, "opening a staging area whose group component symlinks out of the workspace must be refused")
	assert.ErrorIs(t, err, errStagingArea)
	assert.Nil(t, area)
	assert.NoDirExists(t, filepath.Join(outside, "r"), "the refusal must also mean nothing was created outside the workspace")
}

// TestStagingArea_RefusesSymlinkedFinalComponent covers the leaf of the
// staging key — the per-agent directory — being the symlink, rather than
// an interior one. It is a distinct case: MkdirAll's last step is a Mkdir
// on a name that already resolves to an out-of-root directory, not a
// traversal through it.
func TestStagingArea_RefusesSymlinkedFinalComponent(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	outside := realTempDir(t)
	mkdirAllUnderStaging(t, workspaceRoot, filepath.Join("bobcob7", "doc-server", "wb-9c2f1a"))
	plantSymlink(t, outside, filepath.Join(workspaceRoot, loamDirName, stagingDirName, "bobcob7", "doc-server", "wb-9c2f1a", "ada-lovelace-7-reviewer"))
	ws := stagingWorkspace(workspaceRoot, "ada-lovelace-7-reviewer")
	area, err := ws.OpenStaging("bobcob7/doc-server", "wb-9c2f1a")
	require.Error(t, err, "the per-agent leaf directory symlinking out of the workspace must be refused")
	assert.ErrorIs(t, err, errStagingArea)
	assert.Nil(t, area)
	assert.NoFileExists(t, filepath.Join(outside, "staged.json"))
}

// TestStagingArea_RefusesSymlinkedLoamDirectory covers the component above
// the staging key entirely: .loam itself pointing outside the workspace.
// The staging root is created, not assumed, so containment has to hold for
// that creation too — otherwise the entire staging tree relocates before
// any key is ever joined.
func TestStagingArea_RefusesSymlinkedLoamDirectory(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	outside := realTempDir(t)
	plantSymlink(t, outside, filepath.Join(workspaceRoot, loamDirName))
	ws := stagingWorkspace(workspaceRoot, "ada-lovelace-7-reviewer")
	area, err := ws.OpenStaging("bobcob7/doc-server", "wb-9c2f1a")
	require.Error(t, err, "a .loam directory symlinking out of the workspace must be refused")
	assert.ErrorIs(t, err, errStagingArea)
	assert.Nil(t, area)
	assert.NoDirExists(t, filepath.Join(outside, stagingDirName), "no part of the staging tree may be created outside the workspace")
}

// TestStagingArea_RefusesSymlinkedStagedFile covers the FINAL component of
// a write: the staged file name itself already exists inside a perfectly
// legitimate staging directory, as a symlink to a file outside the
// workspace. Nothing about the path OpenStaging walked was suspicious —
// only the file being written is — so this case is reachable even when
// every directory component is sound.
func TestStagingArea_RefusesSymlinkedStagedFile(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	outside := realTempDir(t)
	ws := stagingWorkspace(workspaceRoot, "ada-lovelace-7-reviewer")
	area, err := ws.OpenStaging("bobcob7/doc-server", "wb-9c2f1a")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, area.Close()) })
	areaDir, err := ws.stagingPath("bobcob7/doc-server", "wb-9c2f1a")
	require.NoError(t, err)
	loot := filepath.Join(outside, "loot.json")
	plantSymlink(t, loot, filepath.Join(areaDir, "staged.json"))
	err = area.WriteFile("staged.json", []byte("secret"))
	require.Error(t, err, "writing through a staged-file symlink that leaves the workspace must be refused")
	assert.ErrorIs(t, err, errStagingArea)
	assert.NoFileExists(t, loot, "the symlink's target outside the workspace must never be created")
}

// TestStagingArea_SymlinkPlantedAfterOpen_WriteStaysInTheRealDirectory is
// the TOCTOU case: the staging area is opened and validated, and only THEN
// is an interior component swapped for a symlink pointing outside. A path
// string captured at validation time would resolve through the new symlink
// at write time; the os.Root handle is pinned to the directory it opened,
// so the write still lands in that real directory and the attacker's
// target is never touched.
func TestStagingArea_SymlinkPlantedAfterOpen_WriteStaysInTheRealDirectory(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	outside := realTempDir(t)
	ws := stagingWorkspace(workspaceRoot, "ada-lovelace-7-reviewer")
	area, err := ws.OpenStaging("bobcob7/doc-server", "wb-9c2f1a")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, area.Close()) })
	group := filepath.Join(workspaceRoot, loamDirName, stagingDirName, "bobcob7")
	pinned := filepath.Join(workspaceRoot, loamDirName, stagingDirName, "pinned")
	require.NoError(t, os.Rename(group, pinned), "precondition: move the real tree aside so a symlink can take its name")
	plantSymlink(t, outside, group)
	// Pre-create the mirrored tree at the symlink's target so an unsafe
	// write would SUCCEED there. Without this the escape would merely fail
	// with ENOENT, and the test could not tell a contained write apart from
	// a write that was broken for an unrelated reason.
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "doc-server", "wb-9c2f1a", "ada-lovelace-7-reviewer"), 0o755))
	require.NoError(t, area.WriteFile("staged.json", []byte("secret")))
	assert.NoFileExists(t, filepath.Join(outside, "doc-server", "wb-9c2f1a", "ada-lovelace-7-reviewer", "staged.json"),
		"a symlink planted after the area was opened must not redirect the write outside the workspace")
	assert.FileExists(t, filepath.Join(pinned, "doc-server", "wb-9c2f1a", "ada-lovelace-7-reviewer", "staged.json"),
		"the write must land in the real directory the handle was pinned to")
}

// TestStagingArea_ReopeningAfterSymlinkPlanted_IsRefused is the other half
// of the TOCTOU case: a later invocation of the CLI re-opens the same
// staging area, by then poisoned. Containment is re-checked against the
// live filesystem on every open, so a key validated as safe in one process
// is not trusted as safe in the next.
func TestStagingArea_ReopeningAfterSymlinkPlanted_IsRefused(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	outside := realTempDir(t)
	ws := stagingWorkspace(workspaceRoot, "ada-lovelace-7-reviewer")
	first, err := ws.OpenStaging("bobcob7/doc-server", "wb-9c2f1a")
	require.NoError(t, err)
	require.NoError(t, first.Close())
	group := filepath.Join(workspaceRoot, loamDirName, stagingDirName, "bobcob7")
	require.NoError(t, os.RemoveAll(group))
	plantSymlink(t, outside, group)
	second, err := ws.OpenStaging("bobcob7/doc-server", "wb-9c2f1a")
	require.Error(t, err, "re-opening a staging area that has since been poisoned must be refused")
	assert.ErrorIs(t, err, errStagingArea)
	assert.Nil(t, second)
	assert.NoDirExists(t, filepath.Join(outside, "doc-server"), "nothing may be created outside the workspace on the refused re-open")
}

// TestStagingArea_RejectsEscapingEntryName proves the entry name a writer
// passes is itself constrained: a traversal or absolute name is a usage
// error (exit 2), rejected before any syscall, rather than something the
// os.Root has to catch.
func TestStagingArea_RejectsEscapingEntryName(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	outside := realTempDir(t)
	ws := stagingWorkspace(workspaceRoot, "ada-lovelace-7-reviewer")
	area, err := ws.OpenStaging("bobcob7/doc-server", "wb-9c2f1a")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, area.Close()) })
	for _, name := range []string{"", ".", "..", "../escape.json", "sub/staged.json", filepath.Join(outside, "loot.json"), "../../../../../../etc/passwd"} {
		t.Run("name="+name, func(t *testing.T) {
			t.Parallel()
			err := area.WriteFile(name, []byte("secret"))
			require.Errorf(t, err, "staged entry name %q must be rejected", name)
			assert.ErrorIs(t, err, errInvalidStagingKey)
			assert.ErrorIs(t, err, errUsage, "rejection must classify as a usage error (exit 2)")
		})
	}
	assert.NoFileExists(t, filepath.Join(outside, "loot.json"))
}

// TestStagingArea_RejectsInvalidKeysBeforeTouchingTheFilesystem proves the
// lexical guards still run first and still produce the precise usage error
// (exit 2) naming the offending key — the thing os.Root containment cannot
// do, since a syscall refusal says only "path escapes", never which
// argument was malformed.
func TestStagingArea_RejectsInvalidKeysBeforeTouchingTheFilesystem(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	ws := stagingWorkspace(workspaceRoot, "ada-lovelace-7-reviewer")
	area, err := ws.OpenStaging("../../etc", "wb-9c2f1a")
	require.Error(t, err)
	assert.ErrorIs(t, err, errInvalidStagingKey)
	assert.ErrorIs(t, err, errUsage, "an invalid key is a usage error (exit 2), not an unavailable staging area")
	assert.NotErrorIs(t, err, errStagingArea)
	assert.Nil(t, area)
	assert.NoDirExists(t, filepath.Join(workspaceRoot, loamDirName), "a rejected key must not even create the staging root")
}
