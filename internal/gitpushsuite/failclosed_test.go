package gitpushsuite

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFailClosed_PolicySocketDown_PushRejectedThroughRealHTTPHeaders is
// this bead's own acceptance criterion, named verbatim on loam-yox ("Git
// transport push-matrix suite (loam-li0.7) green incl. socket-down
// fail-closed"): the policy socket is never started at all (newStack's
// startSocket=false -- the real compiled hook still installs, it just has
// nothing to dial), and a push from a VALID, fully-configured identity
// must still be rejected, never silently accepted.
//
// internal/hooksocket/e2e_test.go's own TestE2E_PolicySocketDown_
// PushFailsClosed already proves the hook's own fail-closed behavior for
// real, but through a fixture that injects identity straight into request
// context (see that file's withIdentity helper) rather than sending real
// Loam-Agent-* HTTP headers. This test is the one place that reason
// string is exercised through the real HTTP path end to end -- real
// header-carrying git client, real httpauth.GitIdentity, real
// handler.GitRoleGate, real compiled hook -- which is exactly the demo
// step (loam-yox) whose wording nobody else has driven this way.
func TestFailClosed_PolicySocketDown_PushRejectedThroughRealHTTPHeaders(t *testing.T) {
	t.Parallel()
	env := newStack(t, nil, loamhookBinary, false) // false: the policy socket is never started at all
	clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
	commitFile(t, clonePath, "failclosed.txt", "must never land")
	out, err := pushRef(t, clonePath, "refs/heads/wb-anything")
	require.Error(t, err, "a push must be rejected when the policy socket is unreachable, never silently accepted: %s", out)
	assert.Contains(t, out, "remote: loam:", "the hook's own fail-closed explanation must still reach the real git client")
	assert.Contains(t, out, "connect", "the hook's fail-closed message must name the connection failure")
	assert.Empty(t, mirrorRefSHA(t, env.mirrorDir, "refs/heads/wb-anything"), "the ref must never have been created on the mirror")
}
