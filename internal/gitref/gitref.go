// Package gitref writes Loam's own refs into a repo's bare mirror. Today
// that is exactly one thing: creating (and, on a compensating rollback,
// removing) a work branch's ref at `work start` time.
//
// docs/git-spec.md -> Ref Policy makes this the server's job and nobody
// else's: work-branch refs are "created server-side by `work start` only,
// never by push", and the pre-receive hook enforces the other half by
// rejecting a push that tries to create one. Before loam-5iu nothing in
// this tree honoured that -- CreateWorkBranch inserted the work_branches
// row and stopped -- so GetWorkBranchDiff answered ErrRefMissing for
// essentially every work branch and `loam clone` of a freshly started
// branch had no ref to fetch.
//
// # Why a separate package
//
// Same reason internal/gitancestry, internal/gitmergetree and
// internal/gitdiff are each their own package: subprocess handling, argv
// construction and environment isolation stay in one small place with a
// single seam above it, and the orchestration (internal/handler/workbranch)
// stays testable against that seam rather than against a git binary.
//
// # Isolation
//
// Identical in shape and rationale to internal/gitdiff's and
// internal/gitancestry's: `--git-dir=<mirrorDir>` (never `-C`, which
// performs upward repository discovery and would silently operate on an
// ENCLOSING repository when the mirror path is wrong -- catastrophic for
// an operation that WRITES refs), GIT_CONFIG_NOSYSTEM plus a redirected
// HOME/XDG_CONFIG_HOME/GIT_CONFIG_GLOBAL so no host or user gitconfig is
// read, an explicit minimal environment rather than os.Environ() plus
// additions, and a WaitDelay so a canceled request's context kills the
// subprocess.
package gitref

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/refnames"
)

// maxStderrBytes caps retained stderr, matching internal/gitancestry's and
// internal/gitmergetree's own cap and for the same reason: every stderr
// this package classifies on is a single short line, retained only so a
// failure message can quote what git actually said.
const maxStderrBytes = 8 << 10

// subprocessWaitDelay bounds how long a canceled invocation's git process
// may keep this call's pipes open after the context kills it, matching
// internal/gitdiff, internal/gitancestry and internal/gitmergetree.
const subprocessWaitDelay = 5 * time.Second

// ErrMirrorMissing indicates the repo's bare mirror does not exist on disk,
// or the path mirrorpath.Dir derived is not a valid git repository -- an
// operational fault (the repo is enrolled but its mirror is absent or
// corrupt), not a caller mistake. Deliberately the same distinction
// internal/gitdiff draws with its own identically-named sentinel.
var ErrMirrorMissing = errors.New("gitref: bare mirror missing or invalid on disk")

// ErrTargetMissing indicates the branch a work branch is being cut FROM
// does not exist in the mirror. Reachable without anything being broken:
// a repo enrolled moments ago whose first mirror sync has not landed yet
// has a repo_target_branches row but no ref, and CreateWorkBranch
// validates `from` against that table, not against the mirror.
var ErrTargetMissing = errors.New("gitref: target branch not found in mirror")

// ErrRefExists indicates the work-branch ref already exists in the mirror.
// The create is deliberately guarded rather than an unconditional
// update-ref: a work branch's ref must be created once, at its base
// commit, and never moved except by an agent's own push (docs/git-spec.md:
// "work-branch refs advance only by agent pushes"). An unguarded write
// would silently rewind a live branch's history on a name collision --
// randomWorkBranchName's 3 random bytes make that unlikely, not impossible,
// and "unlikely" is not a safety property.
var ErrRefExists = errors.New("gitref: work-branch ref already exists")

// Creator creates work-branch refs in bare mirrors under dataDir
// (LOAM_DATA_DIR). It holds no per-repo state: the repo name is a
// parameter, so one Creator serves every enrolled repo.
type Creator struct {
	dataDir string
}

// New builds a Creator rooted at dataDir (LOAM_DATA_DIR), deriving each
// repo's mirror path through internal/mirrorpath exactly as
// internal/gitdiff and internal/mirrorsync derive theirs.
func New(dataDir string) *Creator {
	return &Creator{dataDir: dataDir}
}

// CreateWorkBranchRef creates refnames.WorkBranch(name) in repoName's bare
// mirror, pointing at whatever refnames.TargetBranch(from) currently
// resolves to. It fails with ErrRefExists rather than moving an existing
// ref, with ErrTargetMissing when from names no ref, and with
// ErrMirrorMissing when the mirror is absent or invalid.
//
// The commit is resolved to a SHA first and the ref is then created at
// that SHA, rather than pointing the ref straight at the target ref in one
// update-ref. That is not ceremony: it separates "the target does not
// exist" (ErrTargetMissing, a precondition a caller can report precisely)
// from "the create was refused" (ErrRefExists), which a single failing
// invocation could not distinguish, and it pins the base commit at this
// instant rather than at whatever the target happens to be when git gets
// around to dereferencing it.
func (c *Creator) CreateWorkBranchRef(ctx context.Context, repoName, name, from string) error {
	mirrorDir := mirrorpath.Dir(c.dataDir, repoName)
	baseSHA, err := c.resolve(ctx, mirrorDir, refnames.TargetBranch(from))
	if err != nil {
		return fmt.Errorf("resolving target branch %s for work branch %s in %s: %w", from, name, repoName, err)
	}
	ref := refnames.WorkBranch(name)
	// `create` is git-update-ref(1)'s own guarded form: "Create <ref> with
	// <newvalue> after verifying it does not exist." Using --stdin rather
	// than the positional `update-ref <ref> <new> <old>` form avoids
	// having to spell an all-zero old-value whose LENGTH depends on the
	// repository's hash algorithm (40 hex for SHA-1, 64 for SHA-256).
	out, err := c.run(ctx, mirrorDir, strings.NewReader("create "+ref+" "+baseSHA+"\n"), "update-ref", "--stdin")
	if err != nil {
		return fmt.Errorf("creating %s in %s: %w", ref, repoName, err)
	}
	if out.exitCode != 0 {
		return fmt.Errorf("creating %s at %s in %s: %w", ref, baseSHA, repoName, classifyUpdateRefStderr(out.stderr))
	}
	return nil
}

// DeleteWorkBranchRef removes refnames.WorkBranch(name) from repoName's
// mirror. Its only production caller is CreateWorkBranch's compensating
// rollback: the ref is created BEFORE the work_branches row (so a row can
// never exist without its ref, which is the invariant docs/git-spec.md's
// Ref Policy states and every other component -- diff, clone, mergeability,
// refpolicy -- relies on), and if the insert then fails this puts the
// mirror back.
//
// A ref that is already absent is NOT an error: the rollback path must be
// safe to run against a create that never landed.
func (c *Creator) DeleteWorkBranchRef(ctx context.Context, repoName, name string) error {
	mirrorDir := mirrorpath.Dir(c.dataDir, repoName)
	ref := refnames.WorkBranch(name)
	out, err := c.run(ctx, mirrorDir, nil, "update-ref", "-d", ref)
	if err != nil {
		return fmt.Errorf("deleting %s in %s: %w", ref, repoName, err)
	}
	if out.exitCode != 0 {
		return fmt.Errorf("deleting %s in %s: %s", ref, repoName, summarize(out.stderr))
	}
	return nil
}

// resolve returns ref's current commit SHA, classifying `git rev-parse
// --verify --quiet`'s three distinguishable outcomes exactly as
// internal/gitdiff's verifyRef does: exit 0 (resolved), exit 1 with no
// stderr under --quiet (no such ref), exit 128 with "not a git repository"
// (bad --git-dir).
func (c *Creator) resolve(ctx context.Context, mirrorDir, ref string) (string, error) {
	out, err := c.run(ctx, mirrorDir, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return "", err
	}
	switch out.exitCode {
	case 0:
		return strings.TrimSpace(out.stdout), nil
	case 1:
		return "", ErrTargetMissing
	default:
		if isMirrorMissingStderr(out.stderr) {
			return "", fmt.Errorf("%s: %w", mirrorDir, ErrMirrorMissing)
		}
		return "", fmt.Errorf("git rev-parse %s exited %d: %s", ref, out.exitCode, summarize(out.stderr))
	}
}

// classifyUpdateRefStderr maps a failed `update-ref --stdin create` to this
// package's sentinels. git's own wording for a refused create is "fatal:
// cannot lock ref '<ref>': reference already exists" (measured against real
// git 2.50.1), and a bad --git-dir is the same "not a git repository"
// complaint rev-parse makes.
func classifyUpdateRefStderr(stderr string) error {
	switch {
	case strings.Contains(stderr, "already exists"):
		return ErrRefExists
	case isMirrorMissingStderr(stderr):
		return ErrMirrorMissing
	default:
		return errors.New(summarize(stderr))
	}
}

// isMirrorMissingStderr reports whether stderr is git's "not a git
// repository" complaint about a bad --git-dir, matched case-insensitively
// for the reason internal/gitdiff's identically-named helper documents:
// different git subcommands capitalize it differently.
func isMirrorMissingStderr(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "not a git repository")
}

// summarize renders git's stderr for a failure message, or a placeholder
// when git said nothing at all.
func summarize(stderr string) string {
	if s := strings.TrimSpace(stderr); s != "" {
		return s
	}
	return "(no output)"
}

// gitOutput is one subprocess invocation's classified result.
type gitOutput struct {
	stdout   string
	stderr   string
	exitCode int
}

// run executes one git subcommand against mirrorDir (via --git-dir, never
// -C -- see the package doc comment), isolated from the host and user
// gitconfig, optionally feeding stdin. A nonzero git exit is not itself a
// Go error -- gitOutput.exitCode reports it for the caller to classify
// against git's own stderr -- so only a failure to run git at all comes
// back as err.
func (c *Creator) run(ctx context.Context, mirrorDir string, stdin *strings.Reader, args ...string) (gitOutput, error) {
	home, err := os.MkdirTemp("", "loam-gitref-*")
	if err != nil {
		return gitOutput{}, fmt.Errorf("creating isolated git environment: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	fullArgs := append([]string{"--no-pager", "-c", "credential.helper=", "--git-dir=" + mirrorDir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.WaitDelay = subprocessWaitDelay
	cmd.Env = gitEnv(home)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var outBuf bytes.Buffer
	errBuf := &cappedBuffer{max: maxStderrBytes}
	cmd.Stdout = &outBuf
	cmd.Stderr = errBuf
	runErr := cmd.Run()
	if runErr == nil {
		return gitOutput{stdout: outBuf.String(), stderr: errBuf.buf.String()}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return gitOutput{stdout: outBuf.String(), stderr: errBuf.buf.String(), exitCode: exitErr.ExitCode()}, nil
	}
	return gitOutput{}, fmt.Errorf("running git %v: %w", args, runErr)
}

// gitEnv builds the environment for one git subprocess invocation: an
// explicit minimal list rather than os.Environ() plus additions, the same
// shape and rationale as internal/gitdiff's, internal/gitancestry's and
// internal/gitmergetree's.
//
// GIT_AUTHOR_*/GIT_COMMITTER_* are deliberately absent: this package only
// ever moves refs, never creates an object, so git never needs an identity
// and would never prompt for one.
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
		"GIT_CURL_VERBOSE=0",
		"GIT_TRACE_PACKET=0",
		"GIT_TRACE_PACK_ACCESS=0",
		"GIT_TRACE_SETUP=0",
	}
}

// cappedBuffer is an io.Writer retaining only the first max bytes ever
// written, matching internal/gitancestry's and internal/gitdiff's own.
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
