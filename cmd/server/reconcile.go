package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
)

// mirrorReconcilerFunc matches mirrorreconcile.ReconcileMirror's
// signature. reconcileMirrors takes it as a parameter, rather than calling
// mirrorreconcile.ReconcileMirror directly, so a test can substitute a
// recording spy and prove which mirror paths it reconciled -- and that a
// per-repo failure aborts the loop -- without touching real git. This
// mirrors connectDatabase's migrateFunc/newPoolFunc convention in
// database.go.
type mirrorReconcilerFunc func(ctx context.Context, repoPath string) error

// reconcileMirrors runs docs/server-spec.md Startup step 3: idempotently
// reconciles every enrolled repo's bare mirror (docs/git-spec.md
// "Enforcement Mechanics"), skipping -- not erroring on -- a mirror
// missing from disk, exactly as mirrorreconcile.ReconcileMirror's own doc
// comment requires. lister supplies the enrollment: production always
// passes *reposstore.Store, which satisfies repoNameLister structurally
// via its own ListAllRepoNames. A real reconciliation error (as opposed to
// a merely-missing mirror, which reconcile itself reports as nil) aborts
// the loop immediately rather than continuing past it, matching
// docs/server-spec.md Startup's own "failing fast at each step" contract.
func reconcileMirrors(ctx context.Context, logger *slog.Logger, dataDir string, lister repoNameLister, reconcile mirrorReconcilerFunc) error {
	names, err := lister.ListAllRepoNames(ctx)
	if err != nil {
		return fmt.Errorf("listing enrolled repos for mirror reconciliation: %w", err)
	}
	for _, name := range names {
		path := mirrorPath(dataDir, name)
		if err := reconcile(ctx, path); err != nil {
			return fmt.Errorf("reconciling mirror for %s: %w", name, err)
		}
	}
	logger.Info("reconciled mirrors", "count", len(names))
	return nil
}

// mirrorPath derives an enrolled repo's bare-mirror path from
// docs/server-spec.md's LOAM_DATA_DIR row: "bare mirrors under
// <dir>/mirrors/<group>/<repo_name>.git". repoName is repos.name, already
// the "<group>/<repo_name>" string (docs/persistence-spec.md "Git
// mirrors": "path derived from repos.name"; internal/mirrorsync.RepoID's
// doc comment makes the same point), so this is a single join, not a
// two-level split/join.
func mirrorPath(dataDir, repoName string) string {
	return filepath.Join(dataDir, "mirrors", repoName+".git")
}
