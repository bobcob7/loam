// Package reporemove composes the two halves of unenrolling a repo
// (docs/web-spec.md -> RepoAdminService "RemoveRepo": "unenroll: drop the
// mirror, the derived indexes, and the repo's metadata (work branches,
// rounds, verdicts, threads -- unenrollment removes history; re-enrolling
// starts fresh). Queued/running ingest jobs are deleted."): the repos-row
// delete, whose ON DELETE CASCADE chain takes every repo-scoped and
// derived table with it in one transaction, and the removal of the bare
// mirror directory the row's name points at.
//
// It exists as its own package rather than as a method on
// internal/reposstore.Store or an adapter closure in cmd/server/main.go
// for two reasons. A store does not remove directories -- reposstore
// speaks only to a *gen.Queries and has no business knowing LOAM_DATA_DIR
// exists. And the ordering, the rename-before-delete, and what a partial
// failure is allowed to leave behind (see Remover.DeleteRepo) are real,
// testable decisions, not wiring: putting them in main.go would put them
// where no test in this tree can reach them.
//
// It does NOT touch the upstream forge. Unenrollment is a Loam-side
// operation: docs/web-spec.md scopes RemoveRepo to Loam's own mirror,
// indexes, and metadata, and an admin removing a repo from Loam must
// never risk deleting the repository it was mirroring. That is enforced
// structurally, not by discipline -- this package imports no forge
// client, no HTTP client, and no git transport, and its only two
// collaborators are a repoDeleter (one DELETE against Postgres) and the
// standard library's os. There is no code path from here to a forge API
// call, and adding one would require adding an import.
package reporemove

import (
	"context"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/reposstore"
)

//go:generate go tool moq -out moq_test.go . repoDeleter

// repoDeleter deletes a repos row and returns the row as it was
// immediately before deletion, defined here at the consumer.
// *reposstore.Store satisfies it structurally. Remover needs the returned
// row for exactly one field -- Name, which internal/mirrorpath.Dir turns
// into the bare mirror's on-disk path -- but takes the whole Repo because
// that is the store method's own signature, and narrowing it to a string
// here would mean an adapter in the composition root for no gain.
type repoDeleter interface {
	DeleteRepo(ctx context.Context, id uuid.UUID) (reposstore.Repo, error)
}
