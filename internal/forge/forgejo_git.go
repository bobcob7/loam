package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckRepo confirms upstreamURL exists and is accessible for both git
// read and git write, using the instance's bound token. The read probe
// is an authenticated `git ls-remote`; the write probe is the smart-HTTP
// receive-pack advertisement request — the same first request a real
// `git push` makes, so it proves write access without transferring any
// pack data or touching a ref.
//
// upstreamURL's host must match the instance's bound host: the bound
// token belongs to one forge host, and CheckRepo must never send it to
// a different one just because a caller passed an arbitrary URL.
func (f *Forgejo) CheckRepo(ctx context.Context, upstreamURL string) error {
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return fmt.Errorf("checking repo %s: parsing upstream URL: %w", upstreamURL, err)
	}
	if boundHost := hostOf(f.host); u.Host != boundHost {
		return fmt.Errorf("checking repo %s: upstream host %q does not match the bound credential's host %q", upstreamURL, u.Host, boundHost)
	}
	if err := f.lsRemoteProbe(ctx, upstreamURL); err != nil {
		return fmt.Errorf("checking repo %s: %w", upstreamURL, err)
	}
	if err := f.receivePackProbe(ctx, u); err != nil {
		return fmt.Errorf("checking repo %s: %w", upstreamURL, err)
	}
	return nil
}

// lsRemoteProbe runs an authenticated `git ls-remote` against
// upstreamURL to confirm read access and existence. It returns an error
// wrapping ErrRepoNotFound only when the failure is plausibly "the repo
// isn't there or isn't readable" — a cancelled/deadline-exceeded context
// or a missing git binary are reported unclassified so callers don't
// mistake infrastructure trouble for a missing repo.
func (f *Forgejo) lsRemoteProbe(ctx context.Context, upstreamURL string) error {
	home, err := os.MkdirTemp("", "loam-forge-probe-*")
	if err != nil {
		return fmt.Errorf("creating isolated git environment: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	cmd := exec.CommandContext(ctx, "git", "-c", "credential.helper=", "ls-remote", upstreamURL)
	cmd.Env = f.gitAuthEnv(home)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	f.logger.DebugContext(ctx, "ls-remote probe failed", "upstream_url", upstreamURL, "err", err, "output", string(bytes.TrimSpace(out)))
	if ctx.Err() != nil {
		return fmt.Errorf("ls-remote %s: %w", upstreamURL, ctx.Err())
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("ls-remote %s: %w", upstreamURL, err)
	}
	return fmt.Errorf("ls-remote %s: %w: %s", upstreamURL, ErrRepoNotFound, bytes.TrimSpace(out))
}

// receivePackProbe issues the GET request that a `git push` makes as its
// first step (the receive-pack ref advertisement) and classifies the
// result: only an explicit auth rejection (401/403) means write access
// is denied. Any other non-2xx status or transport failure (DNS, TCP,
// TLS, rate-limiting, a 5xx) is reported unclassified — it says nothing
// about the token's git permissions. No pack data is ever sent and no
// ref is ever touched.
func (f *Forgejo) receivePackProbe(ctx context.Context, upstreamURL *url.URL) error {
	probe := *upstreamURL
	probe.Path = strings.TrimSuffix(probe.Path, "/") + "/info/refs"
	probe.RawQuery = "service=git-receive-pack"
	probe.Fragment = ""
	probeURL := probe.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return fmt.Errorf("building receive-pack probe request: %w", err)
	}
	if f.token != "" {
		req.SetBasicAuth(gitUsername, f.token)
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		f.logger.DebugContext(ctx, "receive-pack probe transport error", "url", probeURL, "err", err)
		return fmt.Errorf("receive-pack probe %s: %w", probeURL, err)
	}
	defer drainAndClose(resp.Body)
	f.logger.DebugContext(ctx, "receive-pack probe response", "url", probeURL, "status", resp.StatusCode)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("receive-pack probe %s: %w", probeURL, ErrNoWriteAccess)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("receive-pack probe %s: unexpected status %s", probeURL, resp.Status)
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
	if f.token == "" {
		return append(env, "GIT_CONFIG_COUNT=0")
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(gitUsername+":"+f.token))
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
