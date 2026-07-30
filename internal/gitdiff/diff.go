// Package gitdiff implements internal/handler/workbranch.DiffComputer
// (loam-fwk): it shells out to real git against a repo's bare mirror on
// disk and returns `git diff <target>...<name>`'s unified-diff output --
// the three-dot form, i.e. the diff from the MERGE BASE of target and
// name, which is what the work branch itself changed, not everything that
// has happened on target since the branch was cut (docs/cli-spec.md ->
// "diff": "the unified diff of the work branch against its target
// branch"). Before this package, no code anywhere in this tree shelled out
// to `git diff` at all -- confirmed by grep against internal/ -- despite
// docs/git-spec.md's "Enforcement Mechanics" section asserting in passing
// that "the server already shells out to git for sync, diffs, and ingest";
// that sentence does not describe this tree's actual history and should
// not be read as evidence this plumbing predates loam-fwk.
//
// Isolation (loam-ldx: now internal/gitrun's Env/NewCommand, not
// hand-rolled here -- this package was gitrun's original source, before
// six further identical copies made the duplication worth extracting) and
// why: GIT_CONFIG_NOSYSTEM plus a redirected HOME/XDG_CONFIG_HOME/
// GIT_CONFIG_GLOBAL so no host or user gitconfig is ever read,
// credential.helper explicitly cleared, GIT_TRACE* forced off via "=0"
// and GIT_CURL_VERBOSE kept off the explicit allowlist (git only
// presence-checks that one, so "=0" would enable it -- see gitrun.Env),
// and exec.CommandContext with a WaitDelay so a canceled request's
// diff against a bare mirror needs no credential (unlike gittransport's
// own upstream operations), but the config-isolation property is still
// load-bearing here for a reason gittransport never has to worry about:
// `git diff` is the one git operation in this tree that can read
// diff.external (an arbitrary external diff driver) or core.pager from an
// ambient gitconfig, either of which would corrupt this handler's returned
// diff text or hang the request waiting on a pager that never gets a tty
// to write to. --no-pager closes the pager risk directly; the
// diff.external risk is closed by runDiff's own `--no-ext-diff` flag, NOT
// by `-c diff.external=` -- verified empirically that the latter is
// actively harmful: git treats an explicitly-configured EMPTY
// diff.external (via `-c` or the GIT_EXTERNAL_DIFF env var) as "an
// external diff IS configured, its command is the empty string," and
// tries to execute it, failing every diff with "fatal: external diff
// died." Only omitting diff.external entirely (this package's config
// isolation already guarantees no ambient gitconfig sets it) or passing
// `--no-ext-diff` (which unconditionally overrides any diff.external,
// however it got set) actually disables it; this package does both:
// isolates against an ambient source AND passes --no-ext-diff as a second,
// independent guard against a mirror's own repository-level config ever
// setting it. Unlike gittransport's own gitEnv, gitrun.Env's environment
// is NOT built by appending to os.Environ() -- it is an explicit, minimal
// list (PATH plus the isolation variables) -- since this operation needs
// no ambient host environment variable to function and every inherited
// GIT_* variable is a variable this package does not have to reason
// about.
//
// `--git-dir=<mirrorDir>` addresses the mirror explicitly, never `-C
// <mirrorDir>`: loam-ofg.19's review established that `git -C` performs
// upward repository discovery when the given directory is not itself a
// valid repository, silently falling back to operate on whatever
// enclosing repository it finds instead (internal/mirrorreconcile/
// reconcile.go's own doc comments carry the same citation) -- a
// mirror-path typo or a not-yet-cloned mirror must fail loudly, not
// quietly diff the wrong repository. verifyRef below turns that failure
// mode into gitdiff.ErrMirrorMissing by classifying `not a git
// repository` in git's own stderr, rather than letting an ambiguous
// argument-parsing error (verified empirically: a bad --git-dir plus a
// three-dot revspec makes git misparse the whole invocation as `git diff
// --no-index <path> <path>` and print its own usage text, exit 129) reach
// the caller unclassified.
package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bobcob7/loam/internal/gitrun"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/refnames"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// maxDiffBytes bounds how much of `git diff`'s stdout Computer retains in
// memory per call. GetWorkBranchDiffResponse (proto/loam/v1/workbranch.proto)
// carries the diff as a single `string diff = 1` field with no sibling
// `truncated` bool -- unlike ListWorkBranchesResponse and the graph RPCs'
// own limit/truncated contract -- so there is no envelope field to signal
// a cap here. Given that shape, an unbounded diff piped straight into a
// Connect response is a memory risk with no upstream field to guard it, so
// this package caps the byte count itself and, when the cap binds, appends
// a visible trailing marker to the returned text (diffTruncatedMarker)
// rather than truncating silently or growing the response without bound.
// 4 MiB is not pinned by any spec or existing constant in this tree (grepped:
// none exists) -- it is this package's own choice, sized to comfortably
// hold a realistic single-branch diff while keeping a concurrent request's
// worst-case memory bounded, the same reasoning gRPC's own common 4 MiB
// default receive-message-size embodies. A package-level var, not a const,
// solely so this package's own whitebox tests can shrink it temporarily
// rather than materializing a genuine multi-megabyte fixture to exercise
// truncation.
var maxDiffBytes = 4 << 20

// maxStderrBytes bounds captured stderr the same way maxDiffBytes bounds
// stdout, sized much smaller since git's own error output is always a few
// lines, never proportional to repository size.
const maxStderrBytes = 64 << 10

// diffTruncatedMarker is appended to a capped diff's text when maxDiffBytes
// bound, so truncation is visible in the returned string itself -- the only
// channel available given GetWorkBranchDiffResponse carries no separate
// truncated field (see maxDiffBytes' doc comment).
const diffTruncatedMarkerFormat = "\n... diff truncated at %d bytes; git produced more -- fetch %s locally and diff against %s directly for the full change ...\n"

// ErrMirrorMissing indicates the repo's bare mirror does not exist on disk,
// or the path mirrorpath.Dir derived is not a valid git repository at all
// -- an operational fault (the repo is enrolled in the store but its
// mirror is absent or corrupt), not a caller mistake.
var ErrMirrorMissing = errors.New("gitdiff: bare mirror missing or invalid on disk")

// ErrRefMissing indicates a ref this diff needs -- the work branch's
// target, or its own name -- does not exist in the mirror. Both refs exist
// for any genuinely created work branch (docs/git-spec.md -> "Ref Policy":
// work-branch refs are created server-side by `work start`, which since
// loam-5iu it really does; target branches are mirrored refs kept current
// by upstream sync), so this signals the mirror has fallen out of sync with
// the work-branch registry, not that the caller named something invalid --
// resolveWorkBranch (workbranch.go) already rejects an unknown work branch
// before Computer.Diff is ever called.
var ErrRefMissing = errors.New("gitdiff: ref not found in mirror")

// ErrNoMergeBase indicates target and name share no common ancestor --
// `git diff target...name` requires one to compute the three-dot range,
// and fails outright without it (verified empirically: exit 128, stderr
// "fatal: <target>...<name>: no merge base"). This is a real, reachable
// condition (unrelated histories), not a defect in this package -- the
// bead's own instructions call it out as a distinct case to surface, not
// paper over.
var ErrNoMergeBase = errors.New("gitdiff: no merge base between target and work branch")

// Computer implements internal/handler/workbranch.DiffComputer by shelling
// out to real git against the bare mirror at
// mirrorpath.Dir(dataDir, repoName).
type Computer struct {
	dataDir string
	repos   RepoStore
}

// New builds a Computer rooted at dataDir (LOAM_DATA_DIR), resolving a work
// branch's repo name via repos before deriving its mirror path.
func New(dataDir string, repos RepoStore) *Computer {
	return &Computer{dataDir: dataDir, repos: repos}
}

// Diff implements workbranch.DiffComputer: it resolves wb's repo to a
// mirror path, verifies both refs the three-dot range needs exist, then
// runs `git --git-dir=<mirrorDir> diff <target>...<name>` and returns its
// output, truncated (with a visible marker, see maxDiffBytes) if it
// exceeds maxDiffBytes. An empty diff (target and name have identical
// trees) is a valid, non-error result: the empty string.
func (c *Computer) Diff(ctx context.Context, wb workbranchstore.WorkBranch) (string, error) {
	repo, err := c.repos.GetRepoByID(ctx, wb.RepoID)
	if err != nil {
		return "", fmt.Errorf("resolving repo for work branch %s: %w", wb.Name, err)
	}
	mirrorDir := mirrorpath.Dir(c.dataDir, repo.Name)
	// The two refs live in DIFFERENT namespaces and neither may be
	// spelled by hand here: a target branch is a mirrored ref and stays
	// where upstream put it (refs/heads/<target>), while a work-branch ref
	// lives under Loam's own reserved, server-owned namespace
	// (refs/heads/loam-reserved/<name>) so a mid-fetch prune can never
	// delete it -- see internal/refnames.
	targetRef, workBranchRef := refnames.TargetBranch(wb.Target), refnames.WorkBranch(wb.Name)
	if err := c.verifyRef(ctx, mirrorDir, wb.Target, targetRef); err != nil {
		return "", fmt.Errorf("verifying target branch %s: %w", wb.Target, err)
	}
	if err := c.verifyRef(ctx, mirrorDir, wb.Name, workBranchRef); err != nil {
		return "", fmt.Errorf("verifying work branch %s: %w", wb.Name, err)
	}
	diff, err := c.runDiff(ctx, mirrorDir, targetRef, workBranchRef, wb.Name)
	if err != nil {
		return "", fmt.Errorf("diffing %s...%s in %s: %w", wb.Target, wb.Name, mirrorDir, err)
	}
	return diff, nil
}

// verifyRef confirms the full ref path ref exists in the mirror at
// mirrorDir, via `git rev-parse --verify --quiet`, classifying the three
// distinguishable outcomes verified empirically against real git: exit 0
// (ref exists), exit 1 with no stderr under --quiet (ref does not exist --
// ErrRefMissing), or exit 128 with "fatal: not a git repository" in stderr
// (bad --git-dir -- ErrMirrorMissing, via isMirrorMissingStderr). Any other
// nonzero exit is reported as-is rather than forced into one of those two
// buckets.
//
// name is the bare branch name the same ref is known by everywhere a
// human or an agent reads (a CLI argument, a work_branches row); it
// appears in the returned errors, while ref is what git is actually asked
// about. Keeping both is what lets ErrRefMissing say "wb-9c2f1a" rather
// than making a caller decode a reserved-namespace ref path.
func (c *Computer) verifyRef(ctx context.Context, mirrorDir, name, ref string) error {
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

// isMirrorMissingStderr reports whether stderr is git's own "not a git
// repository" complaint about a bad --git-dir, checked case-insensitively
// because the two git subcommands this package runs word it differently
// on real git (verified empirically): `rev-parse --verify` says "fatal:
// not a git repository: '<path>'", while `diff` against the same bad path
// instead misparses the whole invocation as `diff --no-index <path>
// <path>` (see package doc comment) and says "warning: Not a git
// repository." -- capital N. A case-sensitive check would silently miss
// the second form.
func isMirrorMissingStderr(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "not a git repository")
}

// runDiff runs `git diff <targetRef>...<workBranchRef>` (three-dot: the
// diff from the two refs' merge base, per this package's own doc comment)
// against mirrorDir and returns its stdout, capped at maxDiffBytes with a
// visible trailing marker if git produced more. Both arguments are FULL
// ref paths, never bare names -- the work branch's is under
// refnames.ReservedNamespace, which no bare-name revspec would resolve.
// name is the work branch's bare name, used only in the truncation marker
// an agent reads.
//
// verifyRef has already confirmed both refs exist by the time this runs,
// so a nonzero exit here means either no merge base (ErrNoMergeBase) or a
// mirror that went missing between verifyRef and this call
// (ErrMirrorMissing) -- both classified from git's own stderr, the same
// way verifyRef classifies its own exit codes.
func (c *Computer) runDiff(ctx context.Context, mirrorDir, targetRef, workBranchRef, name string) (string, error) {
	out, err := c.run(ctx, mirrorDir, "diff", "--no-ext-diff", targetRef+"..."+workBranchRef)
	if err != nil {
		return "", err
	}
	if out.exitCode != 0 {
		switch {
		case strings.Contains(out.stderr, "no merge base"):
			return "", fmt.Errorf("%s...%s: %w", targetRef, workBranchRef, ErrNoMergeBase)
		case isMirrorMissingStderr(out.stderr):
			return "", fmt.Errorf("%s: %w", mirrorDir, ErrMirrorMissing)
		default:
			return "", fmt.Errorf("git diff exited %d: %s", out.exitCode, strings.TrimSpace(out.stderr))
		}
	}
	diff := string(out.stdout)
	if out.truncated {
		diff += fmt.Sprintf(diffTruncatedMarkerFormat, maxDiffBytes, name, name)
	}
	return diff, nil
}

// gitOutput is one subprocess invocation's classified result: stdout
// (capped at maxDiffBytes, with truncated reporting whether git actually
// produced more than that), the process's exit code, and stderr (capped at
// maxStderrBytes).
type gitOutput struct {
	stdout    []byte
	truncated bool
	exitCode  int
	stderr    string
}

// run executes one git subcommand against mirrorDir (via --git-dir, never
// -C -- see package doc comment), isolated from the host and user
// gitconfig, with stdout/stderr captured into capped in-memory buffers so
// neither an enormous diff nor a runaway error stream can grow this
// process's memory without bound. A nonzero git exit is not itself a Go
// error -- gitOutput.exitCode reports it for the caller (verifyRef,
// runDiff) to classify against git's own stderr text; only a failure to
// even run git (binary missing, context already canceled before start,
// ...) is returned as err.
func (c *Computer) run(ctx context.Context, mirrorDir string, args ...string) (gitOutput, error) {
	home, cleanup, err := gitrun.NewIsolatedHome()
	if err != nil {
		return gitOutput{}, err
	}
	defer cleanup()
	outBuf := gitrun.NewCappedBuffer(maxDiffBytes)
	errBuf := gitrun.NewCappedBuffer(maxStderrBytes)
	cmd := gitrun.NewCommand(ctx, gitrun.Env(home), nil, outBuf, errBuf, gitrun.GitDirArgs(mirrorDir, args...)...)
	runErr := cmd.Run()
	if runErr == nil {
		return gitOutput{stdout: outBuf.Bytes(), truncated: outBuf.Overflowed(), exitCode: 0, stderr: errBuf.String()}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return gitOutput{stdout: outBuf.Bytes(), truncated: outBuf.Overflowed(), exitCode: exitErr.ExitCode(), stderr: errBuf.String()}, nil
	}
	return gitOutput{}, fmt.Errorf("running git %v: %w", args, runErr)
}
