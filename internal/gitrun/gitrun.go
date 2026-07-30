// Package gitrun owns exactly one thing: how to launch a `git` subprocess
// against a LOCAL, anonymous target (a bare mirror on disk, or nothing at
// all) with an isolated, hardened environment. It does not know what
// subcommand it is running or what a nonzero exit means -- that stays at
// each call site, deliberately (see below).
//
// # Why this package exists (loam-ldx)
//
// Before this package, "run a local git subprocess with hardened,
// isolated config" was carbon-copied across SEVEN call sites, not the
// four loam-ldx's own DESCRIPTION named: internal/gitdiff (the original,
// which internal/diffplan's own package doc comment says it copied
// "verbatim" because gitdiff exported none of it), internal/diffplan,
// internal/handler/git, internal/gitmergetree, and three more this bead's
// own instructions asked to be grepped for and found:
// internal/gitref, internal/gitancestry, and
// internal/ingest/orchestrator (gitreader.go). Six of those seven
// (every one except internal/handler/git) are byte-for-byte identical in
// their isolation: an explicit environment list (never os.Environ() plus
// additions) built from a fresh os.MkdirTemp'd HOME, GIT_CONFIG_NOSYSTEM,
// a redirected XDG_CONFIG_HOME and a GIT_CONFIG_GLOBAL pointed at a path
// that never exists, GIT_TERMINAL_PROMPT/ASKPASS/SSH_ASKPASS disabled,
// GIT_PAGER=cat, and every GIT_TRACE* forced to "0" (GIT_CURL_VERBOSE
// deliberately excluded -- see Env's own doc comment). Seven copies means
// a hardening fix applied to one is silently missed in the other six,
// with nothing to catch the omission -- exactly the shape of bug that bit
// this repo twice already (macOS's system gitconfig setting
// credential.helper=osxkeychain, which keys by protocol+host IGNORING THE
// PORT and so authenticated a test that deliberately sent no
// credentials, inverting a credentials guard).
//
// internal/handler/git was the weakest of the seven: it built its
// environment from PATH, GIT_CONFIG_NOSYSTEM, and GIT_TERMINAL_PROMPT
// alone, with no HOME redirection at all -- meaning a user-level
// ~/.gitconfig on whatever host runs the loam server (unlike the system
// gitconfig, GIT_CONFIG_NOSYSTEM does not block that) could still reach
// every upload-pack/receive-pack invocation this package now serves.
// That gap is closed by routing it through this package's Env, same as
// every other caller -- see internal/handler/git/subprocess.go's own doc
// comment for why that is safe for its stateless-rpc argv shape.
//
// # What is NOT shared, and why
//
// The callers differ in argv (a diff needs --no-ext-diff, merge-tree
// needs --write-tree, upload-pack/receive-pack take the mirror as a
// trailing positional argument rather than --git-dir=) and, more
// importantly, in how they classify a subprocess's raw result:
// internal/gitmergetree deliberately treats exit 1 plus a well-formed
// object ID as a real conflict answer, exit 1 with empty stdout as a
// check failure, and a context already Done by the time cmd.Run returns
// as a cancellation that OVERRIDES whatever exit status came back (see
// its own classifyRunErr) -- flattening any of that into a shared
// "succeeded or not" helper would silently break it. This package
// therefore stops at building and launching the *exec.Cmd (NewCommand)
// and reports only what every caller already agrees on structurally: the
// isolated environment (Env) and, for the --git-dir family, the
// isolation flags that belong ahead of --git-dir (GitDirArgs). Running
// the command, and turning cmd.Run's error into a domain answer, is left
// to each call site, unchanged from before this package existed.
//
// # internal/forge's gitAuthEnv: deliberately NOT folded in
//
// internal/forge/forgejo_git.go's gitAuthEnv is the most thoroughly
// hardened git-environment builder in this tree (hardened this session by
// loam-7gc: GIT_CONFIG_NOSYSTEM, a redirected HOME/XDG_CONFIG_HOME, a
// nonexistent GIT_CONFIG_GLOBAL, GIT_CONFIG_PARAMETERS cleared, an
// explicit GIT_CONFIG_COUNT, and -c credential.helper= in argv), and
// loam-ldx's own instructions asked for a deliberate call on whether it
// joins this extraction. It does not, for two structural reasons, not
// just "it handles credentials":
//
//  1. Different base model. gitAuthEnv starts from a FILTERED
//     os.Environ() (dropGitCurlVerbose, then appended overrides), because
//     it must inject the bound forge credential via GIT_CONFIG_COUNT/
//     GIT_CONFIG_KEY_0/GIT_CONFIG_VALUE_0 and still needs whatever else a
//     real network git invocation legitimately wants from the host
//     (proxy variables, etc. -- it talks to a real upstream over HTTP(S),
//     not a local bare mirror). Env below is the opposite: an explicit
//     list built from NOTHING ambient, which is strictly the more
//     isolated of the two shapes for a purely local, anonymous
//     invocation -- there is nothing for gitAuthEnv's
//     GIT_CONFIG_PARAMETERS-clearing or explicit GIT_CONFIG_COUNT to
//     protect against here, since cmd.Env being fully explicit already
//     means no ambient GIT_CONFIG_* of any kind is visible to the child.
//     Adding those two keys anyway was considered and rejected: it would
//     be pure belt-and-suspenders with no attack it actually closes under
//     this package's model, AND it is not free --
//     internal/gitmergetree's own TestGitEnv_IsolatesFromAmbientConfig
//     pins gitEnv's output as an explicit whitelist of variable NAMES;
//     adding two more would fail that test, which is exactly the "you
//     changed behaviour, stop and report it" signal loam-ldx's own
//     instructions warn about, not a change this refactor should absorb.
//  2. Different argv shape and purpose. Every gitrun caller passes
//     --git-dir=<local bare mirror> (or, for internal/handler/git, the
//     mirror as a positional argument) and touches no credential.
//     gitAuthEnv's callers pass a bare upstream URL with no --git-dir at
//     all and exist specifically to inject a bound token. Folding it in
//     would either weaken this package's env (adopting the os.Environ()
//     base) or complicate it with a credential-injection path none of
//     its seven current callers need.
//
// Net effect: internal/forge stays exactly as loam-7gc left it. Nothing
// here should be read as "gitAuthEnv is less correct" -- it is solving a
// different problem under a different, arguably-necessary base model.
package gitrun

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// subprocessWaitDelay bounds how long a canceled invocation's git process
// gets to exit on its own, and how long Wait then waits on its pipes,
// before Cmd forces them closed -- the same value and purpose every one
// of this package's absorbed copies independently arrived at.
const subprocessWaitDelay = 5 * time.Second

// NewIsolatedHome creates a fresh, per-invocation temp directory to use as
// one git subprocess's HOME (see Env), and returns a cleanup func that
// removes it -- callers must defer cleanup() once the subprocess this
// home was built for has exited. A new directory per invocation, never a
// shared one, is what makes Env's GIT_CONFIG_GLOBAL path (inside home)
// reliably not exist: reusing a directory across calls would let one
// invocation's accidental write there leak into every later one.
func NewIsolatedHome() (string, func(), error) {
	home, err := os.MkdirTemp("", "loam-gitrun-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating isolated git environment: %w", err)
	}
	return home, func() { _ = os.RemoveAll(home) }, nil
}

// Env builds the complete environment for one local, anonymous git
// subprocess invocation: an explicit list, never os.Environ() plus
// additions, so no variable this function does not name is ever visible
// to the child -- see the package doc comment for why that already
// subsumes the ambient-config hazards a filtered-os.Environ() model (like
// internal/forge's gitAuthEnv) has to defend against by other means.
//
// GIT_CONFIG_NOSYSTEM plus the redirected HOME/XDG_CONFIG_HOME/
// GIT_CONFIG_GLOBAL mean no system, user-global, or ambient
// GIT_CONFIG_GLOBAL-pointed config is ever read -- the exact class of
// hazard that inverted a credentials guard once already in this tree
// (macOS's system gitconfig setting credential.helper=osxkeychain, keyed
// by protocol+host and ignoring the port). GIT_PAGER=cat plus a caller's
// own --no-pager flag (see GitDirArgs) doubly guard against core.pager
// blocking on a tty this subprocess never has. GIT_TERMINAL_PROMPT=0 and
// the two ASKPASS variables mean a misconfigured or unreachable remote
// can never block a request-handling goroutine waiting on a prompt. The
// GIT_TRACE* overrides keep every subprocess's own diagnostic output off
// by default. GIT_CURL_VERBOSE is deliberately NOT one of them: git only
// presence-checks that variable rather than parsing it as a boolean, so
// setting it to "0" would turn curl tracing ON; simply never naming it in
// this from-scratch list is what actually keeps it off.
func Env(home string) []string {
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

// GitDirArgs prepends this package's own argv-level isolation --
// --no-pager (closes the core.pager risk directly; see Env) and -c
// credential.helper= (belt-and-suspenders alongside Env's isolation,
// which already guarantees no ambient config ever configures one) --
// ahead of --git-dir=mirrorDir and the caller's own subArgs.
//
// --git-dir, never -C: loam-ofg.19's review established that `git -C`
// performs upward repository discovery when the given directory is not
// itself a valid repository, silently falling back to operate on
// whatever enclosing repository it finds instead -- every caller of this
// needs a bad or not-yet-cloned mirror path to fail loudly, not quietly
// run against the wrong repository.
func GitDirArgs(mirrorDir string, subArgs ...string) []string {
	return append([]string{"--no-pager", "-c", "credential.helper=", "--git-dir=" + mirrorDir}, subArgs...)
}

// NewCommand builds an *exec.Cmd for `git <args...>`, tied to ctx (so a
// canceled request kills the subprocess via exec.CommandContext's default
// Cancel) with subprocessWaitDelay bounding how long a killed process may
// keep this call's pipes open, env as its complete environment (see Env),
// and stdin/stdout/stderr wired directly -- any of which may be nil,
// matching exec.Cmd's own meaning for a nil stream (stdin: /dev/null;
// stdout/stderr: discarded).
//
// It does not run the command. Running it, and turning a nonzero exit or
// a failure to run at all into a domain answer, is every caller's own job
// -- see the package doc comment for why that classification is
// deliberately not this package's concern.
func NewCommand(ctx context.Context, env []string, stdin io.Reader, stdout, stderr io.Writer, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = subprocessWaitDelay
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd
}

// CappedBuffer is an io.Writer that retains only the first max bytes ever
// written to it while still reporting every byte written (Write always
// returns len(p), nil) -- so a subprocess writing to one never blocks on
// a full pipe waiting for a reader that has stopped consuming, the same
// hazard a capped io.Reader would introduce. It was duplicated
// byte-for-byte across every absorbed copy of this package (see the
// package doc comment) and is folded in here for the same reason Env and
// NewCommand are: it carries no per-caller classification, only
// mechanical buffering behaviour every caller wants identically.
type CappedBuffer struct {
	buf   bytes.Buffer
	max   int
	total int
}

// NewCappedBuffer returns a CappedBuffer retaining at most max bytes.
func NewCappedBuffer(max int) *CappedBuffer {
	return &CappedBuffer{max: max}
}

// Write implements io.Writer.
func (c *CappedBuffer) Write(p []byte) (int, error) {
	c.total += len(p)
	if room := c.max - c.buf.Len(); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		c.buf.Write(p[:room])
	}
	return len(p), nil
}

// Bytes returns the retained prefix of everything written, up to max
// bytes.
func (c *CappedBuffer) Bytes() []byte {
	return c.buf.Bytes()
}

// String is Bytes as a string.
func (c *CappedBuffer) String() string {
	return c.buf.String()
}

// Overflowed reports whether this buffer ever received more than max
// bytes total, regardless of how much it retained.
func (c *CappedBuffer) Overflowed() bool {
	return c.total > c.max
}
