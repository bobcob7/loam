// Command loamhook is the pre-receive hook stub docs/git-spec.md
// "Enforcement Mechanics" describes: internal/mirrorreconcile installs a
// copy of this compiled binary at every bare mirror's hooks/pre-receive
// (0755). git execs it once per push, feeding the whole set of proposed
// ref updates on stdin ("<old-sha> <new-sha> <ref>" per line) and the
// pushing agent's identity via environment variables
// (LOAM_AGENT_NAME/_ID/_ROLE, LOAM_REPO -- set on the receive-pack
// subprocess by internal/handler/git's serveRPC, per its own doc
// comment). This binary's entire job is to forward that one request over
// the policy socket at <LOAM_DATA_DIR>/hook.sock and translate the
// server's verdict into an exit code and, on rejection, one loam:-prefixed
// stderr line per failing ref -- which git's own remote-helper prefixes
// with "remote: " when relaying it back to the pushing client (git's own
// documented behavior for pre-receive hook stderr, not anything
// docs/git-spec.md itself states).
//
// This is a genuinely separate, minimal Go binary -- not a POSIX shell
// script -- because the wire protocol is JSON over a unix domain socket,
// and this repo makes no assumption that curl, nc, or any other external
// tool capable of speaking to a unix socket is present in the minimal
// environment internal/handler/git's gitCommand builds for the
// receive-pack subprocess (PATH, GIT_CONFIG_NOSYSTEM, GIT_TERMINAL_PROMPT,
// GIT_PROTOCOL, plus the four LOAM_* identity variables -- see that
// function's own doc comment for why it is NOT os.Environ() plus
// additions). A compiled Go binary needs nothing from that environment
// beyond PATH (to be found at all) and carries its own socket-speaking
// logic, so it has no such external-tool dependency to justify.
//
// It deliberately learns the policy socket's location from its own
// working directory rather than an environment variable or a baked-in
// path: git invokes every hook with the bare repository as the process's
// cwd (verified empirically against real git during this bead's research;
// docs/git-spec.md does not itself state this), and internal/mirrorpath's
// Dir/DataDir pair is the single, already-established source of the
// "<LOAM_DATA_DIR>/mirrors/<group>/<repo_name>.git" convention this
// inverts. This means the exact same compiled bytes work, unmodified, when
// copied into every mirror on the host -- no per-mirror customization of
// this binary's content is needed at install time.
package main

import (
	"os"

	"github.com/bobcob7/loam/internal/hooksocket"
)

func main() {
	dial := func(socketPath string, req hooksocket.Request) (hooksocket.Response, error) {
		return hooksocket.Call(socketPath, req, hooksocket.DialTimeout, hooksocket.RPCTimeout)
	}
	os.Exit(run(os.Stdin, os.Stderr, os.Getenv, os.Getwd, dial))
}
