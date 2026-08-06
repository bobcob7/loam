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

// stagingWorkspace builds a workspace whose staging tree lives under
// workspaceRoot, with inference pointed at a clone nested inside it. The
// root is passed explicitly because that is now how it is decided
// everywhere (loam-rgyg): it is configuration, not a function of where the
// caller happens to be standing.
func stagingWorkspace(workspaceRoot, agentIdentifier string) *workspace {
	cloneRoot := filepath.Join(workspaceRoot, "doc-server")
	lookup := fixedLookup(cloneRoot, "https://loam.example/git/bobcob7/doc-server.git", "wb-9c2f1a")
	return newWorkspace(cloneRoot, agentIdentifier, workspaceRoot, lookup)
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

// --- carrying staged comments over from the old, cwd-derived location (loam-rgyg) ---

// legacyStagingWorkspace builds a workspace whose staging tree lives under
// root, with inference pointed at a clone nested inside legacyRoot — the
// two directories the fix separates. Before it, the second one WAS the
// staging root; after it, the first one is, and the second is only where a
// pre-upgrade staged.json may still be sitting.
func legacyStagingWorkspace(root, legacyRoot, agentIdentifier string) *workspace {
	cloneRoot := filepath.Join(legacyRoot, "doc-server")
	lookup := fixedLookup(cloneRoot, "https://loam.example/git/bobcob7/doc-server.git", "wb-9c2f1a")
	return newWorkspace(cloneRoot, agentIdentifier, root, lookup)
}

// writeLegacyStaged plants a staged.json where the pre-fix, cwd-derived
// staging root would have put it.
func writeLegacyStaged(t *testing.T, legacyRoot, agent, contents string) {
	t.Helper()
	dir := filepath.Join(legacyRoot, loamDirName, stagingDirName, testRepo, testWorkBranch, agent)
	require.NoError(t, os.MkdirAll(dir, stagingDirPerm))
	require.NoError(t, os.WriteFile(filepath.Join(dir, stagedFileName), []byte(contents), stagingFilePerm))
}

// TestOpenStaging_AdoptsStagedCommentsLeftAtThePreviousLocation proves the
// fix does not itself do what the bug does. Moving where staged comments
// live would otherwise strand every comment staged before the upgrade: the
// reviewer's next `work verdict` would find an empty area and publish an
// outcome with none of their findings attached — the precise failure this
// bead is about, reintroduced by its own fix.
//
// THREE items, with distinct ids and bodies, and next_id carried across:
// with one item a test cannot distinguish "adopted the batch" from
// "adopted an item", and a counter that silently rewound to s1 is the
// symptom by which a second reviewer detected this class of loss at all.
func TestOpenStaging_AdoptsStagedCommentsLeftAtThePreviousLocation(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	legacyRoot := realTempDir(t)
	writeLegacyStaged(t, legacyRoot, testReviewer, `{"version":1,"next_id":9,"items":[
		{"id":"s6","body":"first finding"},
		{"id":"s7","file":"auth.go","line":42,"body":"second finding"},
		{"id":"s8","body":"third finding"}]}`)
	ws := legacyStagingWorkspace(root, legacyRoot, testReviewer)
	store, err := openStagingStore(ws, testRepo, testWorkBranch)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })
	items, err := store.list()
	require.NoError(t, err)
	require.Len(t, items, 3, "the whole batch must come across, not the first of it")
	assert.Equal(t, []string{"s6", "s7", "s8"}, stagedIDs(items))
	assert.Equal(t, "first finding", items[0].Body)
	assert.Equal(t, "auth.go", items[1].File)
	assert.Equal(t, uint32(42), items[1].Line)
	assert.Equal(t, "third finding", items[2].Body)
	next, err := store.add(stagedItem{Body: "a fourth, staged after the upgrade"})
	require.NoError(t, err)
	assert.Equal(t, "s9", next.ID, "the id counter must come across too: a reused id renames a comment the reviewer already recorded")
	dir, err := ws.stagingPath(testRepo, testWorkBranch)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(dir, stagedFileName), "the adopted batch must live at the new location from now on")
}

// TestOpenStaging_ExistingStagedComments_AreNotOverwrittenByTheLegacyOnes
// pins the guard that keeps adoption from becoming its own data-loss path.
// A reviewer with live staged comments at the new location must never have
// them replaced by a stale document left at the old one.
func TestOpenStaging_ExistingStagedComments_AreNotOverwrittenByTheLegacyOnes(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	legacyRoot := realTempDir(t)
	live := openTestStoreFor(t, legacyStagingWorkspace(root, root, testReviewer))
	_, err := live.add(stagedItem{Body: "live finding"})
	require.NoError(t, err)
	writeLegacyStaged(t, legacyRoot, testReviewer, `{"version":1,"next_id":3,"items":[{"id":"s2","body":"stale"}]}`)
	reopened := openTestStoreFor(t, legacyStagingWorkspace(root, legacyRoot, testReviewer))
	items, err := reopened.list()
	require.NoError(t, err)
	require.Len(t, items, 1, "a stale legacy document must not be merged into, or over, a live staging area")
	assert.Equal(t, "live finding", items[0].Body, "adoption must never clobber staged comments that are already here")
}

// TestOpenStaging_UnreadableLegacyDocument_FailsRatherThanReportingEmpty
// is the reason adoption reports its failures. A legacy staged.json that
// exists but cannot be read is exactly the case where carrying on would
// present an empty staging area as the answer — and an empty staging area
// presented as the answer is this whole bead.
func TestOpenStaging_UnreadableLegacyDocument_FailsRatherThanReportingEmpty(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	legacyRoot := realTempDir(t)
	writeLegacyStaged(t, legacyRoot, testReviewer, `{"version":1,"next_id":2,"items":[{"id":"s1","body":"unreachable"}]}`)
	legacyFile := filepath.Join(legacyRoot, loamDirName, stagingDirName, testRepo, testWorkBranch, testReviewer, stagedFileName)
	require.NoError(t, os.Chmod(legacyFile, 0o000))
	t.Cleanup(func() { _ = os.Chmod(legacyFile, stagingFilePerm) })
	_, err := openStagingStore(legacyStagingWorkspace(root, legacyRoot, testReviewer), testRepo, testWorkBranch)
	require.Error(t, err)
	assert.ErrorIs(t, err, errStagingArea)
	assert.Contains(t, err.Error(), legacyFile, "the failure must name the document it could not carry over")
}

// TestOpenStaging_NoLegacyDocument_IsAnOrdinaryEmptyStagingArea is the
// control: adoption must be invisible when there is nothing to adopt,
// which is every invocation after the first.
func TestOpenStaging_NoLegacyDocument_IsAnOrdinaryEmptyStagingArea(t *testing.T) {
	t.Parallel()
	store := openTestStoreFor(t, legacyStagingWorkspace(realTempDir(t), realTempDir(t), testReviewer))
	items, err := store.list()
	require.NoError(t, err)
	assert.Empty(t, items)
}
