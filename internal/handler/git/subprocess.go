package git

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"

	"github.com/bobcob7/loam/internal/gitrun"
	"github.com/bobcob7/loam/internal/httpauth"
)

// gitProtocolHeader is the request header real git clients send to
// negotiate protocol v2 (docs/git-spec.md "Endpoint & Protocol": "smart
// protocol only (v2 for fetch)"). Forwarding its value onto the
// subprocess's GIT_PROTOCOL environment variable is what makes `git
// upload-pack` actually reply in v2 -- an http-backend CGI invocation
// gets this for free from the CGI environment; a manual exec.Cmd does not,
// so this handler must do it itself.
const gitProtocolHeader = "Git-Protocol"

// The subprocessWaitDelay internal/gitrun.NewCommand sets on every *exec.Cmd
// it builds (including this package's own, since loam-ldx routed gitCommand
// through it) bounds two things per (*exec.Cmd).WaitDelay's own doc
// comment: how long a canceled request's process gets to exit on its own
// before Wait sends it a Kill, and how long Wait then waits for the PIPES
// BETWEEN Cmd AND THE CHILD to close before forcibly closing them itself to
// unblock a goroutine reading the child's stdout or writing the child's
// stdin. It does NOT bound every possible way this handler's goroutine
// could stay blocked: measured directly (a client that stalls mid-body
// while the subprocess is producing output), the stdout-copying goroutine
// can be blocked inside w.Write itself -- net/http's server draining the
// unread request body there -- which closing the pipe between Cmd and the
// child process cannot unblock, since that block is on the RESPONSE side,
// not the child's own I/O. WaitDelay is still worth keeping (it is what
// unblocks the case it actually covers: a child that has exited or been
// killed but left its own pipes open), it just is not a universal "this
// handler's goroutine returns within N seconds no matter what" guarantee --
// do not read it as one.

// pktLine encodes s as a single pkt-line: a 4-hex-digit length prefix
// (the length of the prefix itself PLUS s, per the pkt-line format smart
// HTTP piggybacks on) followed by s verbatim. Used only for the one
// hand-written line this handler ever emits itself -- the "# service=...\n"
// header git's own smart-HTTP protocol requires ahead of the real
// advertisement (git-scm.com/docs/http-protocol; docs/git-spec.md names
// the endpoint but does not itself describe pkt-line framing) --
// everything else on the wire is real git's own
// pkt-line-framed stdout, piped straight through.
func pktLine(s string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(s)+4, s))
}

// flushPkt is the pkt-line flush packet ("0000"): a fixed 4 bytes, no
// payload, no length-prefixed content. It terminates the hand-written
// service header ahead of the real advertisement, which is followed by
// upload-pack/receive-pack's OWN flush -- this handler adds exactly one
// flush of its own, never two.
var flushPkt = []byte("0000")

// gitCommand builds the exec.Cmd for one `git <subcommand> --stateless-rpc
// [--advertise-refs] <mirrorDir>` invocation, tied to ctx (normally
// r.Context()) so a client disconnecting mid-request -- net/http cancels
// a request's Context when its underlying connection closes -- kills the
// subprocess via exec.CommandContext's default Cancel (Process.Kill)
// rather than leaving it running forever against a mirror nobody is still
// reading from. The returned cleanup func removes this invocation's
// isolated HOME (see internal/gitrun.NewIsolatedHome) and must be called
// once the subprocess this built for has exited, however it exited.
//
// loam-ldx: this package was one of four (of what turned out to be seven)
// carbon copies of the same "run a local git subprocess with hardened,
// isolated config" pair, and the WEAKEST of them -- unlike its siblings,
// it built its environment from PATH, GIT_CONFIG_NOSYSTEM, and
// GIT_TERMINAL_PROMPT alone, with no HOME redirection at all, leaving a
// user-level ~/.gitconfig on whatever host runs the loam server free to
// reach every upload-pack/receive-pack invocation (GIT_CONFIG_NOSYSTEM
// blocks only the SYSTEM gitconfig, not that layer). It now builds its
// environment through internal/gitrun.Env, same as every other absorbed
// copy, closing that gap: this subprocess still authenticates outward to
// nothing (it serves an agent's clone/push over HTTP, never talks to a
// remote itself), so gitrun.Env's isolation costs nothing here and removes
// one more place a hardening fix could be applied everywhere else and
// missed here. GIT_PROTOCOL (from the client's own negotiation header) and
// extraEnv (the receive-pack CRITICAL SEAM identity vars -- see serveRPC)
// are appended after gitrun.Env's own list, exactly as before.
func gitCommand(ctx context.Context, subcommand, mirrorDir string, extraArgs []string, gitProtocol string, extraEnv []string) (*exec.Cmd, func(), error) {
	home, cleanup, err := gitrun.NewIsolatedHome()
	if err != nil {
		return nil, func() {}, fmt.Errorf("creating isolated git environment: %w", err)
	}
	args := append([]string{subcommand, "--stateless-rpc"}, extraArgs...)
	args = append(args, mirrorDir)
	env := gitrun.Env(home)
	if gitProtocol != "" {
		env = append(env, "GIT_PROTOCOL="+gitProtocol)
	}
	cmd := gitrun.NewCommand(ctx, append(env, extraEnv...), nil, nil, nil, args...)
	return cmd, cleanup, nil
}

// advertisementContentType and rpcResultContentType render the two
// Content-Type shapes git's own smart-HTTP protocol requires
// ("application/x-git-upload-pack-advertisement" / "...-result", and the
// receive-pack equivalents -- git-scm.com/docs/http-protocol, not
// docs/git-spec.md, which does not itself name these MIME types) -- a
// single concatenation expression covers both since service is always
// exactly "git-upload-pack" or "git-receive-pack" (parseGitRequest's
// contract).
func advertisementContentType(service string) string {
	return "application/x-" + service + "-advertisement"
}

func rpcResultContentType(service string) string {
	return "application/x-" + service + "-result"
}

// subcommandFor renders service ("git-upload-pack"/"git-receive-pack") as
// the git subcommand name ("upload-pack"/"receive-pack") docs/git-spec.md
// "Enforcement Mechanics" names.
func subcommandFor(service string) string {
	if service == serviceReceivePack {
		return "receive-pack"
	}
	return "upload-pack"
}

// serveInfoRefs answers GET .../info/refs?service=... : the pkt-line
// service header plus a flush (git's own smart-HTTP protocol; see
// pktLine's doc comment), then `git <subcommand> --stateless-rpc
// --advertise-refs <mirrorDir>`'s own stdout, piped straight to the
// response with no buffering in between -- streaming a large ref
// advertisement is no different from streaming a large pack, so it gets
// the same treatment as serveRPC.
func (h *Handler) serveInfoRefs(w http.ResponseWriter, r *http.Request, mirrorDir, service string) {
	w.Header().Set("Content-Type", advertisementContentType(service))
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(pktLine("# service=" + service + "\n")); err != nil {
		return
	}
	if _, err := w.Write(flushPkt); err != nil {
		return
	}
	cmd, cleanup, err := gitCommand(r.Context(), subcommandFor(service), mirrorDir, []string{"--advertise-refs"}, r.Header.Get(gitProtocolHeader), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "git handler: failed to prepare advertise-refs subprocess", "service", service, "mirror", mirrorDir, "error", err)
		return
	}
	defer cleanup()
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		h.logger.ErrorContext(r.Context(), "git handler: advertise-refs subprocess failed", "service", service, "mirror", mirrorDir, "error", err, "stderr", stderr.String())
	}
}

// serveRPC answers POST .../git-upload-pack or .../git-receive-pack: it
// decompresses a gzip-encoded request body if present (real git clients
// send one for a sufficiently large upload-pack request; see this
// package's tests for the exact threshold verified against git's own
// source), pipes it straight to the subprocess's stdin, and streams the
// subprocess's stdout straight back as the response body -- never
// buffering a request or response in memory, so a large packfile costs
// this handler no more memory than a small one.
//
// For git-receive-pack, this is the CRITICAL SEAM docs/git-spec.md
// "Enforcement Mechanics" names: identity is a required precondition, not
// a graceful fallback -- internal/handler.GitRoleGate and internal/httpauth
// have already resolved it onto the request context by the time this
// handler ever runs in production, so its absence here means some other
// caller (a test, or a future mis-wiring) reached this path without going
// through them, and it fails closed with 500 rather than silently
// running receive-pack -- and therefore the pre-receive hook -- with no
// LOAM_AGENT_* environment at all.
func (h *Handler) serveRPC(w http.ResponseWriter, r *http.Request, repoName, mirrorDir, service string) {
	var extraEnv []string
	if service == serviceReceivePack {
		identity, ok := httpauth.IdentityFromContext(r.Context())
		if !ok {
			h.logger.ErrorContext(r.Context(), "git handler: receive-pack reached with no resolved caller identity", "repo", repoName)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		extraEnv = []string{
			"LOAM_AGENT_NAME=" + identity.Name,
			"LOAM_AGENT_ID=" + identity.ID,
			"LOAM_AGENT_ROLE=" + identity.Role,
			"LOAM_REPO=" + repoName,
		}
	}
	body, closeBody, err := requestBody(r)
	if err != nil {
		http.Error(w, "loam: invalid gzip request body", http.StatusBadRequest)
		return
	}
	defer closeBody()
	cmd, cleanup, err := gitCommand(r.Context(), subcommandFor(service), mirrorDir, nil, r.Header.Get(gitProtocolHeader), extraEnv)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "git handler: failed to prepare rpc subprocess", "service", service, "repo", repoName, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer cleanup()
	cmd.Stdin = body
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	w.Header().Set("Content-Type", rpcResultContentType(service))
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := cmd.Run(); err != nil {
		h.logger.ErrorContext(r.Context(), "git handler: rpc subprocess failed", "service", service, "repo", repoName, "error", err, "stderr", stderr.String())
	}
}

// requestBody returns r.Body, transparently gzip-decompressing it first
// if Content-Encoding: gzip is set. docs/git-spec.md itself says nothing
// about gzip; this is purely an empirical property of real git clients,
// confirmed against git's own remote-curl.c source (post_rpc/fetch_git):
// a git-upload-pack POST body is gzip-compressed once it exceeds 1024
// bytes (protocol v2's stateless-connect path sets gzip_request too, so
// this covers a modern client's default fetch), while git-receive-pack
// (push) is never gzip-compressed regardless of size -- confirmed by this
// package's own measurements (see the tests). The returned
// reader streams the decompression rather than buffering it: gzip.Reader
// wraps r.Body directly, so the subprocess's stdin is fed exactly as fast
// as the client sends compressed bytes, not all at once. The returned
// close func is always safe to defer unconditionally, gzip or not.
func requestBody(r *http.Request) (io.Reader, func(), error) {
	if r.Header.Get("Content-Encoding") != "gzip" {
		return r.Body, func() {}, nil
	}
	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		return nil, func() {}, fmt.Errorf("decompressing gzip request body: %w", err)
	}
	return gz, func() { _ = gz.Close() }, nil
}
