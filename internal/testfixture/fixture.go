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
// Constructors and mutation methods take a plain context.Context and return
// an error, so this package works from callers with no *testing.T in scope
// -- notably godog step definitions and Before(scenario) hooks, which have
// neither (see docs/testing-spec.md, the acceptance "Actors" table). NewT,
// NewBareT, and CloneT are convenience wrappers for callers that do have a
// testing.TB: they materialize into tb.TempDir() and fail tb directly
// instead of returning an error.
//
// Every call to New, NewBare, or Clone materializes a brand-new git
// repository rooted in its own directory, with its own object store: no two
// Repo values are implicitly entangled, and mutating one's refs or history
// never mutates another's, so concurrent callers (and concurrent
// t.Parallel subtests) never observe each other's state. Clone is an
// exception by design, not by accident: it wires an "origin" remote at
// src's URL, so an explicit git push/fetch between a Clone and its src does
// move data between them -- exactly the connectivity acceptance-test actor
// workspaces need.
package testfixture

import (
	"context"
	"embed"
	"fmt"
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
// git repository backed by a directory that only this Repo value controls.
// A Repo is either a normal working-tree repository (from New or Clone) or
// a bare repository (from NewBare).
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

// New materializes a fresh working-tree git repository under root, seeded
// with fixture-polyglot's content and committed on the "main" branch. root
// must already exist (a plain temp directory is enough); New only ever
// writes underneath it.
func New(ctx context.Context, root string) (*Repo, error) {
	if err := writeSeed(root); err != nil {
		return nil, fmt.Errorf("materializing fixture-polyglot into %s: %w", root, err)
	}
	if _, err := runGit(ctx, root, "init", "--quiet", "--initial-branch="+defaultBranch); err != nil {
		return nil, fmt.Errorf("git init in %s: %w", root, err)
	}
	if _, err := runGit(ctx, root, "add", "."); err != nil {
		return nil, fmt.Errorf("git add in %s: %w", root, err)
	}
	// Pin author/committer identity and date so the seed commit's content
	// and SHA are identical across every materialization: two fresh Repos
	// start from the exact same commit, only diverging once a caller
	// mutates one of them.
	env := append(fixtureIdentityEnv(), "GIT_AUTHOR_DATE=2020-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2020-01-01T00:00:00Z")
	if _, err := runGitEnv(ctx, root, env, "commit", "--quiet", "-m", "seed: fixture-polyglot initial commit"); err != nil {
		return nil, fmt.Errorf("committing seed in %s: %w", root, err)
	}
	return &Repo{dir: root}, nil
}

// NewT is New for callers with a testing.TB in scope: root is
// tb.TempDir(), and any failure fails tb directly instead of returning an
// error.
func NewT(ctx context.Context, tb testing.TB) *Repo {
	tb.Helper()
	repo, err := New(ctx, tb.TempDir())
	if err != nil {
		tb.Fatalf("%v", err)
	}
	return repo
}

// NewBare materializes a fresh bare repository at root, mirroring a
// throwaway freshly-seeded working tree, suitable for serving as an
// upstream, e.g. the fake forge's smart-HTTP backend (bead loam-li0.1). It
// shares no state with any other Repo: its scratch source tree is deleted
// once the bare clone is made, and the clone uses --no-hardlinks so its
// objects are its own.
func NewBare(ctx context.Context, root string) (*Repo, error) {
	src, err := os.MkdirTemp("", "fixture-polyglot-src-*")
	if err != nil {
		return nil, fmt.Errorf("creating scratch source dir: %w", err)
	}
	defer os.RemoveAll(src)
	if _, err := New(ctx, src); err != nil {
		return nil, fmt.Errorf("seeding scratch source repo: %w", err)
	}
	if _, err := runGit(ctx, "", "clone", "--quiet", "--bare", "--no-hardlinks", src, root); err != nil {
		return nil, fmt.Errorf("cloning bare repo into %s: %w", root, err)
	}
	return &Repo{dir: root, bare: true}, nil
}

// NewBareT is NewBare for callers with a testing.TB in scope: root is
// tb.TempDir(), and any failure fails tb directly instead of returning an
// error.
func NewBareT(ctx context.Context, tb testing.TB) *Repo {
	tb.Helper()
	repo, err := NewBare(ctx, tb.TempDir())
	if err != nil {
		tb.Fatalf("%v", err)
	}
	return repo
}

// Clone materializes a fresh, non-bare working-tree clone of src under
// root, checked out from src's refs as of the moment of the call. The
// clone's "origin" remote points at src.URL(), so an explicit git
// push/fetch does move data between them; short of that, further commits
// or ref changes to either are invisible to the other.
func Clone(ctx context.Context, root string, src *Repo) (*Repo, error) {
	if _, err := runGit(ctx, "", "clone", "--quiet", "--origin", "origin", src.URL(), root); err != nil {
		return nil, fmt.Errorf("cloning %s into %s: %w", src.URL(), root, err)
	}
	return &Repo{dir: root}, nil
}

// CloneT is Clone for callers with a testing.TB in scope: root is
// tb.TempDir(), and any failure fails tb directly instead of returning an
// error.
func CloneT(ctx context.Context, tb testing.TB, src *Repo) *Repo {
	tb.Helper()
	repo, err := Clone(ctx, tb.TempDir(), src)
	if err != nil {
		tb.Fatalf("%v", err)
	}
	return repo
}

// writeSeed copies the embedded fixture-polyglot tree into dir.
func writeSeed(dir string) error {
	return fs.WalkDir(seed, seedRoot, func(path string, d fs.DirEntry, err error) error {
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
}
