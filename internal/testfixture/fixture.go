// Package testfixture materializes isolated, deterministic copies of
// "fixture-polyglot" (see docs/testing-spec.md, "Fixtures") and provides
// scripted git mutations over them.
//
// fixture-polyglot mixes Go, TypeScript, Python, and Markdown so ingestion
// exercises a known symbol graph:
//
//   - pkg/validate (Go) and src/validate.ts (TypeScript) each export a
//     function named Validate -- the ambiguous, same-named symbol across
//     languages that edge-resolution tests use to prove name-based matching
//     stays intra-language.
//   - pkg/report (Go) and src/index.ts (TypeScript) each import their
//     language's Validate from a separate file, giving cross-file reference
//     resolution within a single language something to find.
//   - scripts/parity.py defines is_even and is_odd, two functions that call
//     each other -- a mutual-recursion cycle for the dependents recursive
//     CTE's cycle-safety case.
//   - docs/OVERVIEW.md has multiple top-level "##" sections, one per
//     concept above, for doc-by-section chunking.
//
// This package owns the fixture and its mutations only; the expected golden
// symbol/reference/edge/chunk values are computed and asserted by the
// ingest test suite (see bead loam-li0.8), not here.
//
// Every call to New, NewBare, or Clone produces a brand-new git repository
// rooted in its own temp directory: no two Repo values ever share state, so
// concurrent callers (and concurrent (t.Parallel) subtests) never observe or
// mutate each other's history.
package testfixture

import (
	"context"
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

//go:embed all:testdata/fixture-polyglot
var seed embed.FS

const (
	seedRoot      = "testdata/fixture-polyglot"
	defaultBranch = "main"
)

// Repo is one isolated materialization of fixture-polyglot: a real, on-disk
// git repository backed by a temp directory that only this Repo value
// controls. A Repo is either a normal working-tree repository (from New or
// Clone) or a bare repository (from NewBare).
type Repo struct {
	dir  string
	bare bool
}

// Dir returns the filesystem path to the repository: the working-tree root
// (containing a .git directory) for a non-bare Repo, or the bare git
// directory itself for a bare Repo.
func (r *Repo) Dir() string { return r.dir }

// IsBare reports whether this Repo is a bare repository.
func (r *Repo) IsBare() bool { return r.bare }

// URL returns a file:// URL usable as a git remote for this repo, e.g. to
// clone from it or configure it as an upstream.
func (r *Repo) URL() string { return "file://" + filepath.ToSlash(r.dir) }

// gitDir returns the path git plumbing commands should treat as GIT_DIR.
func (r *Repo) gitDir() string {
	if r.bare {
		return r.dir
	}
	return filepath.Join(r.dir, ".git")
}

// New materializes a fresh working-tree git repository seeded with
// fixture-polyglot's content, committed on the "main" branch. The returned
// Repo lives entirely under tb.TempDir() and needs no separate cleanup.
func New(ctx context.Context, tb testing.TB) *Repo {
	tb.Helper()
	dir := tb.TempDir()
	writeSeed(tb, dir)
	runGit(ctx, tb, dir, "init", "--quiet", "--initial-branch="+defaultBranch)
	runGit(ctx, tb, dir, "add", ".")
	// Pin author/committer identity and date so the seed commit's content
	// and SHA are identical across every materialization: two fresh Repos
	// start from the exact same commit, only diverging once a caller
	// mutates one of them.
	env := append(append([]string{}, identityEnv...), "GIT_AUTHOR_DATE=2020-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2020-01-01T00:00:00Z")
	runGitEnv(ctx, tb, dir, env, "commit", "--quiet", "-m", "seed: fixture-polyglot initial commit")
	return &Repo{dir: dir}
}

// NewBare materializes a fresh bare mirror of a freshly-seeded fixture repo,
// suitable for serving as an upstream, e.g. the fake forge's smart-HTTP
// backend (bead loam-li0.1). It shares no state with any other Repo.
func NewBare(ctx context.Context, tb testing.TB) *Repo {
	tb.Helper()
	src := New(ctx, tb)
	dir := tb.TempDir()
	runGit(ctx, tb, "", "clone", "--quiet", "--bare", src.dir, dir)
	return &Repo{dir: dir, bare: true}
}

// Clone materializes a fresh, non-bare working-tree clone of src into a new
// temp directory, independent of src and of any other clone. Useful for
// acceptance-test actor workspaces that need their own checkout of the
// fixture (see docs/testing-spec.md, the acceptance "Actors" table).
func Clone(ctx context.Context, tb testing.TB, src *Repo) *Repo {
	tb.Helper()
	dir := tb.TempDir()
	runGit(ctx, tb, "", "clone", "--quiet", "--origin", "origin", src.URL(), dir)
	return &Repo{dir: dir}
}

// writeSeed copies the embedded fixture-polyglot tree into dir.
func writeSeed(tb testing.TB, dir string) {
	tb.Helper()
	err := fs.WalkDir(seed, seedRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(seedRoot, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, readErr := seed.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		tb.Fatalf("materializing fixture-polyglot into %s: %v", dir, err)
	}
}
