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
