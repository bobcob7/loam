package fakeforge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// SeedOptions configures SeedRepoFiles.
type SeedOptions struct {
	// DefaultBranch names the branch the initial commit lands on.
	// Defaults to "main".
	DefaultBranch string
}

// SeedRepo seeds repoName into the fake forge by cloning an existing repo
// (bare or working tree) found at sourcePath into the fake's own bare
// storage. This is the seam the acceptance harness (loam-li0.5) uses to
// wire in a fixture repo (loam-li0.4) without this package importing it.
func (s *Server) SeedRepo(ctx context.Context, repoName, sourcePath string) error {
	dest := s.repoDir(repoName)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("seeding %s: %w", repoName, errRepoExists)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("seeding %s: %w", repoName, err)
	}
	if _, err := s.runGit(ctx, "", "clone", "--bare", sourcePath, dest); err != nil {
		return fmt.Errorf("seeding %s from %s: %w", repoName, sourcePath, err)
	}
	if _, err := s.runGit(ctx, "", "--git-dir="+dest, "config", "http.receivepack", "true"); err != nil {
		return fmt.Errorf("seeding %s: enabling receive-pack: %w", repoName, err)
	}
	return nil
}

// SeedRepoFiles seeds repoName as a brand-new repo with a single initial
// commit containing files (repo-relative path -> content). This is the
// seam for tests that need a small repo without an existing fixture on
// disk.
func (s *Server) SeedRepoFiles(ctx context.Context, repoName string, files map[string][]byte, opts SeedOptions) error {
	branch := opts.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	dest := s.repoDir(repoName)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("seeding %s: %w", repoName, errRepoExists)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("seeding %s: %w", repoName, err)
	}
	if _, err := s.runGit(ctx, "", "init", "--bare", "--initial-branch="+branch, dest); err != nil {
		return fmt.Errorf("seeding %s: initializing: %w", repoName, err)
	}
	if _, err := s.runGit(ctx, "", "--git-dir="+dest, "config", "http.receivepack", "true"); err != nil {
		return fmt.Errorf("seeding %s: enabling receive-pack: %w", repoName, err)
	}
	if err := os.MkdirAll(s.workRoot(), 0o755); err != nil {
		return fmt.Errorf("seeding %s: %w", repoName, err)
	}
	tmp, err := os.MkdirTemp(s.workRoot(), "seed-*")
	if err != nil {
		return fmt.Errorf("seeding %s: %w", repoName, err)
	}
	defer os.RemoveAll(tmp)
	if _, err := s.runGit(ctx, "", "clone", dest, tmp); err != nil {
		return fmt.Errorf("seeding %s: cloning scratch: %w", repoName, err)
	}
	for path, content := range files {
		full := filepath.Join(tmp, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("seeding %s: writing %s: %w", repoName, path, err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return fmt.Errorf("seeding %s: writing %s: %w", repoName, path, err)
		}
	}
	if _, err := s.runGit(ctx, tmp, "add", "-A"); err != nil {
		return fmt.Errorf("seeding %s: staging: %w", repoName, err)
	}
	if _, err := s.runGit(ctx, tmp, "commit", "-m", "seed "+repoName); err != nil {
		return fmt.Errorf("seeding %s: committing: %w", repoName, err)
	}
	if _, err := s.runGit(ctx, tmp, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("seeding %s: pushing: %w", repoName, err)
	}
	return nil
}
