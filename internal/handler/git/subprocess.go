package git

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

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

// subprocessWaitDelay bounds how long a canceled request's git subprocess
// gets to finish its own I/O teardown after being killed, before
// (*exec.Cmd).Wait forces its stdio pipes closed and returns anyway (added
// in Go 1.20 exec specifically for this class of hang). Context
// cancellation (a client disconnecting mid-clone or mid-push) must never
// leave this handler's goroutine blocked forever even in a pathological
// case where the killed process's own I/O goroutines do not unblock on
// their own -- see gitCommand's doc comment.
const subprocessWaitDelay = 5 * time.Second

// pktLine encodes s as a single pkt-line: a 4-hex-digit length prefix
// (the length of the prefix itself PLUS s, per the pkt-line format smart
// HTTP piggybacks on) followed by s verbatim. Used only for the one
// hand-written line this handler ever emits itself -- the "# service=...\n"
// header docs/git-spec.md's "Endpoint & Protocol" requires ahead of the
// real advertisement -- everything else on the wire is real git's own
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
// reading from. subprocessWaitDelay is the second half of that guarantee:
// even if the killed process's stdin/stdout pump goroutines somehow never
// observe the kill, Wait gives up on them and returns instead of hanging
// this handler's goroutine indefinitely.
//
// env is deliberately NOT os.Environ() plus additions: this subprocess
// serves an agent's clone/push over HTTP, it does not authenticate
// outward to anything, so none of the ambient credential-helper machinery
// gittransport.Transport's own gitEnv isolates against (osxkeychain,
// inherited GIT_* trace/credential vars) is something this process should
// ever need -- but leaving it in reach anyway, by inheriting the full host
// environment, is exactly the "an inherited GIT_* var from the server's
// environment reaching upload-pack is still a real hazard" case this
// bead's own instructions call out. Building an explicit, minimal
// environment (PATH so git can find its own libexec helpers,
// GIT_CONFIG_NOSYSTEM so an ambient system gitconfig's credential.helper
// can never activate even for some future hook path that talks outward,
// GIT_TERMINAL_PROMPT=0 so a misconfigured mirror can never block waiting
// on a tty prompt from a request handler goroutine) costs nothing here and
// closes that hazard outright rather than trusting every future change to
// this handler to keep re-deriving why it was safe.
func gitCommand(ctx context.Context, subcommand, mirrorDir string, extraArgs []string, gitProtocol string, extraEnv []string) *exec.Cmd {
	args := append([]string{subcommand, "--stateless-rpc"}, extraArgs...)
	args = append(args, mirrorDir)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = subprocessWaitDelay
	env := []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
	if gitProtocol != "" {
		env = append(env, "GIT_PROTOCOL="+gitProtocol)
	}
	cmd.Env = append(env, extraEnv...)
	return cmd
}

// advertisementContentType and rpcResultContentType render the two
// Content-Type shapes docs/git-spec.md pins ("application/x-git-upload-
// pack-advertisement" / "...-result", and the receive-pack equivalents) --
// a single concatenation expression covers both since service is always
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
// service header plus a flush (docs/git-spec.md "Endpoint & Protocol"),
// then `git <subcommand> --stateless-rpc --advertise-refs <mirrorDir>`'s
// own stdout, piped straight to the response with no buffering in
// between -- streaming a large ref advertisement is no different from
// streaming a large pack, so it gets the same treatment as serveRPC.
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
	cmd := gitCommand(r.Context(), subcommandFor(service), mirrorDir, []string{"--advertise-refs"}, r.Header.Get(gitProtocolHeader), nil)
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
	cmd := gitCommand(r.Context(), subcommandFor(service), mirrorDir, nil, r.Header.Get(gitProtocolHeader), extraEnv)
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
// if Content-Encoding: gzip is set (docs/git-spec.md's own instructions:
// "request bodies may be gzip-encoded ... git sends this" -- confirmed
// against git's own remote-curl.c source, which gzip-compresses a
// git-upload-pack POST body once it exceeds 1024 bytes). The returned
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
