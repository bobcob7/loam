// Package gitmergetree answers one question against a repo's bare mirror:
// would this work branch still merge into that target tip? It is the git
// half of docs/sync-spec.md's Mergeability Check ("the server tests each
// open work branch targeting that branch against the new tip with `git
// merge-tree` -- no worktree, no writes to any ref"), split out of
// internal/mirrorsync so that package's own orchestration stays free of
// subprocess handling exactly as MirrorFetcher's does (it delegates every
// git invocation to internal/gittransport rather than shelling out
// itself). gittransport itself is the wrong home: every method there
// resolves a forge credential for a host and talks to an upstream, while
// merge-tree is purely local, anonymous, and read-only with respect to
// refs.
//
// # Which merge-tree
//
// git-merge-tree(1) is really two commands sharing a name, and they do not
// merely differ in output -- they answer different questions and report
// their answer through different channels:
//
//   - The legacy trivial-merge form, `git merge-tree <base-tree>
//     <branch1> <branch2>`, takes an explicit merge base, performs only a
//     trivial merge, prints a conflict-marker diff, and ALWAYS exits 0.
//     Exit status carries no signal at all there.
//   - The real-merge form, `git merge-tree --write-tree <branch1>
//     <branch2>` (git >= 2.38), finds the merge base itself, runs the
//     ort merge strategy, writes the resulting tree, and reports the
//     outcome in its exit status.
//
// This package uses the second form exclusively, and never falls back to
// the first: a silent fallback would turn every check into "clean",
// because the legacy form's exit 0 is indistinguishable from success. On a
// git too old to know --write-tree the flag is rejected with exit 129 and
// classified as a check failure below, which is the loud failure that
// belongs here.
//
// # "No writes" means no REF writes
//
// --write-tree is named for what it does: it writes the merged tree (and,
// on conflict, the conflicted blobs) into the mirror's object database.
// Verified empirically against git 2.50.1 -- a clean check on a fresh
// mirror took its loose-object count from 15 to 17 while `git
// for-each-ref` output was byte-identical before and after. That is the
// property docs/sync-spec.md and this bead actually require: no ref is
// created, moved, or deleted, so nothing an agent or the pre-receive
// policy (docs/git-spec.md -> Enforcement Mechanics) can observe changes.
// The objects themselves are unreferenced and are reclaimed by ordinary
// `git gc`. Callers should not read the bead's shorthand "tree written
// in-memory then discarded" literally; nothing about merge-tree is
// in-memory.
//
// # Exit statuses, as observed rather than as assumed
//
// git-merge-tree(1) documents "0 on a clean merge, 1 on conflicts, and
// something other than 0 or 1 if the merge could not be attempted." That
// is NOT what git actually does for every failure, and the gap is the
// dangerous one. Measured against git 2.50.1 with --write-tree:
//
//	case                     exit  stdout                       stderr
//	clean merge              0     "<oid>\n"                    empty
//	conflicting merge        1     "<oid>\n" + conflict detail  empty
//	unknown/unmergeable ref  1     EMPTY                        "merge-tree: <ref> - not something we can merge"
//	unrelated histories      128   EMPTY                        "fatal: refusing to merge unrelated histories"
//	unknown option           129   EMPTY                        usage text
//
// A ref that does not exist exits 1 -- the same status as a conflict --
// so classifying on exit status alone would report a missing work-branch
// ref as "this branch conflicts" and demote a reviewable branch to draft
// over a check that never ran. This package therefore requires BOTH a 0/1
// exit AND a well-formed object ID as the first line of stdout before it
// will believe either answer; anything else is a check failure. That
// stdout validation is also what makes the legacy-form and old-git
// hazards above non-silent. This is the same failure mode
// internal/mirrorsync's parsePorcelainFetch was bitten by (commit
// 5aaf563: fabricating RefUpdates out of interleaved stderr) -- validate
// the shape, then trust it, never the other way round.
package gitmergetree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxStdoutBytes caps how much of merge-tree's stdout is retained. Only
// the first line (an object ID) is ever interpreted; the rest is kept
// solely so a check failure's error message can quote what git actually
// said. A pathological conflict across a very large tree could otherwise
// stream an unbounded conflicted-path listing into this process's memory.
const maxStdoutBytes = 64 << 10

// maxStderrBytes caps retained stderr for the same reason, smaller
// because every stderr this package classifies on is a single short line.
const maxStderrBytes = 8 << 10

// subprocessWaitDelay bounds how long a canceled invocation's git process
// may keep this call's pipes open after the context kills it, matching
// internal/gitdiff and internal/diffplan's own subprocess handling.
const subprocessWaitDelay = 5 * time.Second

// errCheckFailed is the sentinel every "the check itself did not run"
// outcome wraps -- a missing ref, unrelated histories, a git too old for
// --write-tree, a missing mirror, a canceled context. It exists so a
// caller can never confuse "these branches conflict" (reported as
// conflicted=true with a nil error) with "we do not know whether they
// conflict" (a non-nil error): the former demotes a work branch, the
// latter must abort the sync cycle and leave every work branch exactly as
// it was.
var errCheckFailed = errors.New("gitmergetree: merge-tree check did not complete")

// Checker runs merge-tree checks against bare mirrors. Construct with New.
// It holds no per-repo state: mirrorDir is a parameter, so one Checker
// serves every enrolled repo.
type Checker struct {
	logger *slog.Logger
}

// New builds a Checker logging through logger.
func New(logger *slog.Logger) *Checker {
	return &Checker{logger: logger}
}

// MergeTree reports whether merging theirs into ours conflicts, inside the
// bare mirror at mirrorDir. ours and theirs are anything git resolves to a
// commit -- in this tree's one production call site, a work branch's ref
// path ("refs/heads/wb-9c2f1a") and the target branch's newly advanced
// SHA respectively, in that order, so the merge modelled is the one an
// agent catching up would actually run (docs/git-spec.md -> Target
// Advances & Catch-Up: "merge the target into the work branch rather than
// rebase"). Conflict detection is symmetric in the two arguments; the
// order matters only to which side git labels "ours" in output this
// package does not interpret.
//
// It returns (false, nil) for a clean merge and (true, nil) for a
// conflicting one. EVERY other outcome -- a ref that does not resolve,
// histories with no common ancestor, a missing or corrupt mirror, a git
// without --write-tree, a canceled context -- is a non-nil error wrapping
// errCheckFailed, never (true, nil). See the package doc comment for the
// exit statuses and stdout shapes that classification is built on, and
// why exit status alone is not enough.
//
// No ref in mirrorDir is created, moved, or deleted. Merged tree and blob
// objects are written to the object database; see the package doc
// comment.
func (c *Checker) MergeTree(ctx context.Context, mirrorDir, ours, theirs string) (bool, error) {
	out, err := c.run(ctx, mirrorDir, "merge-tree", "--write-tree", ours, theirs)
	if err != nil {
		return false, fmt.Errorf("merge-testing %s against %s in %s: %w", ours, theirs, mirrorDir, err)
	}
	conflicted, err := classify(out)
	if err != nil {
		return false, fmt.Errorf("merge-testing %s against %s in %s: %w", ours, theirs, mirrorDir, err)
	}
	c.logger.DebugContext(ctx, "merge-tree check completed", "mirror_dir", mirrorDir, "ours", ours, "theirs", theirs, "conflicted", conflicted)
	return conflicted, nil
}

// gitOutput is one subprocess invocation's classified result, matching
// internal/gitdiff's and internal/diffplan's own shape: a nonzero exit is
// data here, not a Go error, because merge-tree's exit 1 is a legitimate
// answer rather than a failure.
type gitOutput struct {
	stdout   []byte
	exitCode int
	stderr   string
}

// classify turns one merge-tree invocation's output into the clean/
// conflicting/failed trichotomy. It insists on a well-formed object ID as
// stdout's first line before believing either of exit 0 and exit 1,
// because git reports an unresolvable ref with exit 1 and an empty
// stdout -- see the package doc comment's table. An exit-1 answer with no
// object ID is therefore a failure, not a conflict.
func classify(out gitOutput) (bool, error) {
	if out.exitCode != 0 && out.exitCode != 1 {
		return false, fmt.Errorf("%w: git exited %d: %s", errCheckFailed, out.exitCode, summarize(out))
	}
	if !hasObjectIDFirstLine(out.stdout) {
		return false, fmt.Errorf("%w: git exited %d without a merged tree object id on stdout: %s", errCheckFailed, out.exitCode, summarize(out))
	}
	return out.exitCode == 1, nil
}

// hasObjectIDFirstLine reports whether stdout's first line is exactly a
// git object ID -- 40 lowercase hex characters for a SHA-1 repository, 64
// for a SHA-256 one (git's own object-format sizes; both are accepted so
// this never depends on how a mirror was initialized). Nothing shorter,
// longer, or with trailing text on the same line counts.
func hasObjectIDFirstLine(stdout []byte) bool {
	line, _, _ := bytes.Cut(stdout, []byte("\n"))
	if len(line) != 40 && len(line) != 64 {
		return false
	}
	for _, b := range line {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

// summarize renders whichever of stderr and stdout git actually produced,
// for a failure message. stderr is preferred because every classified
// failure above puts its explanation there; stdout is the fallback for a
// hypothetical failure that reported on the other stream.
func summarize(out gitOutput) string {
	if s := strings.TrimSpace(out.stderr); s != "" {
		return s
	}
	if s := strings.TrimSpace(string(out.stdout)); s != "" {
		return s
	}
	return "(no output)"
}

// run executes one git subcommand against mirrorDir (via --git-dir, never
// -C: a bare mirror has no working tree to change into), isolated from the
// host and user gitconfig. A nonzero exit is returned in gitOutput rather
// than as an error -- only a failure to run git at all, or a context
// canceled before or during the run, comes back as err, wrapped in
// errCheckFailed so every caller-visible failure from this package shares
// one sentinel.
func (c *Checker) run(ctx context.Context, mirrorDir string, args ...string) (gitOutput, error) {
	home, err := os.MkdirTemp("", "loam-gitmergetree-*")
	if err != nil {
		return gitOutput{}, fmt.Errorf("%w: creating isolated git environment: %w", errCheckFailed, err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	fullArgs := append([]string{"--no-pager", "-c", "credential.helper=", "--git-dir=" + mirrorDir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.WaitDelay = subprocessWaitDelay
	cmd.Env = gitEnv(home)
	outBuf := &cappedBuffer{max: maxStdoutBytes}
	errBuf := &cappedBuffer{max: maxStderrBytes}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	runErr := cmd.Run()
	if runErr == nil {
		return gitOutput{stdout: outBuf.buf.Bytes(), exitCode: 0, stderr: errBuf.buf.String()}, nil
	}
	return classifyRunErr(ctx, runErr, outBuf.buf.Bytes(), errBuf.buf.String(), args)
}

// classifyRunErr decides what a failed cmd.Run means. Split out of run so
// it can be exercised deterministically: the context-cancellation branch
// below is only reachable in a real run when cancellation lands AFTER git
// starts, which no test can schedule reliably.
//
// The context check comes FIRST, ahead of the ExitError path.
// exec.CommandContext kills the subprocess when the context is done, and
// a killed process surfaces as an ordinary *exec.ExitError -- so without
// this ordering a cancellation mid-check would be classified as git's own
// answer and reported with whatever exit status the kill produced. It
// would still be an error today (a signal-killed process reports exit
// code -1, which classify rejects), but only by accident of that number
// not being 0 or 1; checking the context explicitly makes it true by
// construction and keeps context.Canceled/DeadlineExceeded in the error
// chain, so a caller can tell "we shut down mid-check" from "git broke".
func classifyRunErr(ctx context.Context, runErr error, stdout []byte, stderr string, args []string) (gitOutput, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return gitOutput{}, fmt.Errorf("%w: running git %v: %w", errCheckFailed, args, ctxErr)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return gitOutput{stdout: stdout, exitCode: exitErr.ExitCode(), stderr: stderr}, nil
	}
	return gitOutput{}, fmt.Errorf("%w: running git %v: %w", errCheckFailed, args, runErr)
}

// gitEnv builds the environment for one git subprocess invocation, an
// explicit minimal list rather than os.Environ() plus additions -- the
// same shape and rationale as internal/gitdiff's and internal/diffplan's
// own gitEnv. GIT_CONFIG_NOSYSTEM plus the redirected HOME/
// XDG_CONFIG_HOME/GIT_CONFIG_GLOBAL mean no system, user-global, or
// ambient GIT_CONFIG_GLOBAL-pointed config is ever read: on macOS the
// Command Line Tools ship a system gitconfig, and a merge driver,
// merge.conflictStyle, core.autocrlf, or a rerere setting picked up from
// it could change this check's verdict from one developer machine to
// another. GIT_PAGER=cat plus the invocation's own --no-pager doubly
// guard against core.pager blocking on a tty this subprocess does not
// have; the GIT_TRACE* overrides are carried over from
// internal/gittransport's gitEnv on the same belt-and-suspenders
// reasoning, even though this package injects no credential to leak.
// GIT_CURL_VERBOSE is deliberately not one of them: git only
// presence-checks that variable, so "=0" would turn curl tracing on;
// leaving it off this explicit allowlist is what keeps it off.
func gitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "unused-global-gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_PAGER=cat",
		"GIT_TRACE=0",
		"GIT_TRACE_CURL=0",
		"GIT_TRACE_PACKET=0",
		"GIT_TRACE_PACK_ACCESS=0",
		"GIT_TRACE_SETUP=0",
	}
}

// cappedBuffer is an io.Writer retaining only the first max bytes ever
// written, matching internal/gitdiff's own cappedBuffer.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

// Write implements io.Writer.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		c.buf.Write(p[:room])
	}
	return len(p), nil
}
