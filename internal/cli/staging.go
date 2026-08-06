package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// loamDirName is the per-workspace Loam directory (docs/cli-spec.md ->
// comment (add), "Staging location": "the workspace's .loam/ directory"),
// and stagingDirName is the staging tree inside it.
const (
	loamDirName    = ".loam"
	stagingDirName = "staging"
)

// stagingDirPerm and stagingFilePerm are the modes every staging directory
// and staged file is created with. Staged comments are the caller's own
// unpublished review notes (docs/cli-spec.md -> comment (add): "stay
// invisible to everyone else until verdict publishes them"), so they are
// owner-only rather than the usual 0o755/0o644. The perms are fixed here
// instead of taken from the caller so no future writer can widen them by
// passing a laxer mode.
const (
	stagingDirPerm  = 0o700
	stagingFilePerm = 0o600
)

// errStagingArea is the underlying cause when the local staging area
// cannot be opened or written. Its most important case is a containment
// refusal: some component of the staging path is a symbolic link leading
// outside the staging root, so the operation would have written outside
// the workspace. That refusal comes from os.Root, which resolves every
// component against a directory file descriptor at the SYSCALL level —
// StagingPath's lexical guards (an allowlist plus filepath.IsLocal) cannot
// see symlinks at all, because filepath.IsLocal is by its own godoc
// "purely lexical" and "does not account for the effect of any symbolic
// links". Ordinary I/O failures (a full disk, a read-only workspace) also
// surface here; the wrapped cause distinguishes them.
var errStagingArea = errors.New("staging area unavailable")

// OpenStaging opens the local staging area for repo/workBranch, keyed also
// by the calling agent's identifier, under the workspace root's .loam/
// directory (see docs/cli-spec.md -> "Staging location"), creating it and
// any missing parents if needed.
//
// Every directory it creates and every file the returned StagingArea
// touches is resolved through os.Root handles rooted at the workspace root
// and then at the staging root. That is what makes containment structural
// rather than advisory: os.Root refuses to traverse a symbolic link whose
// target lies outside its root, so a pre-planted .loam/staging/<group>
// symlink pointing anywhere outside the workspace makes the directory
// creation fail instead of silently relocating the whole staging tree. The
// check is re-done against the live filesystem on every component of every
// operation, so it also holds when a symlink appears after the key was
// validated.
//
// The workspace never hands out a raw staging path string: a caller can
// only reach staged files through the returned StagingArea, so there is no
// way to obtain a path and then write to it with plain os.WriteFile,
// bypassing containment. Callers must Close the area when done.
func (w *workspace) OpenStaging(repo, workBranch string) (StagingArea, error) {
	if w.rootErr != nil {
		return nil, w.rootErr
	}
	rel, err := w.stagingRel(repo, workBranch)
	if err != nil {
		return nil, err
	}
	workspaceRoot, err := os.OpenRoot(w.root)
	if err != nil {
		return nil, fmt.Errorf("%w: opening workspace root %s: %w", errStagingArea, w.root, err)
	}
	defer workspaceRoot.Close()
	stagingRoot, err := makeContainedDir(workspaceRoot, filepath.Join(loamDirName, stagingDirName))
	if err != nil {
		return nil, fmt.Errorf("%w: creating %s under workspace root %s: %w", errStagingArea, filepath.Join(loamDirName, stagingDirName), w.root, err)
	}
	defer stagingRoot.Close()
	areaRoot, err := makeContainedDir(stagingRoot, rel)
	if err != nil {
		return nil, fmt.Errorf("%w: creating %s under staging root %s: %w", errStagingArea, rel, stagingRoot.Name(), err)
	}
	area := &stagingArea{root: areaRoot}
	if err := w.adoptLegacyStaging(area, rel); err != nil {
		_ = area.Close()
		return nil, err
	}
	return area, nil
}

// adoptLegacyStaging moves a staged.json left behind at the OLD,
// cwd-derived staging location into the area just opened at the configured
// one, when the new location has none of its own.
//
// It exists because the fix for loam-rgyg MOVES where staged comments
// live. Without this, deploying that fix would itself do the thing the bug
// does: a reviewer who staged comments before the upgrade and published
// after it would find an empty staging area and publish a verdict with
// none of their findings attached. A migration that silently strands data
// is not an acceptable fix for silently stranding data.
//
// It is deliberately conservative in three ways. It never runs when the
// new location already holds a staged.json, so it cannot overwrite live
// staged state. It never DELETES the legacy file — a copy that leaves the
// original readable is recoverable by hand if anything about this is
// wrong, and the legacy file is inert once the new one exists. And it
// reports its failures instead of swallowing them: a legacy document that
// exists but cannot be read or re-written is precisely the case where
// carrying on would lose comments, so it fails the command loudly rather
// than presenting an empty staging area as the answer.
func (w *workspace) adoptLegacyStaging(area *stagingArea, rel string) error {
	if w.legacyRoot == "" || w.legacyRoot == w.root {
		return nil
	}
	if _, err := area.ReadFile(stagedFileName); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	legacy := filepath.Join(w.legacyRoot, loamDirName, stagingDirName, rel, stagedFileName)
	data, err := os.ReadFile(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: reading staged comments left at the previous staging location %s: %w", errStagingArea, legacy, err)
	}
	if err := area.WriteFile(stagedFileName, data); err != nil {
		return fmt.Errorf("%w: carrying staged comments over from the previous staging location %s: %w", errStagingArea, legacy, err)
	}
	return nil
}

// makeContainedDir creates name (and any missing parents) under parent and
// returns it as a root of its own, so subsequent operations are confined to
// it and not merely to parent. Both steps go through parent's os.Root, so a
// symlinked component anywhere in name that leaves parent is refused rather
// than followed — this is the step the demonstrated bypass went through,
// where a plain os.MkdirAll would happily create the tree at the symlink's
// target.
func makeContainedDir(parent *os.Root, name string) (*os.Root, error) {
	if err := parent.MkdirAll(name, stagingDirPerm); err != nil {
		return nil, err
	}
	return parent.OpenRoot(name)
}

// stagingArea is the only handle through which staged files are read,
// written, or removed. It holds an os.Root pinned to one agent's staging
// directory for one repo/work-branch pair; every method resolves its name
// argument against that root, so nothing it does can touch a file outside
// it — including through a symlink planted inside the directory after the
// area was opened. Implements StagingArea.
type stagingArea struct {
	root *os.Root
}

// entryName validates name as a single staging entry: exactly one path
// segment matching the same allowlist staging keys use. Rejecting "/" here
// keeps staged items flat and gives a usage error (exit 2) with a readable
// message for the ordinary mistakes — a nested or absolute name, a "..",
// an empty name — instead of an opaque syscall error. It is a convenience,
// NOT the containment mechanism: os.Root refuses an escaping name whether
// or not this check runs.
func entryName(name string) error {
	if err := validateStagingKey("staged entry", name, false); err != nil {
		return newUsageCLIError(err.Error(), err)
	}
	return nil
}

// WriteFile implements StagingArea.
func (a *stagingArea) WriteFile(name string, data []byte) error {
	if err := entryName(name); err != nil {
		return err
	}
	if err := a.root.WriteFile(name, data, stagingFilePerm); err != nil {
		return fmt.Errorf("%w: writing %s in %s: %w", errStagingArea, name, a.root.Name(), err)
	}
	return nil
}

// ReadFile implements StagingArea.
func (a *stagingArea) ReadFile(name string) ([]byte, error) {
	if err := entryName(name); err != nil {
		return nil, err
	}
	data, err := a.root.ReadFile(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: reading %s in %s: %w", errStagingArea, name, a.root.Name(), err)
	}
	return data, nil
}

// Remove implements StagingArea.
func (a *stagingArea) Remove(name string) error {
	if err := entryName(name); err != nil {
		return err
	}
	if err := a.root.Remove(name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return err
		}
		return fmt.Errorf("%w: removing %s in %s: %w", errStagingArea, name, a.root.Name(), err)
	}
	return nil
}

// Location implements StagingArea.
func (a *stagingArea) Location() string { return a.root.Name() }

// Close implements StagingArea.
func (a *stagingArea) Close() error { return a.root.Close() }
