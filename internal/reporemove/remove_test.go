package reporemove

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
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

// raceTreeWidth and raceTreeFiles size the tree the concurrent writer below
// repopulates. Wide rather than deep: os.RemoveAll empties a directory and
// then rmdirs it, so every directory is an independent chance for the writer
// to land a file in the window between those two steps, and 40 of them make
// the window collectively hard to miss.
const (
	raceTreeWidth = 40
	raceTreeFiles = 40
)

// raceAttempts bounds the retry loop. Each attempt is independently very
// likely to leave a remnant, so this exists only so that the test FAILS,
// naming the mechanism, rather than passing vacuously if the writer never
// once wins -- the shape of "prove the failure was manufactured" that the
// rest of this branch applies to preconditions.
const raceAttempts = 8

// seedRaceTree adds a wide tree of files under mirrorDir for a concurrent
// writer to repopulate while the removal walks it.
func seedRaceTree(t *testing.T, mirrorDir string) {
	t.Helper()
	for i := range raceTreeWidth {
		dir := filepath.Join(mirrorDir, "race", fmt.Sprintf("d%d", i))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		for j := range raceTreeFiles {
			require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", j)), []byte("x"), 0o644))
		}
	}
}

// deleteRacingAConcurrentWriter runs one attempt: unenroll a repo whose
// mirror a still-running writer is creating files under, and report whether
// the delete was in fact left unfinished. Everything that must hold
// REGARDLESS of who won the race is asserted here; only the return value is
// conditional.
//
// The writer holds a DIRECTORY HANDLE opened before DeleteRepo is called,
// which is the detail that makes this work at all. The test cannot name the
// trash path -- removeMirror composes it from a fresh uuid -- but it does not
// need to: an open handle is pinned to the directory's inode, so it follows
// the tree through the rename exactly as a still-running git subprocess with
// the mirror open would. os.Root is used here purely as that handle; none of
// its containment checking is under test.
func deleteRacingAConcurrentWriter(t *testing.T) bool {
	t.Helper()
	r, _, dataDir := newRemover(t)
	mirrorDir := seedMirror(t, dataDir)
	seedRaceTree(t, mirrorDir)
	handle, err := os.OpenRoot(mirrorDir)
	require.NoError(t, err)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			for i := range raceTreeWidth {
				// Errors are the normal case once the walk has passed a
				// directory, and are not the writer's business: it is
				// modelling a process that does not know it is racing.
				if f, err := handle.Create(fmt.Sprintf("race/d%d/n%d", i, n)); err == nil {
					_ = f.Close()
				}
			}
		}
	}()
	deleteErr := r.DeleteRepo(t.Context(), uuid.New())
	close(stop)
	<-done
	require.NoError(t, handle.Close())
	require.NoError(t, deleteErr, "a mirror whose delete cannot finish must not fail the removal once the canonical path is free")
	assert.NoDirExists(t, mirrorDir, "the canonical mirror path must be free, so the same repo can be re-enrolled")
	require.NoError(t, os.MkdirAll(mirrorDir, 0o755), "re-enrolment must be able to create the mirror path again -- which is the entire point of freeing it")
	require.NoError(t, os.RemoveAll(mirrorDir))
	left := trashSiblings(t, mirrorDir)
	if len(left) != 1 {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(mirrorDir), left[0]))
	require.NoError(t, err)
	assert.NotEmpty(t, entries, "the remnant must be the tree that could not be deleted, not an empty husk renamed aside")
	return true
}

// TestDeleteRepo_FreesTheCanonicalPathEvenWhenTheDeleteCannotFinish is the
// test that makes the rename-before-delete load-bearing rather than
// decorative. With the delete unable to finish, a plain os.RemoveAll of the
// mirror would fail partway and leave the canonical path occupied; the
// rename frees that path first, so re-enrolling the same repo still works
// and the RPC still reports success.
//
// IT USED TO SKIP UNDER ROOT, WHICH MEANT IT HAD NEVER RUN IN CI (loam-0nuo)
// -- the gate is uid 0, and a skip and a pass are indistinguishable in
// summary output. It now runs at every uid, because the delete is made to
// fail by THE MECHANISM removeMirror'S DOC COMMENT ALREADY NAMES as its own
// justification:
//
//	"a concurrent git subprocess (a still-in-flight fetch or ingest) can
//	re-create files under it while the walk is in progress, so it can return
//	a partial success with the CANONICAL mirror path still occupied"
//
// A goroutine doing exactly that -- creating files under the tree while
// os.RemoveAll walks it -- makes the removal fail with ENOTEMPTY, and no
// capability exempts anyone from ENOTEMPTY. The fixture is now the hazard the
// design exists for rather than a chmod standing in for it, which is a better
// test than the one it replaces and not merely a uid-portable one.
//
// FOUR EARLIER ATTEMPTS AND WHAT EACH COST, recorded as an enumeration and
// deliberately not as a claim about what exists -- three drafts of this
// comment asserted a universal and all three were falsified. All measured
// inside golang:1.26.5, the gate's own image, at uid 0 (CapEff 0x800405fb:
// CAP_DAC_OVERRIDE set, CAP_LINUX_IMMUTABLE and CAP_SYS_ADMIN clear):
//
//   - 0o500 or 0o000 on a containing directory, which is what this test used
//     to do: os.RemoveAll returns nil and the tree is gone. Permission bits
//     are precisely what CAP_DAC_OVERRIDE ignores. (At uid 1000 the same two
//     fixtures fail at unlinkat and openfdat -- that asymmetry IS the bug
//     this bead is about.)
//   - The immutable inode flag does block root, but setting it needs
//     CAP_LINUX_IMMUTABLE, which the gate's container lacks (the ioctl
//     returns EPERM), and an unprivileged developer could never set it --
//     it would trade this skip for a differently-shaped one.
//   - EBUSY from a mount point inside the tree needs CAP_SYS_ADMIN: mount -t
//     tmpfs returns EPERM there.
//   - RLIMIT_NOFILE survives uid 0 -- it is not a permission and no
//     capability bypasses it -- and at NOFILE=12 over a 40-deep tree
//     os.RemoveAll fails at "too many open files" with the top-level path
//     still occupied. Rejected because syscall.Setrlimit is process-global
//     and would take the rest of the package down with it, and because
//     isolating it in a subprocess would pin the fixture to how many
//     directory fds os.RemoveAll holds concurrently -- a stdlib
//     implementation detail.
//
// Path length is not among them, for a reason worth recording because it is
// the obvious next idea: os.RemoveAll descends with openat and unlinkat
// relative to a directory fd, so it deletes trees no absolute path could
// name. A tree whose nominal absolute path is 12080 characters -- three
// times PATH_MAX -- is removed with a nil error.
//
// IT IS A RACE, SO ITS DETERMINISM IS MEASURED RATHER THAN ASSUMED. With the
// retry loop set to a SINGLE attempt -- i.e. asking how often the writer wins
// on the first try -- 200 consecutive runs under -race passed at uid 0, 200
// at uid 1000, and 200 on darwin/arm64: 600 for 600.
//
// THE LOOP IS A VACUITY GUARD AT DEFAULT PARALLELISM AND FLAKE TOLERANCE
// UNDER FORCED SINGLE-THREADING, AND raceAttempts IS SIZED FOR THE SECOND.
// The distinction matters to anyone tempted to cut it to 1 on the strength of
// the paragraph above, which is why it is spelled out rather than left as
// "eight seems safe". At an explicit GOMAXPROCS=1 on Linux the single-attempt
// win rate falls off 100%. Five 200-run batches under -race gave 170, 155,
// 150, 155 and 145 passes -- a per-attempt loss of 15% to 27.5%. Sizing off
// the WORST OBSERVED value, 0.275^8 is about 1 in 30,600, which is why eight
// and not one.
//
// The spread is worth more than the number, and is the reason a RANGE rather
// than a rate appears above. Four of those batches came from one session and are
// statistically indistinguishable from each other (chi-squared 1.86, 3 df,
// p about 0.60); the fifth came from a different session and genuinely
// differs (z = 2.84, p about 0.005). So the rate is stable WITHIN a session
// and moves about nine points BETWEEN sessions -- which means any one
// session's batches look perfectly repeatable and invite quoting a rate to
// three significant digits that the next session will not reproduce. Two
// independent measurements were what revealed the band; one would have
// hidden it however many times it was repeated.
//
// THE SHIPPED CONFIGURATION WAS THEREFORE MEASURED IN THAT REGIME RATHER
// THAN INFERRED FROM THE ARITHMETIC -- which matters precisely because the
// arithmetic's INPUT turned out to be the soft part: raceAttempts = 8 at
// GOMAXPROCS=1, uid 0, 200 consecutive runs under -race, 200 for 200.
//
// At default parallelism nothing is being absorbed, and a failure there means
// the writer has stopped winning entirely: the test fails and says so,
// instead of quietly asserting nothing about a delete that actually
// succeeded.
//
// That regime is hard to reach by accident, and deliberately not overstated
// here. A CPU-quota runner does NOT land in it: Go floors container-derived
// GOMAXPROCS at 2, measured inside a --cpus=1 container (nproc reports 5,
// GOMAXPROCS reports 2), and at GOMAXPROCS=2 the single-attempt rate is
// 200/200 again. It takes an explicit GOMAXPROCS=1 or -cpu=1. Nor does it
// reproduce everywhere: on darwin/arm64, -cpu=1 is still 200/200 single
// attempt, so this is a Linux scheduling effect and not a universal property
// of the fixture.
//
// GOMAXPROCS is worth remembering as the knob for auditing ANY racing fixture
// in this tree. Measurements at default parallelism say very little about the
// adversarial case: forcing it to 1 turned "600 for 600, no question" into a
// visible failure rate in one run, and it is what found the only soft spot in
// this one.
//
// WHAT THIS DOES NOT COVER: EACCES specifically. The delete now fails with
// ENOTEMPTY rather than EPERM, so the literal "an operator's mirror contains
// a file this process may not unlink" is no longer exercised. That is
// acceptable because removeMirror does not discriminate on errno -- any
// non-nil error from os.RemoveAll takes the same log-and-succeed branch --
// so the errno is not part of the behaviour under test. What is under test
// is that a delete which cannot finish still leaves the canonical path free
// and still reports success, and that is now asserted at every uid.
func TestDeleteRepo_FreesTheCanonicalPathEvenWhenTheDeleteCannotFinish(t *testing.T) {
	t.Parallel()
	for range raceAttempts {
		if deleteRacingAConcurrentWriter(t) {
			return
		}
	}
	t.Fatalf("the concurrent writer never once left the delete unfinished in %d attempts: this test can no longer reach the branch it exists for", raceAttempts)
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
