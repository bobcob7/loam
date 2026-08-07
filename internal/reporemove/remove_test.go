package reporemove

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/reposstore"
)

const testRepoName = "acme/widgets"

// newRemover builds a Remover over a fresh temp LOAM_DATA_DIR and a
// repoDeleter mock that reports a successful delete of testRepoName,
// returning both plus the data dir. Individual tests override
// DeleteRepoFunc to drive the failure paths.
func newRemover(t *testing.T) (*Remover, *repoDeleterMock, string) {
	t.Helper()
	dataDir := t.TempDir()
	deleter := &repoDeleterMock{
		DeleteRepoFunc: func(_ context.Context, id uuid.UUID) (reposstore.Repo, error) {
			return reposstore.Repo{ID: id, Name: testRepoName}, nil
		},
	}
	return New(dataDir, deleter, slog.New(slog.NewJSONHandler(io.Discard, nil))), deleter, dataDir
}

// seedMirror creates a non-empty directory tree where testRepoName's bare
// mirror would live, so a test can tell "removed" apart from "was never
// there" and so removal has to actually recurse rather than succeed on an
// empty directory.
func seedMirror(t *testing.T, dataDir string) string {
	t.Helper()
	mirrorDir := mirrorpath.Dir(dataDir, testRepoName)
	require.NoError(t, os.MkdirAll(filepath.Join(mirrorDir, "objects", "pack"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(mirrorDir, "hooks"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mirrorDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mirrorDir, "hooks", "pre-receive"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mirrorDir, "objects", "pack", "pack-1.pack"), []byte("PACK"), 0o644))
	return mirrorDir
}

// trashSiblings returns every ".removing-" directory left behind next to
// mirrorDir -- the doomed rename targets DeleteRepo is supposed to have
// deleted after renaming.
func trashSiblings(t *testing.T, mirrorDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(mirrorDir))
	require.NoError(t, err)
	var left []string
	for _, e := range entries {
		if strings.Contains(e.Name(), trashSuffix) {
			left = append(left, e.Name())
		}
	}
	return left
}

// TestDeleteRepo_DeletesTheRowAndTheWholeMirrorTree is the happy path: the
// repos row is deleted (which cascades every derived table away inside
// Postgres -- proved separately in remove_integration_test.go) and the
// bare mirror, INCLUDING the installed pre-receive hook and the pack
// files under it, is gone from disk with no doomed sibling left behind.
func TestDeleteRepo_DeletesTheRowAndTheWholeMirrorTree(t *testing.T) {
	t.Parallel()
	r, deleter, dataDir := newRemover(t)
	mirrorDir := seedMirror(t, dataDir)
	id := uuid.New()
	require.NoError(t, r.DeleteRepo(t.Context(), id))
	require.Len(t, deleter.DeleteRepoCalls(), 1)
	assert.Equal(t, id, deleter.DeleteRepoCalls()[0].ID)
	assert.NoDirExists(t, mirrorDir, "the bare mirror must be gone from disk")
	assert.NoFileExists(t, filepath.Join(mirrorDir, "hooks", "pre-receive"), "the installed loamhook pre-receive hook goes with the mirror")
	assert.Empty(t, trashSiblings(t, mirrorDir), "the doomed rename target must be deleted too, not merely renamed aside")
}

// TestDeleteRepo_StoreFailure_LeavesTheMirrorAlone proves the ordering is
// row-first and that it is genuinely a gate, not just a sequence: if the
// repos row cannot be deleted, nothing on disk may be touched. The
// opposite order would have already destroyed a mirror belonging to a repo
// that is still enrolled -- the one unrecoverable half-state this
// operation has.
func TestDeleteRepo_StoreFailure_LeavesTheMirrorAlone(t *testing.T) {
	t.Parallel()
	r, deleter, dataDir := newRemover(t)
	mirrorDir := seedMirror(t, dataDir)
	sentinel := errors.New("connection refused")
	deleter.DeleteRepoFunc = func(_ context.Context, _ uuid.UUID) (reposstore.Repo, error) {
		return reposstore.Repo{}, sentinel
	}
	err := r.DeleteRepo(t.Context(), uuid.New())
	require.ErrorIs(t, err, sentinel)
	assert.DirExists(t, mirrorDir, "a failed metadata delete must not remove the still-enrolled repo's mirror")
	assert.FileExists(t, filepath.Join(mirrorDir, "HEAD"))
}

// TestDeleteRepo_NotFound_LeavesTheMirrorAlone is the same gate for the
// specific store error unenrolling an already-unenrolled repo produces.
// It must not fall through and delete a directory on the strength of a
// name it never actually read from a row.
func TestDeleteRepo_NotFound_LeavesTheMirrorAlone(t *testing.T) {
	t.Parallel()
	r, deleter, dataDir := newRemover(t)
	mirrorDir := seedMirror(t, dataDir)
	deleter.DeleteRepoFunc = func(_ context.Context, _ uuid.UUID) (reposstore.Repo, error) {
		return reposstore.Repo{}, reposstore.ErrNotFound
	}
	require.ErrorIs(t, r.DeleteRepo(t.Context(), uuid.New()), reposstore.ErrNotFound)
	assert.DirExists(t, mirrorDir)
}

// TestDeleteRepo_NoMirrorOnDisk_Succeeds proves a repo whose enrollment
// clone failed -- a repos row with sync_state error and no directory ever
// created (internal/handler/repoadmin's EnrollRepo leaves exactly that) --
// is still removable. An absent mirror is the goal state, not an error.
func TestDeleteRepo_NoMirrorOnDisk_Succeeds(t *testing.T) {
	t.Parallel()
	r, deleter, _ := newRemover(t)
	require.NoError(t, r.DeleteRepo(t.Context(), uuid.New()))
	assert.Len(t, deleter.DeleteRepoCalls(), 1)
}

// TestDeleteRepo_UsesTheDeletedRowsName_NotTheID proves the mirror path is
// derived from the name the DELETE returned, which is the only reason
// reposstore.DeleteRepo returns the row at all. A Remover that guessed the
// path from anything else would leave this mirror standing.
func TestDeleteRepo_UsesTheDeletedRowsName_NotTheID(t *testing.T) {
	t.Parallel()
	r, deleter, dataDir := newRemover(t)
	deleter.DeleteRepoFunc = func(_ context.Context, id uuid.UUID) (reposstore.Repo, error) {
		return reposstore.Repo{ID: id, Name: "other/project"}, nil
	}
	kept := seedMirror(t, dataDir)
	removed := mirrorpath.Dir(dataDir, "other/project")
	require.NoError(t, os.MkdirAll(removed, 0o755))
	require.NoError(t, r.DeleteRepo(t.Context(), uuid.New()))
	assert.NoDirExists(t, removed, "the mirror named by the deleted row must be the one removed")
	assert.DirExists(t, kept, "no other repo's mirror may be touched")
}

// TestDeleteRepo_MirrorCannotBeMovedAside_IsReported proves the leg that
// leaves the CANONICAL mirror path still occupied is a returned error, not
// a log line: the metadata is already gone, so silence here would leave an
// admin with a repo that looks unenrolled but cannot be re-enrolled under
// the same name (git refuses to clone into a non-empty directory), with
// nothing anywhere saying why.
//
// The rename is made to fail without any chmod -- planting a regular file
// where the mirror's parent group directory belongs makes every path
// operation under it fail with ENOTDIR -- so this test behaves identically
// whether or not the test process happens to be running as root.
func TestDeleteRepo_MirrorCannotBeMovedAside_IsReported(t *testing.T) {
	t.Parallel()
	r, _, dataDir := newRemover(t)
	groupDir := filepath.Dir(mirrorpath.Dir(dataDir, testRepoName))
	require.NoError(t, os.MkdirAll(filepath.Dir(groupDir), 0o755))
	require.NoError(t, os.WriteFile(groupDir, []byte("not a directory"), 0o644))
	err := r.DeleteRepo(t.Context(), uuid.New())
	require.Error(t, err, "a mirror that cannot be moved aside must not be reported as a clean removal")
	assert.Contains(t, err.Error(), "must be removed by hand", "the error has to tell the admin what is left over")
	assert.Contains(t, err.Error(), testRepoName)
}

// TestDeleteRepo_FreesTheCanonicalPathEvenWhenTheDeleteCannotFinish is the
// test that makes the rename-before-delete load-bearing rather than
// decorative. With an undeletable entry inside it, a plain os.RemoveAll of
// the mirror would fail partway and leave the canonical path occupied; the
// rename frees that path first, so re-enrolling the same repo still works
// and the RPC still reports success.
//
// THE SKIP UNDER ROOT IS DELIBERATE AND, UNLIKE THE OTHER FOUR SITES IN
// THIS TREE, UNAVOIDABLE (loam-0nuo). It is a real hole -- the gate runs as
// uid 0, so this branch has never been exercised in CI -- and the reason it
// stays open is worth writing down, because "just use the uid-immune
// technique" is the obvious review note and it does not work here.
//
// loam-9g17's technique replaces a permission failure with a SHAPE failure:
// a directory where a file is expected reads EISDIR, a regular file where a
// directory is expected opens ENOTDIR, a dangling symlink creates ENOENT.
// Root receives all three exactly as anyone else does, because none of them
// is a permission check.
//
// What this test needs is for os.RemoveAll to STOP PARTWAY with the
// canonical path still occupied. The distinction that matters is not which
// syscall fails -- under 0o000 it stops at openfdat, an open failure, which
// is the same class loam-9g17 uses -- it is DAC VERSUS SHAPE. Every way of
// making a tree undeletable that is available to an ordinary test turns out
// to be a permission bit on the containing directory, and permission bits
// are what CAP_DAC_OVERRIDE ignores. A shape cannot substitute, because a
// tree the walk cannot delete is not a tree of the wrong shape; it is a tree
// the walker is not allowed into.
//
// NO SHAPE COMPATIBLE WITH A PARALLEL, IN-PROCESS TEST SURVIVES UID 0. That
// is deliberately not the same claim as "no shape survives uid 0" -- the
// fourth bullet is a measured counterexample, and a universal here would
// only stop the next person looking. All four measured inside
// golang:1.26.5, the gate's own image, at uid 0 (CapEff 0x800405fb:
// CAP_DAC_OVERRIDE set, CAP_LINUX_IMMUTABLE and CAP_SYS_ADMIN clear):
//
//   - 0o500 and 0o000 on the containing directory: os.RemoveAll returns nil
//     and the tree is gone. (At uid 1000 the same two fixtures fail at
//     unlinkat and openfdat respectively, which is the asymmetry.)
//   - The immutable inode flag does block root, but setting it needs
//     CAP_LINUX_IMMUTABLE, which the gate's container does not have (the
//     ioctl returns EPERM there), and an unprivileged developer could never
//     set it either -- so it would trade this skip for a differently-shaped
//     one.
//   - EBUSY from a mount point inside the tree needs CAP_SYS_ADMIN, same
//     problem: mount -t tmpfs returns EPERM there.
//   - Resource exhaustion DOES survive uid 0, and is the reason the sentence
//     above is qualified rather than universal. RLIMIT_NOFILE is not a
//     permission and no capability bypasses it, and LOWERING A SOFT LIMIT
//     NEEDS NO PRIVILEGE: with NOFILE=12 over a 40-deep tree, os.RemoveAll
//     as uid 0 fails at "openfdat ...: too many open files" with the
//     top-level path still occupied -- exactly the state this test needs.
//     It is unusable HERE, for reasons that have nothing to do with uid:
//     syscall.Setrlimit is process-global and this test is t.Parallel(), so
//     a shared test binary at NOFILE=12 would take the rest of the package
//     down with it. A re-exec'd helper subprocess would isolate it, at the
//     cost of pinning the fixture to how many directory fds os.RemoveAll
//     holds concurrently -- an implementation detail of the standard
//     library, and a far more fragile thing to depend on than a chmod.
//
// Path length is not a fourth option, for a reason worth recording because
// it is the obvious next idea: os.RemoveAll descends with openat and
// unlinkat relative to a directory fd, so it deletes trees no absolute path
// could name. Measured, a tree whose nominal absolute path is 12080
// characters -- three times PATH_MAX -- is removed with a nil error.
//
// So this stays a skip, and everything around it is arranged so that the
// skip is the only thing lost. The precondition below asserts the subtree
// really was made undeletable before anything is concluded from it, so the
// day this fixture stops working the test says which step stopped working
// rather than failing as a bare "expected no error". And the rename leg
// itself IS covered at every uid by
// TestDeleteRepo_MirrorCannotBeMovedAside_IsReported above; what is
// uncovered in CI is specifically the combination "rename succeeded, delete
// failed, RPC still reports success".
func TestDeleteRepo_FreesTheCanonicalPathEvenWhenTheDeleteCannotFinish(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("uid 0 holds CAP_DAC_OVERRIDE and deletes the subtree this test makes undeletable; see the comment above for why no uid-immune shape reaches an unlink failure")
	}
	r, _, dataDir := newRemover(t)
	mirrorDir := seedMirror(t, dataDir)
	locked := filepath.Join(mirrorDir, "objects", "pack")
	require.NoError(t, os.Chmod(locked, 0o500))
	t.Cleanup(func() {
		// Restored before t.TempDir's own cleanup runs (cleanups are
		// LIFO), which would otherwise fail the test trying to remove
		// what this one deliberately made unremovable. Registered before
		// the precondition below so that a precondition failure still
		// leaves a removable tree behind.
		_ = os.Chmod(locked, 0o700)
		_ = os.Chmod(filepath.Dir(locked), 0o700)
	})
	// The whole test rests on that chmod having actually taken effect for
	// whoever is running it. Probe it directly rather than inferring it from
	// the outcome: a caller that is not uid 0 but does hold CAP_DAC_OVERRIDE
	// (or a filesystem that ignores mode bits) would otherwise sail past the
	// guard above and fail three assertions later, saying nothing about why.
	probeErr := os.Remove(filepath.Join(locked, "pack-1.pack"))
	require.Error(t, probeErr, "precondition: the 0o500 chmod must really make %s undeletable, or this test proves nothing", locked)
	require.ErrorIs(t, probeErr, fs.ErrPermission, "precondition: the undeletability must come from the mode bits this test set")
	require.NoError(t, r.DeleteRepo(t.Context(), uuid.New()), "a mirror whose delete cannot finish must not fail the removal once the canonical path is free")
	assert.NoDirExists(t, mirrorDir, "the canonical mirror path must be free, so the same repo can be re-enrolled")
	left := trashSiblings(t, mirrorDir)
	require.Len(t, left, 1, "what could not be deleted must still have been renamed out of the way")
	// Point the cleanup at where the locked subtree actually ended up.
	locked = filepath.Join(filepath.Dir(mirrorDir), left[0], "objects", "pack")
}

// TestPackageNeverImportsAForgeClient is the structural guarantee behind
// this package's "removal never touches upstream" claim (docs/web-spec.md
// scopes RemoveRepo to Loam's own mirror, indexes, and metadata): there is
// no code path from here to a forge API call or a git subprocess because
// there is no import that could carry one. Asserted rather than promised,
// so adding one is a test failure and a deliberate decision, not an
// oversight in review.
func TestPackageNeverImportsAForgeClient(t *testing.T) {
	t.Parallel()
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	require.NoError(t, err)
	require.Contains(t, pkgs, "reporemove")
	forbidden := []string{"/forge", "/gittransport", "/gitdiff", "/gitancestry", "/gitmergetree", "net/http", "os/exec"}
	var seen []string
	for _, file := range pkgs["reporemove"].Files {
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			seen = append(seen, path)
			for _, bad := range forbidden {
				assert.NotContains(t, path, bad, "unenrolling a repo must not be able to reach the upstream forge or run git")
			}
		}
	}
	require.NotEmpty(t, seen, "the import scan found nothing -- it would pass vacuously against any implementation")
}
