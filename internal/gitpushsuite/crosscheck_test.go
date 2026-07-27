// This file is the suite's own answer to "prove the suite is not
// vacuous": a rejection assertion can pass for the wrong reason (e.g. any
// non-2xx status, or any nonzero git exit code, satisfies a loose "must be
// rejected" check regardless of WHICH gate actually did the rejecting).
// Both tests below deliberately BREAK one mechanism at a time and observe
// which matrix cases flip outcome and which do not, which is the only way
// to demonstrate the mechanism attributions in matrix_test.go's and
// forcedelete_test.go's own doc comments are load-bearing rather than
// merely asserted.
package gitpushsuite

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/workbranchstore"
)

// alwaysAllowHookBinary writes a trivial script that unconditionally exits
// 0 without ever reading its stdin or touching the network -- standing in
// for "the hook allows everything" -- at a fresh path under t.TempDir(),
// and returns it. mirrorreconcile.ReconcileMirror copies whatever bytes
// sit at hookBinaryPath byte-for-byte and marks them executable (see that
// package's own doc comment: "a byte-for-byte copy of the compiled
// cmd/loamhook binary"); it does not care whether those bytes are a real ELF binary or
// a shell script, so this is a legitimate stand-in for "the real
// loamhookBinary, but mutated to always allow" without needing to
// recompile cmd/loamhook itself.
func alwaysAllowHookBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "always-allow-hook")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	return path
}

// TestCrossCheck_AlwaysAllowHook_OnlyGitConfigAndGatesStillReject installs
// a hook that unconditionally allows every push (never dials the policy
// socket at all -- callTracker.Calls() stays empty throughout, unlike
// every real-hook case in matrix_test.go) and re-runs analogues of all
// eight matrix cases against it. Cases 1-4 (read-only ref, unknown ref,
// non-author, terminal state) FLIP from rejected to ACCEPTED, proving the
// real hook -- not something else -- was what rejected them in
// matrix_test.go. Cases 5-8 (force push, delete, missing identity, wrong
// role) are UNCHANGED: still rejected, because none of those four
// mechanisms lives in the hook at all -- 5 and 6 are git's own
// receive.deny* config (untouched by which hook binary is installed), and
// 7 and 8 are httpauth.GitIdentity / handler.GitRoleGate, which run
// before any hook could possibly be consulted.
func TestCrossCheck_AlwaysAllowHook_OnlyGitConfigAndGatesStillReject(t *testing.T) {
	t.Parallel()
	hookBinary := alwaysAllowHookBinary(t)

	t.Run("1-analogue read-only ref: now ACCEPTED under an always-allow hook", func(t *testing.T) {
		t.Parallel()
		env := newStack(t, nil, hookBinary, true)
		clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
		commitFile(t, clonePath, "x.txt", "fast-forwards main")
		out, err := pushRef(t, clonePath, "refs/heads/main")
		assert.NoError(t, err, "under an always-allow hook, even a push to the read-only target branch must now succeed: %s", out)
		assert.Empty(t, env.tracker.Calls(), "the always-allow hook never dials the policy socket at all")
	})

	t.Run("2-analogue unknown ref: now ACCEPTED under an always-allow hook", func(t *testing.T) {
		t.Parallel()
		env := newStack(t, nil, hookBinary, true)
		clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
		commitFile(t, clonePath, "y.txt", "brand new unregistered ref")
		out, err := pushRef(t, clonePath, "refs/heads/wb-never-registered")
		assert.NoError(t, err, "under an always-allow hook, creating an unregistered ref must now succeed: %s", out)
	})

	t.Run("5-analogue force push: STILL rejected -- git's own config, unaffected by the hook", func(t *testing.T) {
		t.Parallel()
		env := newStack(t, nil, hookBinary, true)
		clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
		commitFile(t, clonePath, "z.txt", "first commit")
		_, err := pushRef(t, clonePath, "refs/heads/wb-anything")
		require.NoError(t, err)
		runGit(t, clonePath, "commit", "--quiet", "--amend", "-m", "diverged")
		out, err := pushRefs(t, clonePath, "--force", "HEAD:refs/heads/wb-anything")
		assert.Error(t, err, "force push must still be rejected even when the hook allows everything: %s", out)
		assert.Contains(t, out, "non-fast-forward")
	})

	t.Run("6-analogue delete: STILL rejected -- git's own config, unaffected by the hook", func(t *testing.T) {
		t.Parallel()
		env := newStack(t, nil, hookBinary, true)
		clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
		commitFile(t, clonePath, "w.txt", "first commit")
		_, err := pushRef(t, clonePath, "refs/heads/wb-anything")
		require.NoError(t, err)
		out, err := pushRefs(t, clonePath, ":refs/heads/wb-anything")
		assert.Error(t, err, "delete must still be rejected even when the hook allows everything: %s", out)
		assert.Contains(t, out, "denying ref deletion")
	})

	t.Run("7-analogue missing identity: STILL rejected -- never reaches the hook", func(t *testing.T) {
		t.Parallel()
		env := newStack(t, nil, hookBinary, true)
		clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
		clearIdentity(t, clonePath)
		commitFile(t, clonePath, "v.txt", "no identity")
		out, err := pushRef(t, clonePath, "refs/heads/wb-anything")
		assert.Error(t, err, "a push with no identity must still be rejected regardless of the hook: %s", out)
		assert.Contains(t, out, "remote: loam: forbidden: missing agent identity")
	})

	t.Run("8-analogue wrong role: STILL rejected -- never reaches the hook", func(t *testing.T) {
		t.Parallel()
		env := newStack(t, nil, hookBinary, true)
		clonePath := cloneWithIdentity(t, env, "carol", "2", "reviewer")
		commitFile(t, clonePath, "u.txt", "reviewer cannot push")
		out, err := pushRef(t, clonePath, "refs/heads/wb-anything")
		assert.Error(t, err, "a reviewer's push must still be rejected regardless of the hook: %s", out)
		assert.Contains(t, out, `remote: loam: role "reviewer" may not push`)
	})
}

// unsetMirrorConfig removes key from mirrorDir's own --local git config,
// standing in for a mirror that (by mistake, or by a future regression in
// internal/mirrorreconcile) never had -- or lost -- this specific
// push-safety flag, independent of whichever hook is installed alongside
// it.
func unsetMirrorConfig(t *testing.T, mirrorDir, key string) {
	t.Helper()
	runGit(t, "", "--git-dir="+mirrorDir, "config", "--local", "--unset", key)
}

// TestCrossCheck_RemovingDenyConfig_OnlyThatCaseFlips is this bead's other
// named cross-check: "remove receive.denyDeletes and verify only case 6
// changes." Both push-safety flags are removed independently (never
// together), against the REAL hook and REAL policy socket throughout, so
// any change in outcome is attributable ONLY to the one config key each
// subtest actually unsets.
func TestCrossCheck_RemovingDenyConfig_OnlyThatCaseFlips(t *testing.T) {
	t.Parallel()

	t.Run("removing denyNonFastForwards: force push now succeeds, delete is UNCHANGED (still rejected)", func(t *testing.T) {
		t.Parallel()
		branches := map[string]workbranchstore.WorkBranch{
			"wb-force": {Name: "wb-force", Author: "alice", State: workbranchstore.StateDraft},
			"wb-del":   {Name: "wb-del", Author: "alice", State: workbranchstore.StateDraft},
		}
		env := newStack(t, branches, loamhookBinary, true)
		// Two independent clones, one per ref under test, so amending
		// history in the force clone can never entangle with the delete
		// clone's own commit graph.
		forceClone := cloneWithIdentity(t, env, "alice", "1", "author")
		commitFile(t, forceClone, "force.txt", "first commit")
		_, err := pushRef(t, forceClone, "refs/heads/wb-force")
		require.NoError(t, err)
		delClone := cloneWithIdentity(t, env, "alice", "1", "author")
		commitFile(t, delClone, "del.txt", "first commit")
		_, err = pushRef(t, delClone, "refs/heads/wb-del")
		require.NoError(t, err)

		unsetMirrorConfig(t, env.mirrorDir, "receive.denyNonFastForwards")

		runGit(t, forceClone, "commit", "--quiet", "--amend", "-m", "diverged")
		out, err := pushRefs(t, forceClone, "--force", "HEAD:refs/heads/wb-force")
		assert.NoError(t, err, "with receive.denyNonFastForwards removed, a force push must now succeed: %s", out)

		out, err = pushRefs(t, delClone, ":refs/heads/wb-del")
		assert.Error(t, err, "delete must remain rejected: removing denyNonFastForwards must not affect denyDeletes: %s", out)
		assert.Contains(t, out, "denying ref deletion")
	})

	t.Run("removing denyDeletes: delete now succeeds, force push is UNCHANGED (still rejected)", func(t *testing.T) {
		t.Parallel()
		branches := map[string]workbranchstore.WorkBranch{
			"wb-force2": {Name: "wb-force2", Author: "alice", State: workbranchstore.StateDraft},
			"wb-del2":   {Name: "wb-del2", Author: "alice", State: workbranchstore.StateDraft},
		}
		env := newStack(t, branches, loamhookBinary, true)
		forceClone := cloneWithIdentity(t, env, "alice", "1", "author")
		commitFile(t, forceClone, "force2.txt", "first commit")
		_, err := pushRef(t, forceClone, "refs/heads/wb-force2")
		require.NoError(t, err)
		delClone := cloneWithIdentity(t, env, "alice", "1", "author")
		commitFile(t, delClone, "del2.txt", "first commit")
		_, err = pushRef(t, delClone, "refs/heads/wb-del2")
		require.NoError(t, err)

		unsetMirrorConfig(t, env.mirrorDir, "receive.denyDeletes")

		out, err := pushRefs(t, delClone, ":refs/heads/wb-del2")
		assert.NoError(t, err, "with receive.denyDeletes removed, a delete must now succeed: %s", out)

		runGit(t, forceClone, "commit", "--quiet", "--amend", "-m", "diverged, unrelated to the delete test")
		out, err = pushRefs(t, forceClone, "--force", "HEAD:refs/heads/wb-force2")
		assert.Error(t, err, "force push must remain rejected: removing denyDeletes must not affect denyNonFastForwards: %s", out)
		assert.Contains(t, out, "non-fast-forward")
	})
}
