package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/hooksocket"
)

func fixedEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func fixedWd(dir string, err error) func() (string, error) {
	return func() (string, error) { return dir, err }
}

// TestRun_AcceptedPushExitsZero proves the whole happy path: stdin parses,
// cwd resolves to a socket path, the dial succeeds and reports Accepted,
// and run exits 0 with no stderr output.
func TestRun_AcceptedPushExitsZero(t *testing.T) {
	t.Parallel()
	stdin := strings.NewReader("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-good\n")
	var stderr strings.Builder
	var gotSocketPath string
	var gotReq hooksocket.Request
	dial := func(socketPath string, req hooksocket.Request) (hooksocket.Response, error) {
		gotSocketPath = socketPath
		gotReq = req
		return hooksocket.Response{Accepted: true, Verdicts: []hooksocket.VerdictWire{{Ref: "refs/heads/wb-good", Allowed: true}}}, nil
	}
	env := fixedEnv(map[string]string{"LOAM_REPO": "acme/widgets", "LOAM_AGENT_NAME": "alice", "LOAM_AGENT_ID": "agent-1", "LOAM_AGENT_ROLE": "author"})
	code := run(stdin, &stderr, env, fixedWd("/data/mirrors/acme/widgets.git", nil), dial)
	assert.Equal(t, 0, code)
	assert.Empty(t, stderr.String())
	assert.Equal(t, "/data/hook.sock", gotSocketPath, "the socket path must be derived from cwd, not an environment variable")
	assert.Equal(t, "acme/widgets", gotReq.Repo)
	assert.Equal(t, "alice", gotReq.Agent.Name)
	require.Len(t, gotReq.Updates, 1)
	assert.Equal(t, "refs/heads/wb-good", gotReq.Updates[0].Ref)
}

// TestRun_MalformedStdinExitsNonzeroWithoutDialing proves a parse failure
// fails closed before ever reaching the socket.
func TestRun_MalformedStdinExitsNonzeroWithoutDialing(t *testing.T) {
	t.Parallel()
	stdin := strings.NewReader("garbage line\n")
	var stderr strings.Builder
	dialed := false
	dial := func(string, hooksocket.Request) (hooksocket.Response, error) {
		dialed = true
		return hooksocket.Response{Accepted: true}, nil
	}
	code := run(stdin, &stderr, fixedEnv(nil), fixedWd("/data/mirrors/acme/widgets.git", nil), dial)
	assert.NotEqual(t, 0, code)
	assert.False(t, dialed, "a malformed stdin must fail closed before ever dialing the policy socket")
	assert.Contains(t, stderr.String(), "loam:")
}

// TestRun_EmptyPushExitsZeroWithoutDialing proves the documented no-op:
// zero proposed ref updates never reaches the socket.
func TestRun_EmptyPushExitsZeroWithoutDialing(t *testing.T) {
	t.Parallel()
	dialed := false
	dial := func(string, hooksocket.Request) (hooksocket.Response, error) {
		dialed = true
		return hooksocket.Response{Accepted: true}, nil
	}
	code := run(strings.NewReader(""), new(strings.Builder), fixedEnv(nil), fixedWd("/data/mirrors/acme/widgets.git", nil), dial)
	assert.Equal(t, 0, code)
	assert.False(t, dialed)
}

// TestRun_GetwdFailureFailsClosed proves a cwd lookup failure (never
// observed against real git, but a real possible OS-level failure) is
// treated as fail-closed, not silently ignored.
func TestRun_GetwdFailureFailsClosed(t *testing.T) {
	t.Parallel()
	stdin := strings.NewReader("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-good\n")
	var stderr strings.Builder
	dialed := false
	dial := func(string, hooksocket.Request) (hooksocket.Response, error) {
		dialed = true
		return hooksocket.Response{Accepted: true}, nil
	}
	code := run(stdin, &stderr, fixedEnv(nil), fixedWd("", errors.New("getwd: no such file or directory")), dial)
	assert.NotEqual(t, 0, code)
	assert.False(t, dialed)
	assert.Contains(t, stderr.String(), "loam:")
}

// TestRun_CwdNotAMirrorPathFailsClosed proves a cwd that does not have the
// "<dataDir>/mirrors/<group>/<repo>.git" shape mirrorpath.DataDir expects
// (a corrupted or unexpected invocation environment) fails closed rather
// than silently deriving a nonsense socket path and dialing it anyway.
func TestRun_CwdNotAMirrorPathFailsClosed(t *testing.T) {
	t.Parallel()
	stdin := strings.NewReader("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-good\n")
	var stderr strings.Builder
	dialed := false
	dial := func(string, hooksocket.Request) (hooksocket.Response, error) {
		dialed = true
		return hooksocket.Response{Accepted: true}, nil
	}
	code := run(stdin, &stderr, fixedEnv(nil), fixedWd("/not/a/mirror/path", nil), dial)
	assert.NotEqual(t, 0, code)
	assert.False(t, dialed)
	assert.Contains(t, stderr.String(), "loam:")
}

// TestRun_DialErrorFailsClosed proves a socket connect/round-trip failure
// -- unreachable socket, timeout, whatever error dial reports -- exits
// nonzero with a loam:-prefixed explanation, never falls back to
// accepting.
func TestRun_DialErrorFailsClosed(t *testing.T) {
	t.Parallel()
	stdin := strings.NewReader("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-good\n")
	var stderr strings.Builder
	dial := func(string, hooksocket.Request) (hooksocket.Response, error) {
		return hooksocket.Response{}, errors.New("dial unix: connection refused")
	}
	code := run(stdin, &stderr, fixedEnv(nil), fixedWd("/data/mirrors/acme/widgets.git", nil), dial)
	assert.NotEqual(t, 0, code)
	assert.Contains(t, stderr.String(), "loam:")
	assert.Contains(t, stderr.String(), "connection refused")
}

// TestRun_RejectedPushPrintsOnlyFailingReasonsAndExitsNonzero proves the
// output half of atomicity: a mixed-verdict response (some refs allowed,
// one rejected) must print ONLY the rejected ref's loam:-prefixed reason
// -- never a line for the individually-allowed ref, which would be
// misleading given the whole push was rejected -- and exit nonzero.
func TestRun_RejectedPushPrintsOnlyFailingReasonsAndExitsNonzero(t *testing.T) {
	t.Parallel()
	stdin := strings.NewReader(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-good\n" +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/main\n",
	)
	var stderr strings.Builder
	dial := func(string, hooksocket.Request) (hooksocket.Response, error) {
		return hooksocket.Response{
			Accepted: false,
			Verdicts: []hooksocket.VerdictWire{
				{Ref: "refs/heads/wb-good", Allowed: true},
				{Ref: "refs/heads/main", Allowed: false, Reason: "loam: refs/heads/main is read-only (target branch)"},
			},
		}, nil
	}
	code := run(stdin, &stderr, fixedEnv(nil), fixedWd("/data/mirrors/acme/widgets.git", nil), dial)
	assert.NotEqual(t, 0, code)
	out := stderr.String()
	assert.Contains(t, out, "loam: refs/heads/main is read-only (target branch)")
	assert.Equal(t, 1, strings.Count(out, "loam:"), "only the failing ref's reason must be printed, not one for the allowed ref too")
}

// TestRun_MultipleFailingRefsPrintAllReasons proves atomicity's OUTPUT
// half against more than one failing ref: a push with TWO bad refs must
// print BOTH loam:-prefixed reasons, not stop after the first. A mutant
// that "break"s out of the printing loop after the first rejected verdict
// would still pass every other test in this file (each of which has
// exactly one failing ref) but fails here.
func TestRun_MultipleFailingRefsPrintAllReasons(t *testing.T) {
	t.Parallel()
	stdin := strings.NewReader(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/main\n" +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-owned-by-bob\n",
	)
	var stderr strings.Builder
	dial := func(string, hooksocket.Request) (hooksocket.Response, error) {
		return hooksocket.Response{
			Accepted: false,
			Verdicts: []hooksocket.VerdictWire{
				{Ref: "refs/heads/main", Allowed: false, Reason: "loam: refs/heads/main is read-only (target branch)"},
				{Ref: "refs/heads/wb-owned-by-bob", Allowed: false, Reason: "loam: wb-owned-by-bob belongs to bob"},
			},
		}, nil
	}
	code := run(stdin, &stderr, fixedEnv(nil), fixedWd("/data/mirrors/acme/widgets.git", nil), dial)
	assert.NotEqual(t, 0, code)
	out := stderr.String()
	assert.Contains(t, out, "loam: refs/heads/main is read-only (target branch)")
	assert.Contains(t, out, "loam: wb-owned-by-bob belongs to bob")
	assert.Equal(t, 2, strings.Count(out, "loam:"), "every failing ref's reason must be printed, not just the first")
}

// TestRun_HardEvaluationErrorResponseFailsClosed is the MUST-FIX case a
// review of this bead caught: a rejected response with NO per-ref
// verdicts at all -- exactly the shape internal/hooksocket.Server's own
// evaluate produces on a hard evaluation error, {Accepted: false,
// Verdicts: nil}, when Postgres is down or a context deadline expires
// mid-lookup -- must still exit nonzero. A mutant of the form
// `if resp.Accepted || len(resp.Verdicts) == 0 { return 0 }` accepts
// every such push and passed this package's whole suite before this test
// existed, because every OTHER test's rejected response carries at least
// one verdict.
func TestRun_HardEvaluationErrorResponseFailsClosed(t *testing.T) {
	t.Parallel()
	stdin := strings.NewReader("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-good\n")
	var stderr strings.Builder
	dial := func(string, hooksocket.Request) (hooksocket.Response, error) {
		return hooksocket.Response{Accepted: false, Verdicts: nil}, nil
	}
	code := run(stdin, &stderr, fixedEnv(nil), fixedWd("/data/mirrors/acme/widgets.git", nil), dial)
	assert.NotEqual(t, 0, code, "a rejected response with no verdicts (a hard evaluation error on the server side) must still fail closed")
	assert.Contains(t, stderr.String(), "loam:", "the agent must see SOME loam:-prefixed explanation, not silence, even when the server had no per-ref detail to give")
}

// TestRun_ForwardsTheObjectQuarantineDirectory proves the hook relays
// receive-pack's own GIT_QUARANTINE_PATH to the server. It is load-bearing
// rather than incidental: the pushed objects live ONLY in that directory
// while pre-receive runs, so without it the server's catch-up check
// (internal/catchup) cannot resolve the pushed tip at all and no
// conflict-flagged branch could ever recover by push.
func TestRun_ForwardsTheObjectQuarantineDirectory(t *testing.T) {
	t.Parallel()
	stdin := strings.NewReader("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-good\n")
	var gotReq hooksocket.Request
	dial := func(_ string, req hooksocket.Request) (hooksocket.Response, error) {
		gotReq = req
		return hooksocket.Response{Accepted: true}, nil
	}
	quarantine := "/data/mirrors/acme/widgets.git/objects/tmp_objdir-incoming-Ab12Cd"
	env := fixedEnv(map[string]string{"LOAM_REPO": "acme/widgets", "GIT_QUARANTINE_PATH": quarantine})
	code := run(stdin, new(strings.Builder), env, fixedWd("/data/mirrors/acme/widgets.git", nil), dial)
	require.Equal(t, 0, code)
	assert.Equal(t, quarantine, gotReq.QuarantineDir)
}

// TestRun_AbsentQuarantinePathIsForwardedAsEmpty proves an environment
// without GIT_QUARANTINE_PATH (an older git, or a hook invoked outside a
// real push) produces an empty field rather than a failure -- the server
// treats that as "nothing extra to read".
func TestRun_AbsentQuarantinePathIsForwardedAsEmpty(t *testing.T) {
	t.Parallel()
	stdin := strings.NewReader("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-good\n")
	var gotReq hooksocket.Request
	dial := func(_ string, req hooksocket.Request) (hooksocket.Response, error) {
		gotReq = req
		return hooksocket.Response{Accepted: true}, nil
	}
	code := run(stdin, new(strings.Builder), fixedEnv(map[string]string{"LOAM_REPO": "acme/widgets"}), fixedWd("/data/mirrors/acme/widgets.git", nil), dial)
	require.Equal(t, 0, code)
	assert.Empty(t, gotReq.QuarantineDir)
}
