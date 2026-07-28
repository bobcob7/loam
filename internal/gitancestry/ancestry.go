// Package gitancestry answers one question against a repo's bare mirror:
// does this commit's history CONTAIN that other commit? It is the git half
// of docs/git-spec.md -> "Target Advances & Catch-Up"'s recovery rule ("an
// agent pushes commits that bring the branch up to date (its history
// contains the current target tip)"), split out of the catch-up detector
// itself for the same reason internal/gitmergetree was split out of
// internal/mirrorsync: subprocess handling stays in one small package with
// its own isolated environment, and the orchestration above it stays
// testable against a seam.
//
// # Contains, not "is descended from"
//
// `git merge-base --is-ancestor A B` is true when A IS B, not only when B
// is strictly newer. That is deliberately what this package reports:
// docs/git-spec.md's criterion is CONTAINMENT ("its history contains the
// current target tip"), and a work branch whose tip is exactly the target
// tip has plainly caught up. A strict-descendant test would refuse to
// clear the flag in that case, which is the one shape a fast-forward
// catch-up produces most often.
//
// # Exit status IS the answer here (unlike merge-tree)
//
// git-merge-base(1) documents --is-ancestor as "exit with status 0 if
// true, or with status 1 if not" and, explicitly, "Errors are signaled by
// a non-zero status that is not 1." That contract holds in practice --
// measured against git 2.50.1:
//
//	case                        exit  stderr
//	A is an ancestor of B       0     empty
//	A is not an ancestor of B   1     empty
//	A and B unrelated           1     empty
//	either rev does not exist   128   "fatal: Not a valid object name ..."
//	unknown option              129   usage text
//
// This is the opposite of internal/gitmergetree's situation, where an
// unresolvable ref and a genuine conflict SHARE exit 1 and stdout has to
// be validated before either answer can be believed. Here the two are
// already distinct statuses, so this package classifies on exit status
// alone -- and treats every status other than 0 and 1 as a check failure
// rather than as "not caught up", so a broken mirror or a bad SHA never
// silently reads as "still behind."
//
// # Reading objects that have not landed yet
//
// A pre-receive hook runs while the pushed objects are still in
// receive-pack's QUARANTINE directory, which is NOT part of the bare
// mirror's own object store: a separate `git --git-dir=<mirror>` process
// cannot see the pushed tip at all (verified against git 2.50.1 -- a clean
// `git cat-file -t <new-sha>` from outside the hook's environment fails
// "could not get object info"). Contains therefore takes extraObjectDir
// and exposes it through GIT_ALTERNATE_OBJECT_DIRECTORIES, which ADDS an
// object store rather than replacing the mirror's own (GIT_OBJECT_DIRECTORY
// would replace it, hiding the target tip this check needs). An empty
// extraObjectDir adds nothing, so a caller reading objects that have
// already landed passes "".
package gitancestry

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

// maxStderrBytes caps retained stderr for a failed check, matching
// internal/gitmergetree's own cap and for the same reason: every stderr
// this package classifies on is a single short line, retained solely so a
// failure message can quote what git actually said.
const maxStderrBytes = 8 << 10

// subprocessWaitDelay bounds how long a canceled invocation's git process
// may keep this call's pipes open after the context kills it, matching
// internal/gitmergetree, internal/gitdiff, and internal/diffplan.
const subprocessWaitDelay = 5 * time.Second

// errCheckFailed is the sentinel every "the check itself did not run"
// outcome wraps -- an unresolvable rev, a missing mirror, a git that
// rejected the invocation, a canceled context. It exists so a caller can
// never confuse "this history does not contain that commit" (reported as
// false with a nil error, and which for the catch-up detector means "leave
// the conflict flag alone") with "we do not know" (a non-nil error, which
// must leave the work branch untouched rather than assume either answer).
var errCheckFailed = errors.New("gitancestry: ancestry check did not complete")

// errUnsafeRev rejects a rev that git would parse as an option rather than
// as a revision. Both of this package's production arguments are
// constructed, not user-supplied (a "refs/heads/<target>" path and a
// validated object id), so this can only fire on a caller bug -- but it
// fires as an error rather than reaching git's argv, where a leading dash
// would silently change what was executed.
var errUnsafeRev = errors.New("gitancestry: rev must be non-empty and must not begin with '-'")

// Checker runs ancestry checks against bare mirrors. Construct with New.
// It holds no per-repo state: mirrorDir is a parameter, so one Checker
// serves every enrolled repo.
type Checker struct {
	logger *slog.Logger
}

// New builds a Checker logging through logger.
func New(logger *slog.Logger) *Checker {
	return &Checker{logger: logger}
}

// Contains reports whether descendant's history contains ancestor, inside
// the bare mirror at mirrorDir, additionally reading objects from
// extraObjectDir when it is non-empty (see the package doc comment for why
// a pre-receive caller must pass receive-pack's quarantine directory
// there).
//
// It returns (true, nil) when ancestor is reachable from descendant --
// including when the two name the same commit -- and (false, nil) when it
// is not, including for two entirely unrelated histories. EVERY other
// outcome -- a rev that does not resolve, a missing or corrupt mirror, a
// git that rejected the invocation, a canceled context -- is a non-nil
// error wrapping errCheckFailed, never (false, nil): "we could not check"
// must not be mistaken for "not caught up."
//
// No ref and no object in mirrorDir is created, moved, or deleted;
// merge-base only reads.
func (c *Checker) Contains(ctx context.Context, mirrorDir, extraObjectDir, ancestor, descendant string) (bool, error) {
	if err := checkRev(ancestor); err != nil {
		return false, fmt.Errorf("checking whether %s contains %s in %s: ancestor: %w", descendant, ancestor, mirrorDir, err)
	}
	if err := checkRev(descendant); err != nil {
		return false, fmt.Errorf("checking whether %s contains %s in %s: descendant: %w", descendant, ancestor, mirrorDir, err)
	}
	exitCode, stderr, err := c.run(ctx, mirrorDir, extraObjectDir, "merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		return false, fmt.Errorf("checking whether %s contains %s in %s: %w", descendant, ancestor, mirrorDir, err)
	}
	if exitCode != 0 && exitCode != 1 {
		return false, fmt.Errorf("checking whether %s contains %s in %s: %w: git exited %d: %s", descendant, ancestor, mirrorDir, errCheckFailed, exitCode, summarize(stderr))
	}
	contains := exitCode == 0
	c.logger.DebugContext(ctx, "ancestry check completed", "mirror_dir", mirrorDir, "ancestor", ancestor, "descendant", descendant, "contains", contains)
	return contains, nil
}

// checkRev rejects a rev git would read as an option. See errUnsafeRev.
func checkRev(rev string) error {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return fmt.Errorf("%w (got %q)", errUnsafeRev, rev)
	}
	return nil
}

// summarize renders git's stderr for a failure message, or a placeholder
// when git said nothing at all.
func summarize(stderr string) string {
	if s := strings.TrimSpace(stderr); s != "" {
		return s
	}
	return "(no output)"
}

// run executes one git subcommand against mirrorDir (via --git-dir, never
// -C: a bare mirror has no working tree to change into), isolated from the
// host and user gitconfig. A nonzero exit is returned as exitCode rather
// than as an error -- merge-base's exit 1 is a legitimate answer, not a
// failure -- so only a failure to run git at all, or a context canceled
// before or during the run, comes back as err.
func (c *Checker) run(ctx context.Context, mirrorDir, extraObjectDir string, args ...string) (exitCode int, stderr string, err error) {
	home, err := os.MkdirTemp("", "loam-gitancestry-*")
	if err != nil {
		return 0, "", fmt.Errorf("%w: creating isolated git environment: %w", errCheckFailed, err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	fullArgs := append([]string{"--no-pager", "-c", "credential.helper=", "--git-dir=" + mirrorDir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.WaitDelay = subprocessWaitDelay
	cmd.Env = gitEnv(home, extraObjectDir)
	errBuf := &cappedBuffer{max: maxStderrBytes}
	cmd.Stdout = nil
	cmd.Stderr = errBuf
	runErr := cmd.Run()
	if runErr == nil {
		return 0, errBuf.buf.String(), nil
	}
	return classifyRunErr(ctx, runErr, errBuf.buf.String(), args)
}

// classifyRunErr decides what a failed cmd.Run means. Split out of run so
// the context-cancellation branch can be exercised deterministically:
// cancellation landing AFTER git starts is not something a test can
// schedule reliably in a real run.
//
// The context check comes FIRST, ahead of the ExitError path, for exactly
// the reason internal/gitmergetree's own classifyRunErr documents:
// exec.CommandContext kills the subprocess when the context is done and a
// killed process surfaces as an ordinary *exec.ExitError, so without this
// ordering a cancellation mid-check would be reported as git's own answer.
// Here that would be worse than in gitmergetree, because a signal-killed
// process's -1 exit code is not 0 or 1 and would already be an error --
// but the error would blame git rather than the shutdown, and
// context.Canceled would be lost from the chain.
func classifyRunErr(ctx context.Context, runErr error, stderr string, args []string) (int, string, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return 0, "", fmt.Errorf("%w: running git %v: %w", errCheckFailed, args, ctxErr)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.ExitCode(), stderr, nil
	}
	return 0, "", fmt.Errorf("%w: running git %v: %w", errCheckFailed, args, runErr)
}

// gitEnv builds the environment for one git subprocess invocation, an
// explicit minimal list rather than os.Environ() plus additions -- the
// same shape and rationale as internal/gitmergetree's, internal/gitdiff's,
// and internal/diffplan's own gitEnv. GIT_CONFIG_NOSYSTEM plus the
// redirected HOME/XDG_CONFIG_HOME/GIT_CONFIG_GLOBAL mean no system,
// user-global, or ambient GIT_CONFIG_GLOBAL-pointed config is ever read.
//
// extraObjectDir, when non-empty, is exposed as
// GIT_ALTERNATE_OBJECT_DIRECTORIES -- an ADDITIONAL object store, so the
// mirror's own objects (the target tip) stay visible; see the package doc
// comment. It is omitted entirely when empty rather than set to "", which
// git treats as an empty alternates list either way but which would put a
// meaningless variable in the environment of every non-quarantine caller.
func gitEnv(home, extraObjectDir string) []string {
	env := []string{
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
		"GIT_CURL_VERBOSE=0",
		"GIT_TRACE_PACKET=0",
		"GIT_TRACE_PACK_ACCESS=0",
		"GIT_TRACE_SETUP=0",
	}
	if extraObjectDir != "" {
		env = append(env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+extraObjectDir)
	}
	return env
}

// cappedBuffer is an io.Writer retaining only the first max bytes ever
// written, matching internal/gitmergetree's and internal/gitdiff's own
// cappedBuffer.
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
