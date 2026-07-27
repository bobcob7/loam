package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// controlTimeout bounds each control-API call. The force-push runs a real
// clone/commit/push inside the fake forge, so it is not instantaneous, but
// it is also strictly local -- a call still running after this long is
// wedged, not slow.
const controlTimeout = 60 * time.Second

// runAdvance performs the single upstream event demo:m3's sync tick
// reacts to, and prints the resulting tip so the caller can assert against
// it.
//
// It is a FORCE-push, not an ordinary advance, and that is the point.
// fakeforge.ForcePushBranch with an empty ToRef resets the branch to its
// current tip's PARENT, commits the auth file there, and pushes with
// --force -- so upstream's main ends on a commit that does not contain
// the mirror's current tip anywhere in its history. A mirror that merely
// fast-forwarded would reject it; only the forced refspec
// MirrorFetcher builds (+refs/heads/*:refs/heads/*) accepts it. That makes
// the demo's later assertions a test of "upstream wins, always" rather
// than of the easy case where upstream only ever grows.
//
// Deleting prunedBranch in the same step gives the same tick a ref to
// prune, so one fetch demonstrates both halves of the mirror's contract.
func runAdvance(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("advance", flag.ContinueOnError)
	forgeURL := fs.String("forge-url", "", "base URL of the running fake forge (required)")
	gitURL := fs.String("git-url", "", "authenticated git URL of the repo, used to read back the new tip (required)")
	repo := fs.String("repo", "fixture-polyglot", "repo name inside the fake forge")
	branch := fs.String("branch", "main", "branch to rewrite")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *forgeURL == "" {
		return errors.New("-forge-url is required")
	}
	if *gitURL == "" {
		return errors.New("-git-url is required")
	}
	before, err := remoteTip(ctx, *gitURL, *branch)
	if err != nil {
		return err
	}
	if err := control(ctx, *forgeURL, "/control/force-push-branch", map[string]any{
		"repo":    *repo,
		"branch":  *branch,
		"path":    authFilePath,
		"content": authFileContent,
		"message": "rewrite " + *branch + ": add " + authSymbol + ", dropping " + legacySymbol,
	}); err != nil {
		return fmt.Errorf("force-pushing %s: %w", *branch, err)
	}
	if err := control(ctx, *forgeURL, "/control/delete-branch", map[string]any{
		"repo":   *repo,
		"branch": prunedBranch,
	}); err != nil {
		return fmt.Errorf("deleting branch %s: %w", prunedBranch, err)
	}
	after, err := remoteTip(ctx, *gitURL, *branch)
	if err != nil {
		return err
	}
	if after == before {
		return fmt.Errorf("force-push left %s at %s: upstream never moved, so nothing downstream could prove anything", *branch, before)
	}
	fmt.Printf("previous_ref=%s\n", before)
	fmt.Printf("new_ref=%s\n", after)
	fmt.Printf("pruned_branch=%s\n", prunedBranch)
	return nil
}

// control POSTs body as JSON to the fake forge's test control API. That
// surface is unauthenticated by design (internal/fakeforge/server.go
// mounts /control/* outside the token gate) -- it models the test
// harness's own privileged back door, not anything a real forge exposes.
func control(ctx context.Context, baseURL, path string, body map[string]any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding %s request: %w", path, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, strings.TrimRight(baseURL, "/")+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("building %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading %s response: %w", path, err)
	}
	// Any 2xx is success. The control endpoints answer 204 (no content)
	// rather than 200, so pinning this to StatusOK alone would reject
	// every successful call.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	return nil
}

// remoteTip resolves branch's current SHA upstream with a real
// authenticated `git ls-remote`, the same probe forge.Forgejo's own
// CheckRepo uses. Reading it from upstream rather than from the mirror is
// deliberate: the demo asserts that the INDEX matches what UPSTREAM says,
// so the expected value has to come from upstream, not from the copy
// under test.
func remoteTip(ctx context.Context, gitURL, branch string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	cmd := exec.CommandContext(callCtx, "git", "ls-remote", gitURL, "refs/heads/"+branch)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ls-remote %s %s: %w: %s", redact(gitURL), branch, err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		return "", fmt.Errorf("ls-remote %s returned no ref for %s", redact(gitURL), branch)
	}
	return fields[0], nil
}

// redact strips userinfo from a URL before it appears in an error, so a
// failure never prints the forge token into the demo's own transcript.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	u.User = nil
	return u.String()
}
