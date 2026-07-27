// Package mirrorreconcile idempotently reconciles a bare git mirror's
// push-safety configuration and pre-receive hook stub, per
// docs/git-spec.md "Enforcement Mechanics" and docs/server-spec.md
// "Startup" step 3: "every mirror carries receive.denyNonFastForwards and
// receive.denyDeletes, so force pushes and ref deletions are rejected by
// git itself, with git's own messages" (docs/git-spec.md "Ref Policy
// (push)"), plus "the server writes the hook and the
// receive.denyNonFastForwards / receive.denyDeletes config idempotently at
// enrollment and on startup, so upgrades never chase stale mirror state"
// (docs/git-spec.md "Enforcement Mechanics").
//
// ReconcileMirror is the single seam both call sites share: loam-ofg.12's
// EnrollRepo (not yet implemented anywhere in this tree) is meant to call
// it right after cloning a fresh mirror, and cmd/server/main.go's Startup
// step 3 calls it in a loop over every enrolled repo. Neither caller needs
// to diff existing state first; see ReconcileMirror's own doc comment for
// why every call is unconditionally safe to repeat.
//
// The pre-receive hook installed at every mirror is a byte-for-byte copy
// of the compiled cmd/loamhook binary (loam-ofg.18): ReconcileMirror takes
// the path to that binary as a parameter and copies it into place,
// atomically, exactly as it always has for the string-literal placeholder
// this package originally shipped (see writeHook's own doc comment for why
// the copy must be atomic). The hook's actual policy-enforcement BEHAVIOR
// -- dialing the policy socket, evaluating docs/git-spec.md's ref-policy
// rules -- lives entirely in cmd/loamhook and internal/hooksocket /
// internal/refpolicy, never in this package: this package only ever
// concerns itself with getting the right bytes onto disk, at the right
// path and mode, idempotently.
package mirrorreconcile

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// hookRelPath is the pre-receive hook's path relative to a bare mirror's
// git directory (a bare mirror's git directory IS the repo root, unlike a
// non-bare repo's .git subdirectory) -- loam-ofg.19's own DESIGN note: "the
// pre-receive hook file ... at /hooks/pre-receive (0755)". docs/git-spec.md
// itself never states the hook's path or mode; only this bead's DESIGN
// note does, so that is what this comment cites, not the spec.
const hookRelPath = "hooks/pre-receive"

// hookMode is the mode git requires a hook file to run under: executable.
// git only inspects the executable bit itself (there is no git config knob
// for hook permissions); 0o755 is simply the conventional
// owner/group/other read+execute, owner-write mode for a script nobody but
// this process writes. loam-ofg.19's own DESIGN note pins the same
// "(0755)" figure -- docs/git-spec.md never mentions a mode.
const hookMode = 0o755

// ReconcileMirror idempotently writes the pre-receive hook (a byte-for-
// byte copy of the compiled binary at hookBinaryPath -- see writeHook's
// own doc comment for why it must be a real, separate binary rather than
// a shell script) and the receive.denyNonFastForwards / receive.denyDeletes
// git config into the bare mirror at repoPath, per docs/git-spec.md
// "Enforcement Mechanics". Every call unconditionally (over)writes the
// hook file's bytes and mode, then sets both config keys -- there is no
// read-modify-write diff step, so two calls in a row produce byte-
// identical, config-identical results: repeating this is always safe, at
// enrollment (loam-ofg.12) and again on every server startup
// (docs/server-spec.md Startup step 3). Nothing here touches the mirror's
// refs, objects, or any other config key, so a reconciliation pass never
// disturbs work already stored in the mirror.
//
// A repoPath that does not exist on disk is NOT an error: it is either not
// yet cloned, or lost -- both derived state docs/server-spec.md's Startup
// step 3 says the next Mirror Sync cycle re-clones, with Postgres as the
// record of enrollment, not the mirror's presence on disk. ReconcileMirror
// returns nil rather than erroring so one missing mirror never aborts
// reconciliation of every other enrolled repo. This check runs BEFORE
// hookBinaryPath is ever read, so a missing mirror never depends on
// hookBinaryPath existing either.
//
// repoPath must resolve, via git's own --git-dir (never -C/-cd, which
// walks UP to an enclosing repository when the given directory is not
// itself a valid git directory), to an actual bare repository: a path that
// exists as a directory but is not a git directory at all (a half-finished
// clone, a restored volume missing its object store) is a real error, not
// a silent success, and a path that is a valid but non-bare repository is
// also rejected -- writing this package's hook at "<repoPath>/hooks/" would
// land outside that repo's real (nested ".git/hooks") hook directory and
// never run, while the config calls below would have silently hardened
// nothing.
func ReconcileMirror(ctx context.Context, repoPath, hookBinaryPath string) error {
	info, err := os.Stat(repoPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("statting mirror %s: %w", repoPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("mirror path %s exists but is not a directory", repoPath)
	}
	if err := verifyBareRepo(ctx, repoPath); err != nil {
		return err
	}
	if err := writeHook(repoPath, hookBinaryPath); err != nil {
		return err
	}
	if err := setConfig(ctx, repoPath, "receive.denyNonFastForwards", "true"); err != nil {
		return err
	}
	if err := setConfig(ctx, repoPath, "receive.denyDeletes", "true"); err != nil {
		return err
	}
	return nil
}

// verifyBareRepo confirms repoPath is itself a bare git repository, using
// --git-dir (which fails loudly, rc=128, on a directory that is not a git
// directory) rather than -C/-cd (which, given a directory that is not
// itself a repo, walks up its parents looking for one that is, and would
// silently read and write an ENCLOSING repository's config instead of
// erroring -- exactly the mistake this function exists to rule out before
// any hook file or config command touches repoPath).
func verifyBareRepo(ctx context.Context, repoPath string) error {
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+repoPath, "rev-parse", "--is-bare-repository")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("verifying %s is a bare git repository: %w: %s", repoPath, err, out)
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("mirror path %s is a git repository but not bare", repoPath)
	}
	return nil
}

// writeHook (over)writes repoPath's pre-receive hook with a byte-for-byte
// copy of the compiled binary at hookBinaryPath, atomically: it reads
// hookBinaryPath's bytes once, writes them to a temp file in the same
// hooks directory (so the final os.Rename stays within one filesystem),
// chmods that temp file to hookMode, then renames it over the real hook
// path. os.Rename is atomic on POSIX filesystems, so any concurrent or
// interrupted receive-pack invocation execs either the complete old hook
// or the complete new one -- never a partially written file. Writing the
// bytes directly via os.WriteFile at the final path, as an earlier version
// of this function did (when the hook was a small string literal, not a
// copied binary), is NOT safe: os.WriteFile truncates the destination
// before writing its new content, and a crash or kill between that
// truncation and the write completing leaves an empty, still-executable
// pre-receive hook on disk -- which git treats as a hook that ran and
// exited 0, ACCEPTING every push, the exact fail-open outcome this whole
// design forbids. The same risk applies at loam-ofg.12's enroll call site,
// which runs against a live, already-serving mirror, not just at startup
// before the listener accepts connections.
//
// hookBinaryPath not existing, or not being readable, is a real error,
// never silently skipped: leaving a mirror's existing (possibly stale, or
// entirely absent) hook in place because the new one could not be read
// would leave that mirror either unprotected or running outdated policy
// logic, exactly the fail-open outcome this whole package exists to
// prevent.
func writeHook(repoPath, hookBinaryPath string) error {
	hookBinary, err := os.ReadFile(hookBinaryPath)
	if err != nil {
		return fmt.Errorf("reading hook binary %s: %w", hookBinaryPath, err)
	}
	hookPath := filepath.Join(repoPath, hookRelPath)
	hooksDir := filepath.Dir(hookPath)
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("creating hooks dir in %s: %w", repoPath, err)
	}
	tmp, err := os.CreateTemp(hooksDir, ".pre-receive-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp hook file in %s: %w", repoPath, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(hookBinary); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing temp hook file in %s: %w", repoPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing temp hook file in %s: %w", repoPath, err)
	}
	if err := os.Chmod(tmpPath, hookMode); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("setting temp hook file mode in %s: %w", repoPath, err)
	}
	if err := os.Rename(tmpPath, hookPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("installing pre-receive hook in %s: %w", repoPath, err)
	}
	return nil
}

// setConfig runs `git config <key> <value>` against the bare mirror at
// repoPath, addressed via --git-dir for the same upward-discovery reason
// verifyBareRepo's doc comment explains (-C would otherwise silently write
// to an enclosing repository if repoPath itself were ever invalid, a case
// verifyBareRepo above already rules out before this runs, but this
// function stays defensive rather than relying solely on that earlier
// check). git config always overwrites an existing key's value
// unconditionally (it neither errors nor skips on a pre-existing single
// value), so this alone is what makes ReconcileMirror's config half
// idempotent: a second call sets the exact same value again, a no-op in
// effect.
func setConfig(ctx context.Context, repoPath, key, value string) error {
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+repoPath, "config", key, value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("setting %s=%s in %s: %w: %s", key, value, repoPath, err, out)
	}
	return nil
}
