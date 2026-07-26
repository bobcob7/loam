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
// The pre-receive hook's actual policy-enforcement BEHAVIOR belongs to
// loam-ofg.18 (the policy socket), not this package: hookScript below is a
// deliberate placeholder seam for that bead to replace wholesale, not a
// real implementation. See hookScript's doc comment.
package mirrorreconcile

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// hookRelPath is the pre-receive hook's path relative to a bare mirror's
// git directory (a bare mirror's git directory IS the repo root, unlike a
// non-bare repo's .git subdirectory) -- docs/git-spec.md "Enforcement
// Mechanics" and loam-ofg.19's own DESIGN note: "the pre-receive hook file
// ... at /hooks/pre-receive (0755)".
const hookRelPath = "hooks/pre-receive"

// hookMode is the mode git requires a hook file to run under: executable,
// per docs/git-spec.md's own "(0755)" callout.
const hookMode = 0o755

// hookScript is the pre-receive hook stub content this package installs.
// Its real body -- dialing <LOAM_DATA_DIR>/hook.sock, forwarding git's
// pre-receive stdin ("<old-sha> <new-sha> <ref>" per line, one invocation
// for the whole push) plus the identity env the receive-pack subprocess
// sets (LOAM_AGENT_NAME/LOAM_AGENT_ID/LOAM_AGENT_ROLE/LOAM_REPO), and
// exiting non-zero on any rejected ref -- belongs entirely to loam-ofg.18
// (docs/git-spec.md "Enforcement Mechanics": "a trivial stub that forwards
// the proposed ref updates, plus the identity ... to the server over a
// unix socket, and passes or fails on the answer"). This package only owns
// getting a file onto disk at the right path, mode, and idempotently --
// not what that file does once loam-ofg.18 lands.
//
// Until then, this stub exits non-zero unconditionally: docs/git-spec.md's
// own policy design is fail-closed ("socket down means push rejected"), so
// a stub with no socket to dial yet should refuse every push rather than
// silently accept it -- the same silent-wrong-behavior-is-worse-than-loud-
// failure choice cmd/server/main.go's notImplementedOrchestrator already
// makes for the ingest pipeline. No caller in this tree invokes
// receive-pack against a real mirror today (loam-ofg.16, the git
// smart-HTTP handler, is still open), so this exit code is inert in
// production until ofg.16 and ofg.18 both land -- at which point ofg.18
// replaces this constant's content (and, if the path or mode ever need to
// change, hookRelPath/hookMode above) with the real dispatch logic, not
// this package.
const hookScript = `#!/bin/sh
# Placeholder pre-receive hook stub. See internal/mirrorreconcile's
# hookScript doc comment: loam-ofg.18 replaces this file's content with the
# real policy-socket dispatch. Fails closed until then.
echo "loam: pre-receive policy socket not yet implemented (loam-ofg.18)" >&2
exit 1
`

// ReconcileMirror idempotently writes the pre-receive hook stub and the
// receive.denyNonFastForwards / receive.denyDeletes git config into the
// bare mirror at repoPath, per docs/git-spec.md "Enforcement Mechanics".
// Every call unconditionally (over)writes the hook file's bytes and mode,
// then sets both config keys -- there is no read-modify-write diff step,
// so two calls in a row produce byte-identical, config-identical results:
// repeating this is always safe, at enrollment (loam-ofg.12) and again on
// every server startup (docs/server-spec.md Startup step 3). Nothing here
// touches the mirror's refs, objects, or any other config key, so a
// reconciliation pass never disturbs work already stored in the mirror.
//
// A repoPath that does not exist on disk is NOT an error: it is either not
// yet cloned, or lost -- both derived state docs/server-spec.md's Startup
// step 3 says the next Mirror Sync cycle re-clones, with Postgres as the
// record of enrollment, not the mirror's presence on disk. ReconcileMirror
// returns nil rather than erroring so one missing mirror never aborts
// reconciliation of every other enrolled repo.
func ReconcileMirror(ctx context.Context, repoPath string) error {
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
	if err := writeHook(repoPath); err != nil {
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

// writeHook (over)writes repoPath's pre-receive hook with hookScript,
// creating the hooks directory first in case it is somehow missing (a
// `git init --bare` mirror always has one, populated with git's *.sample
// files, never a real pre-receive). It always chmods the file after
// writing: os.WriteFile only applies its mode argument when CREATING a
// file, not when overwriting an existing one (open(2)'s O_CREAT semantics
// -- an existing file keeps whatever mode it already had), so without this
// second call a hook that already existed with some other mode (e.g.
// 0o644, non-executable, left by an older version of this package or hand
// edited) would silently stay non-executable forever, defeating the hook
// on every push after the first reconciliation left it wrong.
func writeHook(repoPath string) error {
	hookPath := filepath.Join(repoPath, hookRelPath)
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		return fmt.Errorf("creating hooks dir in %s: %w", repoPath, err)
	}
	if err := os.WriteFile(hookPath, []byte(hookScript), hookMode); err != nil {
		return fmt.Errorf("writing pre-receive hook in %s: %w", repoPath, err)
	}
	if err := os.Chmod(hookPath, hookMode); err != nil {
		return fmt.Errorf("setting pre-receive hook mode in %s: %w", repoPath, err)
	}
	return nil
}

// setConfig runs `git config <key> <value>` against the bare mirror at
// repoPath. git config always overwrites an existing key's value
// unconditionally (it neither errors nor skips on a pre-existing value),
// so this alone is what makes ReconcileMirror's config half idempotent: a
// second call sets the exact same value again, a no-op in effect.
func setConfig(ctx context.Context, repoPath, key, value string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "config", key, value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("setting %s=%s in %s: %w: %s", key, value, repoPath, err, out)
	}
	return nil
}
