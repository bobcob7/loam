package reporemove

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/mirrorpath"
)

// trashSuffix is appended (with a random discriminator) to a mirror's own
// path to name the doomed directory Remover.DeleteRepo renames it to
// before deleting it. It deliberately does not end in ".git", so
// internal/mirrorpath.DataDir rejects such a path outright rather than
// resolving it as a live mirror, and nothing in this tree walks the
// mirrors directory looking for repos to act on.
const trashSuffix = ".removing-"

// Remover implements internal/handler/repoadmin's repoDeleter: the
// production repo-delete path RemoveRepo calls once its non-terminal
// work-branch guard clears. Construct with New and wire in
// cmd/server/main.go.
type Remover struct {
	dataDir string
	repos   repoDeleter
	logger  *slog.Logger
}

// New builds a Remover. dataDir is LOAM_DATA_DIR, the root each repo's
// bare mirror lives under (internal/mirrorpath.Dir) -- the same value
// internal/handler/repoadmin's Handler is constructed with, so enrollment
// and unenrollment necessarily agree on where a mirror is.
func New(dataDir string, repos repoDeleter, logger *slog.Logger) *Remover {
	return &Remover{dataDir: dataDir, repos: repos, logger: logger}
}

// DeleteRepo unenrolls id: it deletes the repos row -- which cascades to
// every repo-scoped and derived table in one Postgres transaction (see
// internal/db/queries/repos.sql's DeleteRepo for the exhaustive list) --
// and then removes the bare mirror the deleted row's name pointed at.
//
// # Ordering, and what a partial failure leaves behind
//
// The row goes first and the directory second, on purpose. A filesystem
// removal cannot join the database transaction (the same reasoning
// loam-giq.7 recorded for a git push: an external effect is not a
// transaction participant, so the only real choice is which side of the
// commit it sits on and which half-state is survivable). The two orders
// leave very different wreckage:
//
//   - Row first. A failure after the commit leaves a mirror directory with
//     no repos row. Nothing in the system looks at it: mirrorsync's
//     scheduler enumerates repos rows, the ingest pool's jobs cascaded away
//     with the row, and internal/handler/git resolves a push/clone through
//     repos too. It is inert garbage, removable with rm -rf, and the only
//     functional consequence is that re-enrolling the SAME name would find
//     its clone target occupied -- which is exactly what the rename below
//     exists to prevent.
//   - Directory first. A failure after the removal leaves a repos row whose
//     mirror is gone: the scheduler keeps listing it and failing to fetch,
//     ingest keeps claiming jobs for it and failing to read it, and a clone
//     or push against it 500s. That is a live, self-perpetuating fault
//     rather than inert garbage, and it is not fixable with rm -rf.
//
// Row-first is the recoverable order, so it is the order used. This is the
// same shape internal/handler/proposal's CloseWorkBranch settled on for
// its own two-legged operation: commit the authoritative row, then attempt
// the external effect.
//
// # Why the mirror is renamed before it is deleted
//
// os.RemoveAll on a live mirror is not atomic: it walks and unlinks, and a
// concurrent git subprocess (a still-in-flight fetch or ingest, see the
// in-flight-work note below) can re-create files under it while the walk
// is in progress, so it can return a partial success with the CANONICAL
// mirror path still occupied. A rename is a single atomic syscall on the
// same filesystem (the trash path is a sibling of the mirror, so it always
// is): once it returns, mirrorpath.Dir(dataDir, name) is guaranteed free
// and re-enrolling that repo will succeed, whatever happens to the doomed
// directory afterwards.
//
// That is why the two legs report failure differently, and the difference
// is load-bearing rather than incidental:
//
//   - The rename failing IS returned. The canonical path is still occupied,
//     with no row behind it, and re-enrollment of that name would fail on a
//     non-empty clone target with an error naming git rather than Loam. The
//     admin has to be told, even though the unenrollment itself already
//     committed -- reporting success while leaving that trap set is worse
//     than an error that says exactly which directory needs removing.
//   - The subsequent delete failing is only LOGGED. The canonical path is
//     already free and every observable part of RemoveRepo's contract is
//     met; what remains is disk usage under a clearly-named path, and
//     failing the RPC over it would tell the admin that an unenrollment
//     that fully succeeded did not.
//
// A mirror that is simply not there is a no-op, not an error: a repo whose
// enrollment clone failed has a repos row and no directory (EnrollRepo
// marks sync_state Error and leaves the row), and that repo must still be
// removable.
//
// # In-flight work: this races, deliberately
//
// A repo can be busy at the moment it is removed: internal/ingest's pool
// may have a job in status running for it, and internal/mirrorsync's
// scheduler may be mid-cycle on it (its per-repo `running` guard only
// stops IT from starting a second cycle, and says nothing to anyone else).
// This method neither blocks on ingest.Pool.DrainRepoID nor refuses while
// either is busy. It races them, because the race is already safe and
// neither alternative actually closes it:
//
//   - Postgres serializes the database half by itself. An in-flight ingest
//     transaction inserting symbols/chunks holds a KEY SHARE lock on the
//     repos row through its foreign keys, so this DELETE waits for that
//     transaction rather than interleaving with it. If ingest commits
//     first, the cascade removes what it just wrote; if the DELETE commits
//     first, ingest's next insert fails its foreign key and its whole
//     transaction rolls back (the ingest swap is one transaction --
//     internal/ingest/orchestrator). There is no interleaving that leaves
//     an orphaned derived row, which is why no Go-side coordination is
//     needed to prevent one.
//   - Draining would not make it deterministic anyway. DrainRepoID returns
//     when zero jobs are queued-or-running, which by its own documented
//     contract can be true while a failed job is waiting out its retry
//     backoff, and nothing stops the sync scheduler from enqueuing a fresh
//     job the instant it returns. The check-then-act window survives the
//     drain; it just gets narrower and much more expensive.
//   - Refusing while busy would trade a benign race for a real failure
//     mode: an admin unable to remove a repo because a periodic background
//     job happened to be running, on an operation whose whole purpose is
//     to stop that job from running ever again.
//
// The losers of the race all fail cleanly and self-correct. A running
// ingest job fails (its repo, its mirror, or both are gone) and its
// ingest_jobs row has already been cascaded away, so nothing retries it. A
// sync cycle mid-fetch fails against the vanished mirror and its
// sync-state write updates zero rows, which mirrorsync's scheduler already
// logs without aborting. The next scheduler tick does not list the repo at
// all, because it enumerates repos rows.
//
// Nothing here touches the upstream forge -- see this package's doc
// comment for why that is structural rather than a promise.
func (r *Remover) DeleteRepo(ctx context.Context, id uuid.UUID) error {
	row, err := r.repos.DeleteRepo(ctx, id)
	if err != nil {
		return fmt.Errorf("unenrolling repo %s: deleting metadata: %w", id, err)
	}
	return r.removeMirror(ctx, row.Name)
}

// removeMirror frees the canonical mirror path for name by renaming it
// aside, then deletes what it renamed. See DeleteRepo's doc comment for
// why those two steps report failure differently.
func (r *Remover) removeMirror(ctx context.Context, name string) error {
	mirrorDir := mirrorpath.Dir(r.dataDir, name)
	trashDir := mirrorDir + trashSuffix + uuid.NewString()
	if err := os.Rename(mirrorDir, trashDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.logger.InfoContext(ctx, "unenrolled repo had no mirror on disk", "repo", name, "mirror_dir", mirrorDir)
			return nil
		}
		return fmt.Errorf("unenrolling repo %s: metadata is deleted but its mirror %s could not be moved aside for removal and must be removed by hand: %w", name, mirrorDir, err)
	}
	if err := os.RemoveAll(trashDir); err != nil {
		r.logger.ErrorContext(ctx, "unenrolled repo's mirror was moved aside but not fully deleted; remove it by hand",
			"repo", name, "trash_dir", trashDir, "error", err)
	}
	return nil
}
