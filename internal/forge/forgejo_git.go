package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bobcob7/loam/internal/urlredact"
)

// CheckRepo confirms upstreamURL exists and is accessible for both git
// read and git write, using the instance's bound token, by delegating
// to checkRepoOverGit -- the forge-agnostic git-protocol implementation
// GitHub.CheckRepo (github.go) shares byte for byte: neither the
// ls-remote read probe nor the receive-pack write probe touches a
// forge's REST API at all, and GitHub's classic-PAT git-over-HTTPS
// convention coincides with Forgejo's (gitCredentialsConvention), so
// there is nothing left that differs per Kind here.
func (f *Forgejo) CheckRepo(ctx context.Context, upstreamURL string) error {
	return checkRepoOverGit(ctx, f.host, f.token, f.httpClient, f.logger, upstreamURL)
}

// checkRepoOverGit is CheckRepo's shared implementation: an
// authenticated `git ls-remote` for read, and the smart-HTTP
// receive-pack advertisement request for write -- the same first
// request a real `git push` makes, so it proves write access without
// transferring any pack data or touching a ref.
//
// upstreamURL's host must match boundHost: the token belongs to one
// forge host, and this must never send it to a different one just
// because a caller passed an arbitrary URL.
func checkRepoOverGit(ctx context.Context, boundHost, token string, httpClient *http.Client, logger *slog.Logger, upstreamURL string) error {
	u, err := url.Parse(upstreamURL)
	if err != nil {
		// upstreamURL is deliberately never interpolated into this
		// error, and err is deliberately not %w-wrapped: *url.Error's
		// Error() renders as `parse "<raw url>": <reason>`, so wrapping
		// it would leak exactly what internal/urlredact exists to
		// prevent (loam-po8e, mirroring
		// internal/handler/repoadmin/probe.go's identical rule for the
		// same reason).
		return errors.New("checking repo: parsing upstream URL: invalid URL")
	}
	redacted := urlredact.URL(u)
	if bound := hostOf(boundHost); u.Host != bound {
		return fmt.Errorf("checking repo %s: upstream host %q does not match the bound credential's host %q", redacted, u.Host, bound)
	}
	if err := lsRemoteProbeOverGit(ctx, upstreamURL, token, logger); err != nil {
		return fmt.Errorf("checking repo %s: %w", redacted, err)
	}
	if err := receivePackProbeOverGit(ctx, u, token, httpClient, logger); err != nil {
		return fmt.Errorf("checking repo %s: %w", redacted, err)
	}
	return nil
}

// lsRemoteProbe runs an authenticated `git ls-remote` against
// upstreamURL to confirm read access and existence, delegating to
// lsRemoteProbeOverGit with the instance's bound token and logger.
func (f *Forgejo) lsRemoteProbe(ctx context.Context, upstreamURL string) error {
	return lsRemoteProbeOverGit(ctx, upstreamURL, f.token, f.logger)
}

// lsRemoteProbeOverGit is lsRemoteProbe's shared implementation. It
// returns an error wrapping ErrRepoNotFound only when the failure is
// plausibly "the repo isn't there or isn't readable" — a
// cancelled/deadline-exceeded context or a missing git binary are
// reported unclassified so callers don't mistake infrastructure trouble
// for a missing repo.
func lsRemoteProbeOverGit(ctx context.Context, upstreamURL, token string, logger *slog.Logger) error {
	u, parseErr := url.Parse(upstreamURL)
	if parseErr != nil {
		// Same rule, for the same reason, as checkRepoOverGit's own parse
		// branch above: neither upstreamURL nor parseErr may be rendered,
		// because *url.Error's Error() quotes the raw string whole. Failing
		// here rather than handing an unparseable string to git also means
		// the credential-scrubbing below always has a complete list of the
		// secrets it must remove -- a URL that did not parse is one whose
		// embedded credential cannot be identified, and git's own output is
		// then unsafe to echo at all (loam-9h1e).
		return errors.New("ls-remote: parsing upstream URL: invalid URL")
	}
	redacted, secrets := urlredact.URL(u), urlredact.Secrets(u)
	home, err := os.MkdirTemp("", "loam-forge-probe-*")
	if err != nil {
		return fmt.Errorf("creating isolated git environment: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	cmd := exec.CommandContext(ctx, "git", "-c", "credential.helper=", "ls-remote", upstreamURL)
	cmd.Env = gitAuthEnvFor(home, token)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// git's own output is scrubbed before it reaches either the log line or
	// the returned error. git strips userinfo from MOST of its messages
	// ("unable to access", "repository ... not found", "Authentication
	// failed"), which is what an earlier reading of this function
	// generalized from -- but not from the one it emits when it needs to
	// PROMPT: against a 401 challenge with a username and no password in the
	// URL, it prints `could not read Password for 'https://<token>@host'`
	// verbatim, because the username is exactly what it is reporting it
	// already has. A 401 is what a wrong, expired, or revoked bound token
	// produces, so that is the LIKELIEST failure this probe sees, not an
	// exotic one (loam-9h1e).
	output := urlredact.Scrub(string(bytes.TrimSpace(out)), secrets...)
	logger.DebugContext(ctx, "ls-remote probe failed", "upstream_url", redacted, "err", urlredact.TransportError(err, secrets), "output", output)
	if ctx.Err() != nil {
		return fmt.Errorf("ls-remote %s: %w", redacted, ctx.Err())
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("ls-remote %s: %w", redacted, err)
	}
	return fmt.Errorf("ls-remote %s: %w: %s", redacted, ErrRepoNotFound, output)
}

// receivePackProbe issues the GET request that a `git push` makes as its
// first step, delegating to receivePackProbeOverGit with the instance's
// bound token, HTTP client, and logger.
func (f *Forgejo) receivePackProbe(ctx context.Context, upstreamURL *url.URL) error {
	return receivePackProbeOverGit(ctx, upstreamURL, f.token, f.httpClient, f.logger)
}

// receivePackProbeOverGit is receivePackProbe's shared implementation:
// the receive-pack ref advertisement request, classified so that only
// an explicit auth rejection (401/403) means write access is denied.
// Any other non-2xx status or transport failure (DNS, TCP, TLS,
// rate-limiting, a 5xx) is reported unclassified — it says nothing
// about the token's git permissions. No pack data is ever sent and no
// ref is ever touched.
func receivePackProbeOverGit(ctx context.Context, upstreamURL *url.URL, token string, httpClient *http.Client, logger *slog.Logger) error {
	probe := *upstreamURL
	probe.Path = strings.TrimSuffix(probe.Path, "/") + "/info/refs"
	probe.RawQuery = "service=git-receive-pack"
	probe.Fragment = ""
	probeURL := probe.String()
	redacted := urlredact.URL(&probe)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return fmt.Errorf("building receive-pack probe request: %w", urlredact.TransportError(err, urlredact.Secrets(&probe)))
	}
	if token != "" {
		req.SetBasicAuth(gitUsername, token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Both the log field and the %w wrap take the REDACTED error, never
		// err itself: http.Client.Do returns *url.Error, which renders the
		// whole request URL through net/http's stripPassword -- and that
		// masks the password component only, so the token in a
		// "https://<token>@host" upstream URL travels through it untouched
		// into the log stream (which ships to Loki) and into any error chain
		// a handler renders upstream. Wrapping is not the lesser half of that
		// pair; an RPC error message discloses a credential exactly as well
		// as a log line does (loam-9h1e).
		safe := urlredact.TransportError(err, urlredact.Secrets(&probe))
		logger.DebugContext(ctx, "receive-pack probe transport error", "url", redacted, "err", safe)
		return fmt.Errorf("receive-pack probe %s: %w", redacted, safe)
	}
	defer drainAndClose(resp.Body)
	logger.DebugContext(ctx, "receive-pack probe response", "url", redacted, "status", resp.StatusCode)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("receive-pack probe %s: %w", redacted, ErrNoWriteAccess)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("receive-pack probe %s: unexpected status %s", redacted, resp.Status)
	}
	return nil
}

// gitAuthEnv returns the full, isolated environment for one lsRemoteProbe
// git invocation: the bound token injected as a Basic-auth header via
// GIT_CONFIG_COUNT/GIT_CONFIG_KEY_0/GIT_CONFIG_VALUE_0 (git ≥2.31), per
// sync-spec.md's askpass-style requirement — the credential is never
// written to argv (visible via `ps`), never to any git config file, and
// never to disk — plus isolation from whatever the host machine has
// configured, ported from internal/gittransport's gitEnv (same defect
// class documented there: an ambient credential.helper or ~/.netrc
// silently rescuing a request that was supposed to be validated against
// only the token under test; here it would mean CheckRepo reports
// success using the *operator's* stored credentials rather than the
// bound token).
//
// GIT_CONFIG_NOSYSTEM drops the system gitconfig; HOME/XDG_CONFIG_HOME
// are redirected at home (a fresh, per-invocation temp directory the
// caller removes when the subprocess returns) so no user-global config
// is read either; GIT_CONFIG_GLOBAL is pointed at a path inside home
// that never exists, since git treats that env var, when set, as an
// authoritative override that wins over HOME — an ambient
// GIT_CONFIG_GLOBAL would otherwise reintroduce the same risk HOME's
// redirection closes. credential.helper is cleared via `-c
// credential.helper=` in lsRemoteProbe's argv (harmless there — it
// carries no secret) so an inherited GIT_CONFIG_* cannot reintroduce a
// helper.
//
// GIT_CONFIG_COUNT is always set explicitly — 0 when there is no token
// to inject, never simply omitted — because os.Environ() may already
// carry an ambient GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n
// (including a hostile http.extraHeader), and exec.Cmd resolves
// duplicate env keys by last-value-wins, so appending this override
// after os.Environ() is what actually neutralises it on the anonymous
// path. GIT_CONFIG_PARAMETERS is the other ambient channel git reads
// config from (how git itself propagates `-c` to subprocesses); leaving
// it set would defeat GIT_CONFIG_COUNT=0 by a different door, so it is
// cleared unconditionally.
//
// GIT_CURL_VERBOSE is dropped from the inherited os.Environ() rather
// than overridden with "=0": git only presence-checks that one variable
// (see gittransport's gitEnv doc comment), so "0" and "" both still
// count as "set" and turn curl tracing on — the only way to guarantee
// it is off is to remove the key entirely, which dropGitCurlVerbose
// does before the overrides below are appended. The other GIT_TRACE*
// variables are ordinary booleans and are safe to override with "0".
func (f *Forgejo) gitAuthEnv(home string) []string {
	return gitAuthEnvFor(home, f.token)
}

// gitAuthEnvFor is gitAuthEnv's shared implementation, parameterized on
// token so lsRemoteProbeOverGit (and, through it, GitHub.CheckRepo) can
// build the identical isolated environment for whichever Kind's bound
// token is in play — the isolation properties documented above apply
// uniformly regardless of which forge the token belongs to.
func gitAuthEnvFor(home, token string) []string {
	env := append(dropGitCurlVerbose(os.Environ()),
		"GIT_CONFIG_NOSYSTEM=1",
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
	if token == "" {
		return append(env, "GIT_CONFIG_COUNT=0")
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(gitUsername+":"+token))
	return append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0="+header,
	)
}

// dropGitCurlVerbose returns environ with any GIT_CURL_VERBOSE entry
// removed, preserving order otherwise — ported from
// internal/gittransport's dropGitCurlVerbose (loam-bot5): git
// presence-checks this variable rather than parsing it as a boolean, so
// an inherited GIT_CURL_VERBOSE=0 — or even GIT_CURL_VERBOSE="" — still
// counts as "set" and still turns curl tracing on; only an absent key is
// guaranteed to leave it off.
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

// drainAndClose discards any remaining response body before closing it,
// so the underlying connection can be reused.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

// hostOf returns the host:port authority of hostOrURL. hostOrURL may be
// a bare domain ("forgejo.example.com", returned as-is) or a full URL
// with a scheme (used by tests pointing at an httptest server, whose
// Host component is extracted).
func hostOf(hostOrURL string) string {
	if !strings.Contains(hostOrURL, "://") {
		return hostOrURL
	}
	u, err := url.Parse(hostOrURL)
	if err != nil {
		return hostOrURL
	}
	return u.Host
}
