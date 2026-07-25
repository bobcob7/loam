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
	cmd := exec.CommandContext(ctx, "git", "ls-remote", upstreamURL)
	cmd.Env = append(append([]string{}, os.Environ()...), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, f.gitAuthEnv()...)
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

// gitAuthEnv returns the GIT_CONFIG_* environment variables that inject
// the bound token as a Basic-auth header for a single git invocation
// (git ≥2.31), per sync-spec.md's askpass-style requirement: the
// credential is never written to argv (visible via `ps`), never to any
// git config file, and never to disk. Returns nil when there is no
// token to inject (e.g. anonymous file:// fixtures in tests).
func (f *Forgejo) gitAuthEnv() []string {
	if f.token == "" {
		return nil
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(gitUsername+":"+f.token))
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=" + header}
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
