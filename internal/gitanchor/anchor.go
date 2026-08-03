// Package gitanchor answers one question against a repo's bare mirror: how
// many lines does a file have in a work branch's own ref, right now? It
// exists to fix loam-hi5o.15 -- `loam work comment --line 270` against a
// ~100-line file was accepted with no validation anywhere, publishing a
// review comment anchored past the end of the file, durable and
// unremarked.
//
// # Why this reads the mirror instead of trusting the caller
//
// Comment staging (`loam work comment`) happens entirely in the CLI's local
// .loam and never touches the server (proto/loam/v1/workbranch.proto's own
// package doc comment: "only SubmitVerdict crosses the wire"). The server
// is therefore the ONLY party that can be authoritative about what the
// work branch's tip actually contains at the moment a verdict publishes --
// a reviewer's local checkout, if they have one at all, may be stale,
// missing, or simply never consulted (docs/cli-spec.md's own workflow lets
// an agent comment from `work diff` output with no checkout whatsoever).
// Validating client-side would validate against the wrong file, or nothing.
//
// # Reusing the established mirror-reading mechanism, not inventing one
//
// Every piece here is deliberately the same plumbing internal/gitdiff (the
// diff path) and internal/ingest/orchestrator's gitReader (the graph path)
// already use: internal/mirrorpath.Dir locates the bare mirror on disk,
// internal/gitrun launches the subprocess with the same isolated,
// hardened environment (--git-dir, never -C; GIT_CONFIG_NOSYSTEM plus a
// redirected HOME; --no-pager; capped stderr), and verifyRef below is
// gitdiff.Computer.verifyRef's exact classification of `git rev-parse
// --verify --quiet` copied for this package's own ref (there is no shared
// export of it to call instead -- gitrun's own package doc comment
// explains why only the launch mechanics, not the classification, are
// shared). What is genuinely new is the two-step blob read
// (checkBlob/countLines below): neither existing caller needed "is this
// path a blob, and how many lines does it have," so there was nothing to
// reuse for that specific answer.
//
// # Streaming, not buffering, the blob
//
// countLines never materializes a file's content in memory: git's stdout is
// wired directly to a lineCounter that only ever holds a running count and
// the last byte seen, so a source file of any realistic size costs O(1)
// memory here, unlike internal/gitdiff's own maxDiffBytes-capped buffer
// (which exists because a diff has no alternative sink -- it IS the
// response). There is nothing here for a caller to read afterward, so there
// is nothing to cap.
package gitanchor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/bobcob7/loam/internal/gitrun"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/refnames"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// maxStderrBytes bounds captured stderr the same way internal/gitdiff and
// internal/ingest/orchestrator do -- git's own error output is a few lines,
// never proportional to repository or blob size.
const maxStderrBytes = 64 << 10

// ErrMirrorMissing indicates the repo's bare mirror does not exist on disk,
// or the path mirrorpath.Dir derived is not a valid git repository at all
// -- an operational fault (the repo is enrolled but its mirror is absent or
// corrupt), not a caller mistake. Matches internal/gitdiff.ErrMirrorMissing.
var ErrMirrorMissing = errors.New("gitanchor: bare mirror missing or invalid on disk")

// ErrRefMissing indicates the work branch's own ref does not exist in the
// mirror. Every genuinely created work branch has one (docs/git-spec.md ->
// "Ref Policy": created server-side by `work start`), so this signals the
// mirror has fallen out of sync with the work-branch registry, not that the
// caller named something invalid. Matches internal/gitdiff.ErrRefMissing.
var ErrRefMissing = errors.New("gitanchor: work branch ref not found in mirror")

// ErrFileNotFound indicates file is not a blob at the work branch's tip --
// either no tree entry names it at all, or the entry there is not a blob
// (a directory, or a submodule gitlink). Both are the caller's own mistake
// (a comment anchored to a path the diff never touched, or a stale path
// from before the author's last push), reported identically since neither
// is anything the caller can act on differently.
var ErrFileNotFound = errors.New("gitanchor: file not found at the work branch tip")

// Checker answers file-line-count questions against the bare mirror at
// mirrorpath.Dir(dataDir, repoName), for the work branch's OWN ref
// (refnames.WorkBranch) -- never the target's, since an anchor names a
// position in the work branch's post-image, not its base.
type Checker struct {
	dataDir string
	repos   RepoStore
}

// New builds a Checker rooted at dataDir (LOAM_DATA_DIR), resolving a work
// branch's repo name via repos before deriving its mirror path -- the same
// shape as internal/gitdiff.New.
func New(dataDir string, repos RepoStore) *Checker {
	return &Checker{dataDir: dataDir, repos: repos}
}

// FileLineCount returns file's line count in wb's own ref at the mirror's
// current tip -- the "actual length" a rejected anchor's error message
// names. A line is delimited by '\n'; a final line with no trailing
// newline still counts (see lineCounter), matching what an editor or `git
// diff` line number would show, and an entirely empty file counts as zero.
//
// Returns ErrFileNotFound when file is not a blob there at all (missing, a
// directory, or a submodule gitlink) -- callers reject that the same way
// they reject an out-of-range line, both being the caller's own mistake.
func (c *Checker) FileLineCount(ctx context.Context, wb workbranchstore.WorkBranch, file string) (int, error) {
	repo, err := c.repos.GetRepoByID(ctx, wb.RepoID)
	if err != nil {
		return 0, fmt.Errorf("resolving repo for work branch %s: %w", wb.Name, err)
	}
	mirrorDir := mirrorpath.Dir(c.dataDir, repo.Name)
	ref := refnames.WorkBranch(wb.Name)
	if err := c.verifyRef(ctx, mirrorDir, wb.Name, ref); err != nil {
		return 0, fmt.Errorf("verifying work branch %s: %w", wb.Name, err)
	}
	isBlob, err := c.isBlob(ctx, mirrorDir, ref, file)
	if err != nil {
		return 0, fmt.Errorf("checking %s at %s: %w", file, wb.Name, err)
	}
	if !isBlob {
		return 0, fmt.Errorf("%s: %w", file, ErrFileNotFound)
	}
	lines, err := c.countLines(ctx, mirrorDir, ref, file)
	if err != nil {
		return 0, fmt.Errorf("counting lines in %s at %s: %w", file, wb.Name, err)
	}
	return lines, nil
}

// verifyRef confirms ref exists in the mirror at mirrorDir, via `git
// rev-parse --verify --quiet` -- byte-for-byte the same classification
// internal/gitdiff.Computer.verifyRef established and documents in full:
// exit 0 (exists), exit 1 with no stderr under --quiet (ErrRefMissing), or
// exit 128 with "not a git repository" in stderr (ErrMirrorMissing).
func (c *Checker) verifyRef(ctx context.Context, mirrorDir, name, ref string) error {
	out, err := c.run(ctx, mirrorDir, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return err
	}
	switch out.exitCode {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%s: %w", name, ErrRefMissing)
	default:
		if isMirrorMissingStderr(out.stderr) {
			return fmt.Errorf("%s: %w", mirrorDir, ErrMirrorMissing)
		}
		return fmt.Errorf("git rev-parse %s exited %d: %s", ref, out.exitCode, strings.TrimSpace(out.stderr))
	}
}

// isBlob reports whether ref:file names a blob, via `git cat-file -t`.
// This check exists as its own step, before countLines ever runs, because
// `git cat-file -p <ref>:<path>` does NOT fail on a tree (directory): it
// pretty-prints the tree's entries, which countLines would otherwise
// happily misread as file content and report a bogus, plausible-looking
// line count. A missing path (the common case: a comment anchored to a
// path the diff never touched) is classified here too, as "not a blob"
// rather than a distinguishable error -- verifyRef has already ruled out a
// broken ref or mirror by the time this runs, so any other nonzero exit
// here is git's own "path does not exist in that tree" complaint.
func (c *Checker) isBlob(ctx context.Context, mirrorDir, ref, file string) (bool, error) {
	out, err := c.run(ctx, mirrorDir, "cat-file", "-t", ref+":"+file)
	if err != nil {
		return false, err
	}
	if out.exitCode != 0 {
		if isMirrorMissingStderr(out.stderr) {
			return false, fmt.Errorf("%s: %w", mirrorDir, ErrMirrorMissing)
		}
		return false, nil
	}
	return strings.TrimSpace(out.stdout) == "blob", nil
}

// countLines streams ref:file's content through `git cat-file -p` straight
// into a lineCounter -- see the package doc comment for why this never
// buffers the content itself. isBlob has already confirmed the object is a
// blob, so a nonzero exit here would mean the mirror changed underneath
// this call (raced with a concurrent write) or git itself failed; either is
// reported as a plain error rather than folded into ErrFileNotFound, which
// is reserved for isBlob's own, already-classified answer.
func (c *Checker) countLines(ctx context.Context, mirrorDir, ref, file string) (int, error) {
	counter := &lineCounter{}
	exitCode, stderr, err := c.runInto(ctx, mirrorDir, counter, "cat-file", "-p", ref+":"+file)
	if err != nil {
		return 0, err
	}
	if exitCode != 0 {
		return 0, fmt.Errorf("git cat-file -p %s:%s exited %d: %s", ref, file, exitCode, strings.TrimSpace(stderr))
	}
	return counter.count(), nil
}

// lineCounter is an io.Writer that counts '\n' bytes as they stream past,
// without retaining any of the content -- see the package doc comment.
// count() adds one more for a trailing partial line (content that does not
// end in '\n'), matching what a text editor's line gutter would show;
// zero bytes written means zero lines, not one.
type lineCounter struct {
	newlines int
	sawByte  bool
	lastByte byte
}

// Write implements io.Writer.
func (l *lineCounter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			l.newlines++
		}
	}
	if len(p) > 0 {
		l.sawByte = true
		l.lastByte = p[len(p)-1]
	}
	return len(p), nil
}

// count returns the number of lines seen so far.
func (l *lineCounter) count() int {
	if !l.sawByte {
		return 0
	}
	if l.lastByte != '\n' {
		return l.newlines + 1
	}
	return l.newlines
}

// isMirrorMissingStderr reports whether stderr is git's own "not a git
// repository" complaint about a bad --git-dir, checked case-insensitively
// for the same reason internal/gitdiff's function of the same name
// documents: different git subcommands word it with different
// capitalization.
func isMirrorMissingStderr(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "not a git repository")
}

// gitOutput is one subprocess invocation's classified result, matching
// internal/gitdiff's type of the same name.
type gitOutput struct {
	stdout   string
	exitCode int
	stderr   string
}

// run executes one git subcommand against mirrorDir, capturing stdout as a
// string (bounded implicitly -- every caller of run here reads a single
// short line: a ref hash or an object type, never blob content) and stderr
// capped at maxStderrBytes.
func (c *Checker) run(ctx context.Context, mirrorDir string, args ...string) (gitOutput, error) {
	var stdout strings.Builder
	exitCode, stderr, err := c.runInto(ctx, mirrorDir, &stdout, args...)
	if err != nil {
		return gitOutput{}, err
	}
	return gitOutput{stdout: stdout.String(), exitCode: exitCode, stderr: stderr}, nil
}

// runInto executes one git subcommand against mirrorDir (via --git-dir,
// never -C: the latter performs upward repository discovery and can
// silently operate on an enclosing repository instead of failing),
// isolated from the host and user gitconfig, streaming stdout into out
// directly rather than buffering it here -- what lets countLines above cap
// its own memory use at O(1) regardless of blob size. A nonzero git exit is
// not itself a Go error -- exitCode reports it for the caller to classify
// against stderr; only a failure to even run git is returned as err.
func (c *Checker) runInto(ctx context.Context, mirrorDir string, out io.Writer, args ...string) (int, string, error) {
	home, cleanup, err := gitrun.NewIsolatedHome()
	if err != nil {
		return 0, "", err
	}
	defer cleanup()
	errBuf := gitrun.NewCappedBuffer(maxStderrBytes)
	cmd := gitrun.NewCommand(ctx, gitrun.Env(home), nil, out, errBuf, gitrun.GitDirArgs(mirrorDir, args...)...)
	runErr := cmd.Run()
	if runErr == nil {
		return 0, errBuf.String(), nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.ExitCode(), errBuf.String(), nil
	}
	return 0, "", fmt.Errorf("running git %v: %w", args, runErr)
}
