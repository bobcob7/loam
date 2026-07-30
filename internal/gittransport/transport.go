package gittransport

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrUpstreamURLHasUserinfo indicates an upstream URL carries embedded
// credentials (user:token@host), rejected by validateUpstreamURL.
// Exported (loam-ra1k) so internal/handler/repoadmin can errors.Is against
// this single sentinel at ProbeRepo and EnrollRepo, rather than
// duplicating the "does this URL carry credentials" check: this package
// is still the last-resort choke point for anything that slips past that
// earlier validation, including a repo enrolled with userinfo before
// either validation existed -- repos.upstream_url still carries the
// credential, and every scheduled sync tick for that repo fails here
// instead, wrapped up through internal/mirrorsync into repos.sync_error.
// Either way the remedy is the same, so the message says so once here:
// remove the embedded credential from the upstream URL and rely on the
// host's configured credential (internal/credentialstore) instead.
var ErrUpstreamURLHasUserinfo = errors.New("gittransport: upstream URL must not carry userinfo; remove the embedded credential and rely on the host's configured credential instead")

// validateUpstreamURL rejects an upstreamURL carrying userinfo
// (user:token@host) before it ever reaches exec.Command args. A credential
// embedded this way would land in `ps` output on every scheduler tick, and
// scrubSecrets cannot redact it -- it is not the credential this package
// itself resolves from credStore and injects via gitEnv's header, so
// scrubSecrets never learns it. forge.CheckRepo parses the URL and checks
// the host but does not reject u.User; Transport is the natural choke
// point, since every exported method (Fetch, Push, DeleteRemoteRef, Clone,
// LsRemote) takes upstreamURL as an explicit parameter and funnels it
// toward runRaw.
// Neither branch echoes anything derived from the URL itself, and that is
// the whole point rather than caution: this function exists to stop a
// credential embedded in an upstream URL from travelling, and its own
// error is returned to the caller, %w-wrapped to the RPC boundary, and on
// the enroll path written into repos.sync_error
// (internal/handler/repoadmin/enroll.go's markSyncError). An error that
// quotes the offending URL defeats the function.
//
// url.URL.Redacted() is NOT sufficient and was the original bug here: it
// masks only the PASSWORD component, and only when a ":" is actually
// present. "https://<token>@host/path" -- the standard PAT-in-URL form for
// GitHub, GitLab and Forgejo, and much the likeliest way a repo gets
// enrolled with an embedded credential -- passes through Redacted()
// verbatim, as does a percent-encoded colon ("user%3Atoken@"). Verified
// against net/url directly.
//
// The parse-failure branch is the same hazard by a different route:
// *url.Error's Error() renders as `parse "<raw url>": <reason>`, so any
// token containing a byte net/url rejects (a space, a control character)
// would land in the message whole. Neither the raw string nor the parsed
// form is safe to render, so neither is rendered; the host is enough to
// diagnose, and it is only available on the branch where parsing worked.
func validateUpstreamURL(upstreamURL string) error {
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return fmt.Errorf("%w: unparseable", ErrUpstreamURLHasUserinfo)
	}
	if u.User != nil {
		return fmt.Errorf("%w (host %s)", ErrUpstreamURLHasUserinfo, u.Host)
	}
	return nil
}

// Transport runs upstream git subprocesses (fetch, push, branch delete)
// with a forge host's token injected per invocation, per
// docs/sync-spec.md "Upstream Transport". It never caches a token: every
// call resolves the current credential from credStore and converts it
// via gitCreds immediately before the one subprocess that needs it.
type Transport struct {
	credStore credentialSource
	gitCreds  gitCredentialConverter
	logger    *slog.Logger
}

// New builds a Transport backed by credStore (typically
// *credentialstore.Store) and gitCreds (typically a *forge.Forgejo,
// which satisfies gitCredentialConverter structurally via its
// GitCredentials method -- only that method is used here).
func New(credStore credentialSource, gitCreds gitCredentialConverter, logger *slog.Logger) *Transport {
	return &Transport{credStore: credStore, gitCreds: gitCreds, logger: logger}
}

// Fetch runs a forced, pruning fetch of refspecs from upstreamURL into
// the bare mirror at mirrorDir, with host's token injected per
// invocation. refspecs is the caller's full set -- e.g. one wildcard
// positive refspec plus a negative exclusion per registered work-branch
// ref, per loam-giq.2's design; this package runs exactly what it is
// given and builds no refspec of its own. The returned bytes are
// --porcelain output with any secret scrubbed (see run), for a caller
// that wants to derive ref SHA transitions itself.
func (t *Transport) Fetch(ctx context.Context, host, mirrorDir, upstreamURL string, refspecs []string) ([]byte, error) {
	if err := validateUpstreamURL(upstreamURL); err != nil {
		return nil, fmt.Errorf("fetching into %s: %w", mirrorDir, err)
	}
	args := append([]string{"fetch", "--prune", "--force", "--porcelain", upstreamURL}, refspecs...)
	out, err := t.run(ctx, host, mirrorDir, args...)
	if err != nil {
		return nil, fmt.Errorf("fetching %s into %s: %w", upstreamURL, mirrorDir, err)
	}
	return out, nil
}

// Push runs a git push of refspec to upstreamURL from the bare mirror at
// mirrorDir, with host's token injected per invocation. Callers decide
// the refspec (a fast-forward-only push for a first accept or a
// re-accept after catch-up -- loam-giq.7's job); this package never adds
// --force, so a non-fast-forward push is rejected by the upstream, not
// silently forced.
func (t *Transport) Push(ctx context.Context, host, mirrorDir, upstreamURL, refspec string) ([]byte, error) {
	if err := validateUpstreamURL(upstreamURL); err != nil {
		return nil, fmt.Errorf("pushing %s: %w", refspec, err)
	}
	out, err := t.run(ctx, host, mirrorDir, "push", upstreamURL, refspec)
	if err != nil {
		return nil, fmt.Errorf("pushing %s to %s: %w", refspec, upstreamURL, err)
	}
	return out, nil
}

// DeleteRemoteRef runs a git push that deletes ref (a full ref path,
// e.g. "refs/heads/loam/wb-9c2f1a") on upstreamURL, with host's token
// injected per invocation -- used for upstream branch cleanup
// (loam-giq.8) once a proposal's PR reaches a terminal state.
func (t *Transport) DeleteRemoteRef(ctx context.Context, host, mirrorDir, upstreamURL, ref string) ([]byte, error) {
	if err := validateUpstreamURL(upstreamURL); err != nil {
		return nil, fmt.Errorf("deleting %s: %w", ref, err)
	}
	out, err := t.run(ctx, host, mirrorDir, "push", upstreamURL, ":"+ref)
	if err != nil {
		return nil, fmt.Errorf("deleting %s on %s: %w", ref, upstreamURL, err)
	}
	return out, nil
}

// Clone creates a fresh bare mirror at mirrorDir by cloning upstreamURL,
// with host's token injected per invocation -- the initial-enrollment leg
// docs/sync-spec.md's Mirror Sync describes as its "degenerate first
// cycle" and loam-giq.2's MirrorFetcher.Fetch itself never performs
// (Fetch's --git-dir addressing assumes mirrorDir already exists;
// nothing before loam-ofg.12 ever ran `git clone --mirror` or `git init
// --bare` anywhere in production). Any pre-existing directory at
// mirrorDir is removed first: the only legitimate way one can be present
// at a fresh CreateRepo's derived path is debris from a previous, crashed
// enrollment attempt for a repo of the same name (repos.name is unique,
// so a live, successfully-enrolled repo's mirror is never re-cloned over
// this path) -- `git clone` itself refuses a non-empty destination, so
// leaving stale debris in place would fail every retry of a
// once-interrupted enrollment. mirrorDir's parent directories are created
// as needed. Runs through the same runRaw core as Fetch/Push/
// DeleteRemoteRef, so it inherits every one of this package's isolation
// properties (per-invocation credential injection via an environment
// header, GIT_CONFIG_NOSYSTEM, redirected HOME/XDG_CONFIG_HOME, cleared
// credential helper, output/error/log scrubbing) with no separate
// implementation to keep in sync.
func (t *Transport) Clone(ctx context.Context, host, mirrorDir, upstreamURL string) ([]byte, error) {
	if err := validateUpstreamURL(upstreamURL); err != nil {
		return nil, fmt.Errorf("cloning into %s: %w", mirrorDir, err)
	}
	if err := os.RemoveAll(mirrorDir); err != nil {
		return nil, fmt.Errorf("clearing stale mirror path %s before clone: %w", mirrorDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o755); err != nil {
		return nil, fmt.Errorf("creating parent directory for mirror %s: %w", mirrorDir, err)
	}
	out, err := t.runRaw(ctx, host, "clone", "--mirror", upstreamURL, mirrorDir)
	if err != nil {
		return nil, fmt.Errorf("cloning %s into %s: %w", upstreamURL, mirrorDir, err)
	}
	return out, nil
}

// LsRemote lists upstreamURL's refs (including its HEAD symref, via
// --symref) with host's token injected per invocation, needing no local
// mirror at all -- RepoAdminService.ProbeRepo's (loam-ofg.12) read-only
// pre-enrollment branch listing. Runs through the same runRaw core as
// every other method here, for the same isolation-inheritance reason
// Clone's doc comment explains.
func (t *Transport) LsRemote(ctx context.Context, host, upstreamURL string) ([]byte, error) {
	if err := validateUpstreamURL(upstreamURL); err != nil {
		return nil, fmt.Errorf("listing refs: %w", err)
	}
	out, err := t.runRaw(ctx, host, "ls-remote", "--symref", upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("listing refs for %s: %w", upstreamURL, err)
	}
	return out, nil
}

// run executes one git subcommand against mirrorDir (via --git-dir),
// resolving host's credential the same way runRaw always does. The
// thin wrapper Fetch/Push/DeleteRemoteRef share, for the common case of
// an operation against an already-existing local mirror; Clone and
// LsRemote call runRaw directly instead, since neither operates against
// a pre-existing --git-dir (Clone creates one; LsRemote needs none at
// all).
func (t *Transport) run(ctx context.Context, host, mirrorDir string, args ...string) ([]byte, error) {
	fullArgs := append([]string{"--git-dir=" + mirrorDir}, args...)
	return t.runRaw(ctx, host, fullArgs...)
}

// runRaw executes one git subcommand with host's credential injected as a
// per-invocation HTTP Authorization header (never argv, never a config
// file, never cached). host may be empty to run anonymously with no
// credential resolution at all -- only legitimate for a caller exercising
// an anonymous fixture (e.g. a file:// URL in a test); every real forge
// host is enrolled with a token, so production call sites always pass
// one. This is the single seam every exported method (Fetch, Push,
// DeleteRemoteRef via run; Clone, LsRemote directly) funnels through, so
// the isolation properties below apply uniformly to all of them.
func (t *Transport) runRaw(ctx context.Context, host string, args ...string) ([]byte, error) {
	var token, password, authHeaderValue string
	if host != "" {
		cred, err := t.credStore.GetByHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolving credential for host %s: %w", host, err)
		}
		token = cred.Token
		username, pw, err := t.gitCreds.GitCredentials(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("converting credential for host %s to git auth: %w", host, err)
		}
		password = pw
		authHeaderValue = base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	}
	home, err := os.MkdirTemp("", "loam-gittransport-*")
	if err != nil {
		return nil, fmt.Errorf("creating isolated git environment: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	fullArgs := append([]string{"-c", "credential.helper="}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Env = gitEnv(home, authHeaderValue)
	out, cmdErr := cmd.CombinedOutput()
	scrubbed := scrubSecrets(string(out), token, password, authHeaderValue)
	// args is echoed into both the log line and the returned error below.
	// It never carries a secret today (Fetch/Push/DeleteRemoteRef/Clone/
	// LsRemote pass upstreamURL/refspecs straight through; credentials
	// live only in gitEnv's environment, never in args), but it is
	// scrubbed here too, independently of scrubbing out/scrubbed above, so
	// a future bug that did let a secret reach args could not surface it
	// through this echo either -- the same belt-and-suspenders reasoning
	// as gitEnv's GIT_TRACE*=0 overrides alongside this same scrubbing.
	scrubbedArgs := scrubSecrets(strings.Join(args, " "), token, password, authHeaderValue)
	if cmdErr != nil {
		t.logger.ErrorContext(ctx, "git subprocess failed", "args", scrubbedArgs, "err", cmdErr, "output", strings.TrimSpace(scrubbed))
		return nil, fmt.Errorf("git %s: %w: %s", scrubbedArgs, cmdErr, strings.TrimSpace(scrubbed))
	}
	t.logger.DebugContext(ctx, "git subprocess succeeded", "args", scrubbedArgs, "output", strings.TrimSpace(scrubbed))
	return []byte(scrubbed), nil
}

// gitEnv builds the environment for one git subprocess invocation,
// isolated from whatever the host machine has configured. This
// isolation is load-bearing, not hygiene: macOS's Command Line Tools
// ship a SYSTEM gitconfig
// (/Library/Developer/CommandLineTools/usr/share/git-core/gitconfig)
// that sets credential.helper=osxkeychain, which keys entries by
// protocol+host while IGNORING the port -- a real defect found today
// against exactly this class of component, where an ambient cached
// credential silently authenticated a request that was supposed to
// fail. GIT_CONFIG_NOSYSTEM drops the system file; HOME/XDG_CONFIG_HOME
// are redirected at home (a fresh, per-invocation temp directory the
// caller removes when the subprocess returns) so no user-global config
// is read either; credential.helper is separately cleared via a `-c`
// flag in run's argv (harmless there -- it carries no secret) so an
// inherited GIT_CONFIG_* cannot reintroduce a helper. The git tracing
// knobs are explicitly forced off (GIT_CURL_VERBOSE by removing it from
// the child's environment entirely rather than setting it, since git
// only presence-checks that one -- see below) so an inherited
// GIT_TRACE/GIT_CURL_VERBOSE/GIT_TRACE_CURL cannot print the injected
// Authorization header -- and therefore the token -- to stderr, where it
// would otherwise land in a returned error or a log line; scrubSecrets
// is the second, independent layer against that same leak.
//
// GIT_CONFIG_GLOBAL is also pointed at a path inside home that never
// exists, not left inherited: git treats that env var, when set, as an
// authoritative override of the user-global config location that wins
// over HOME -- so an ambient GIT_CONFIG_GLOBAL (e.g. set process-wide on
// the host running this component) would otherwise reintroduce exactly
// the ambient-credential-helper risk HOME's redirection is meant to
// close. A path that does not exist is read by git the same way a
// missing ~/.gitconfig is: silently, as "no global config."
//
// GIT_CURL_VERBOSE is dropped from the inherited os.Environ() rather
// than overridden with "=0": unlike every other GIT_TRACE* variable
// here, git only presence-checks GIT_CURL_VERBOSE (see
// http_options() in git's http.c) rather than parsing it as a
// boolean, so "0" and "" both still count as "set" and both turn
// curl tracing ON -- the exact opposite of this function's intent.
// The only way to guarantee it is off is to make sure the key is
// absent from the child's environment altogether, which is what
// dropGitCurlVerbose does before the overrides below are appended.
//
// authHeaderValue is the base64(user:pass) half of "Authorization:
// Basic <...>", injected via GIT_CONFIG_COUNT/GIT_CONFIG_KEY_0/
// GIT_CONFIG_VALUE_0 (git >= 2.31): environment, never argv (a `-c
// http.extraHeader=...` flag would put the token in every process's
// `ps` output on the box) and never a config file (nothing here is ever
// written to disk, so the mirror's .git/config carries no trace of it
// once the subprocess exits). Empty authHeaderValue injects no header
// at all, for an anonymous operation.
func gitEnv(home, authHeaderValue string) []string {
	env := append(dropGitCurlVerbose(os.Environ()),
		"GIT_CONFIG_NOSYSTEM=1",
		// GIT_CONFIG_PARAMETERS is the OTHER ambient channel git reads
		// config from, alongside GIT_CONFIG_COUNT below -- it is how git
		// itself propagates `-c` to subprocesses, so an inherited value
		// is entirely plausible rather than exotic. Leaving it set
		// defeats the GIT_CONFIG_COUNT=0 neutralisation completely: a
		// hostile ambient
		// GIT_CONFIG_PARAMETERS="'http.extraHeader'='Authorization: ...'"
		// authenticates a deliberately-anonymous fetch, which is exactly
		// what this package's isolation test exists to prevent, and it
		// can equally force config (a proxy, say) onto an authenticated
		// one. Clearing it does not disturb the injected header, which
		// travels via GIT_CONFIG_KEY_0/VALUE_0.
		"GIT_CONFIG_PARAMETERS=",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "unused-global-gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_TRACE=0",
		"GIT_TRACE_CURL=0",
		"GIT_TRACE_PACKET=0",
		"GIT_TRACE_PACK_ACCESS=0",
		"GIT_TRACE_SETUP=0",
	)
	if authHeaderValue == "" {
		// GIT_CONFIG_COUNT=0 must be set explicitly here, not simply
		// omitted: os.Environ() above already carries whatever
		// GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n the parent
		// process happened to have set ambiently, and exec.Cmd resolves
		// duplicate env keys by last-value-wins -- so appending this
		// override here, after os.Environ(), is what actually neutralises
		// an inherited GIT_CONFIG_COUNT (including a hostile
		// http.extraHeader) on the anonymous path, exactly the way the
		// header branch below neutralises it by overwriting the same keys
		// with its own values.
		return append(env, "GIT_CONFIG_COUNT=0")
	}
	return append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+authHeaderValue,
	)
}

// dropGitCurlVerbose returns environ with any GIT_CURL_VERBOSE entry
// removed, preserving order otherwise. git presence-checks this
// variable rather than parsing it as a boolean (see gitEnv's doc
// comment), so an inherited GIT_CURL_VERBOSE=0 -- or even
// GIT_CURL_VERBOSE="" -- still counts as "set" and still turns curl
// tracing on; only an absent key is guaranteed to leave it off.
func dropGitCurlVerbose(environ []string) []string {
	filtered := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if name == "GIT_CURL_VERBOSE" {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}

// scrubSecrets returns s with every occurrence of each non-empty value
// in secrets replaced by a fixed marker, so a failing invocation's
// returned output/error can never carry the token even if git itself
// echoed it somehow. Belt and suspenders alongside gitEnv's GIT_TRACE*
// overrides and GIT_CURL_VERBOSE removal, which stop git from producing
// that trace in the first place.
func scrubSecrets(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	return s
}
