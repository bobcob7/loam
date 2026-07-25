package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
)

// CheckRepo confirms upstreamURL exists and is accessible for both git
// read and git write, using the instance's bound token. The read probe
// is an authenticated `git ls-remote`; the write probe is the smart-HTTP
// receive-pack advertisement request — the same first request a real
// `git push` makes, so it proves write access without transferring any
// pack data or touching a ref.
func (f *Forgejo) CheckRepo(ctx context.Context, upstreamURL string) error {
	if err := f.lsRemoteProbe(ctx, upstreamURL); err != nil {
		return fmt.Errorf("checking repo %s: %w: %w", upstreamURL, ErrRepoNotFound, err)
	}
	if err := f.receivePackProbe(ctx, upstreamURL); err != nil {
		return fmt.Errorf("checking repo %s: %w: %w", upstreamURL, ErrNoWriteAccess, err)
	}
	return nil
}

// lsRemoteProbe runs an authenticated `git ls-remote` against
// upstreamURL to confirm read access and existence.
func (f *Forgejo) lsRemoteProbe(ctx context.Context, upstreamURL string) error {
	args := append(f.gitAuthArgs(), "ls-remote", upstreamURL)
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ls-remote %s: %w: %s", upstreamURL, err, bytes.TrimSpace(out))
	}
	return nil
}

// receivePackProbe issues the GET request that a `git push` makes as its
// first step (the receive-pack ref advertisement) and treats a
// non-success status as write access being denied. No pack data is ever
// sent and no ref is ever touched.
func (f *Forgejo) receivePackProbe(ctx context.Context, upstreamURL string) error {
	probeURL := strings.TrimSuffix(upstreamURL, "/") + "/info/refs?service=git-receive-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return fmt.Errorf("building receive-pack probe request: %w", err)
	}
	if f.token != "" {
		req.SetBasicAuth(gitUsername, f.token)
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("receive-pack probe %s: %w", probeURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("receive-pack probe %s: status %s", probeURL, resp.Status)
	}
	return nil
}

// gitAuthArgs returns the `git -c http.extraHeader=...` argument pair
// that injects the bound token as a Basic-auth header per invocation,
// never writing it to any git config file. Returns nil when there is no
// token to inject (e.g. anonymous file:// fixtures in tests).
func (f *Forgejo) gitAuthArgs() []string {
	if f.token == "" {
		return nil
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(gitUsername+":"+f.token))
	return []string{"-c", "http.extraHeader=" + header}
}
