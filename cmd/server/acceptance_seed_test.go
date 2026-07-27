//go:build acceptance

// This file seeds the "the repo ... is enrolled ..." / "I have started the
// work branch ..." Background fixtures clone-and-push.feature's scenarios
// need, by direct SQL INSERT plus a hand-built bare mirror on disk --
// exactly the technique Taskfile.yml's own demo:m2 target established and
// documents at length as this feature's proven happy path, NOT
// RepoAdminService.EnrollRepo: EnrollRepo requires a resolvable credential
// row and a real reachable upstream to clone from, a real enrollment
// flow's own concern that is orthogonal to clone-and-push's scope (see
// demo:m2's own doc comment in Taskfile.yml for the fuller rationale, and
// cmd/server/clonepush_integration_test.go's identical
// seedEnrolledRepoWithMirror for the original precedent this reproduces).
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/mirrorpath"
)

// insertRepoRow inserts world's repo row plus its one registered target
// branch, returning the generated repos.id for insertWorkBranchRow to
// reference. Called from stepRepoIsEnrolled (acceptance_steps_test.go),
// which implements the Background step "the repo ... is enrolled with
// target branch ...". upstream_url/forge_host name this scenario's REAL
// repo on the shared fake forge (seedUpstreamRepo, called by that same
// step just before this one), not a placeholder: every sync scenario
// fetches, pushes, and ls-remotes against exactly this row, so a
// fabricated URL here would make the whole Mirror Sync cycle
// unexercisable.
//
// That step and "I have started the work branch ..."
// (insertWorkBranchRow, seedBareMirrorWithBranches, below) are two
// separate Gherkin steps, not one call, since neither is a
// testing-spec core-vocabulary row (docs/testing-spec.md Layer 1's
// step-vocabulary table) -- this is scenario-specific fixture setup, not
// a driver call in the actor sense, and clone-and-push.feature's own
// Background lists them as two Given lines.
func (h *acceptanceHarness) insertRepoRow(ctx context.Context, world *acceptanceWorld) (uuid.UUID, error) {
	repoID := uuid.Must(uuid.NewV7())
	_, err := h.server.pool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, $3, $4, $5)`,
		repoID, world.repo(), world.upstreamURL, h.forgeHost, world.targetBranch)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("seeding repos row for %s: %w", world.repo(), err)
	}
	_, err = h.server.pool.Exec(ctx,
		`INSERT INTO repo_target_branches (repo_id, branch) VALUES ($1, $2)`,
		repoID, world.targetBranch)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("seeding repo_target_branches row for %s: %w", world.repo(), err)
	}
	return repoID, nil
}

// insertWorkBranchRow inserts one work_branches row owned by author,
// targeting target, in the given state.
func (h *acceptanceHarness) insertWorkBranchRow(ctx context.Context, repoID uuid.UUID, name, target, state, author string) error {
	_, err := h.server.pool.Exec(ctx,
		`INSERT INTO work_branches (id, repo_id, name, target, state, author) VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.Must(uuid.NewV7()), repoID, name, target, state, author)
	if err != nil {
		return fmt.Errorf("seeding work_branches row %s: %w", name, err)
	}
	return nil
}

// seedBareMirrorWithBranches builds a real bare git repository at
// mirrorpath.Dir(dataDir, repo) -- production's own on-disk layout
// convention -- seeded with one commit on targetBranch and a second local
// branch, workBranch, pointing at the same commit. Mirrors
// cmd/server/clonepush_integration_test.go's seedThrowawayBareMirror and
// Taskfile.yml's demo:m2 seed step; reproduced rather than shared across
// build tags, per this package's established convention (see
// startServerWithDataDir's own doc comment for that precedent).
func seedBareMirrorWithBranches(ctx context.Context, dataDir, repo, targetBranch, workBranch string) (string, error) {
	src, err := os.MkdirTemp("", "loam-acceptance-seed-src-*")
	if err != nil {
		return "", fmt.Errorf("creating throwaway seed source for %s: %w", repo, err)
	}
	defer os.RemoveAll(src)
	if err := seedRunGit(ctx, src, "init", "--quiet", "--initial-branch="+targetBranch); err != nil {
		return "", err
	}
	if err := seedRunGit(ctx, src, "config", "user.email", "acceptance-seed@example.invalid"); err != nil {
		return "", err
	}
	if err := seedRunGit(ctx, src, "config", "user.name", "acceptance-seed"); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644); err != nil {
		return "", fmt.Errorf("writing seed file for %s: %w", repo, err)
	}
	if err := seedRunGit(ctx, src, "add", "README.md"); err != nil {
		return "", err
	}
	if err := seedRunGit(ctx, src, "commit", "--quiet", "-m", "init"); err != nil {
		return "", err
	}
	if err := seedRunGit(ctx, src, "branch", workBranch); err != nil {
		return "", err
	}
	mirrorDir := mirrorpath.Dir(dataDir, repo)
	if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o755); err != nil {
		return "", fmt.Errorf("creating mirror parent dir for %s: %w", repo, err)
	}
	if err := seedRunGit(ctx, "", "clone", "--quiet", "--bare", src, mirrorDir); err != nil {
		return "", err
	}
	return mirrorDir, nil
}

// seedRunGit runs a real git subcommand in dir (empty means this process's
// own cwd), wrapping a non-zero exit with its combined output for an
// honest failure message.
func seedRunGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}
