package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// gitUsername is the username Forgejo's git-over-HTTPS convention
// accepts alongside the token as password; Forgejo ignores it.
const gitUsername = "loam"

// probeOwner and probeRepo name a repository path chosen to never exist
// on any real Forgejo instance. ValidateToken's scope probe targets it:
// verified empirically against Forgejo 9.0.3 (gitea API 1.22), the
// scope-enforcement middleware rejects an insufficiently-scoped token
// with 401/403 before the owner or repo is ever resolved, so a request
// against this synthetic path is a safe, non-mutating way to observe
// the scope decision without touching, or even needing to know, any
// real repository.
const (
	probeOwner = "loam-scope-probe-9f3c2e71"
	probeRepo  = "does-not-exist"
)

// Forgejo implements Provider against a Forgejo instance. An instance is
// bound to one host and the token currently on record for it: CheckRepo,
// CreatePR, GetPRState, and ClosePR use that bound credential.
// ValidateToken and GitCredentials take their host/token explicitly so
// callers can validate or convert a candidate token before it is bound
// to (or replaces) an instance's own credential.
type Forgejo struct {
	host       string
	token      string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewForgejo constructs a Provider for the Forgejo instance at host,
// authenticated with token for repo and pull-request operations.
// httpClient must not be nil. host and token may be empty when the
// instance is only ever used for ValidateToken and GitCredentials,
// which take their own host/token explicitly and never read these
// bound fields.
func NewForgejo(host, token string, httpClient *http.Client, logger *slog.Logger) *Forgejo {
	return &Forgejo{host: host, token: token, httpClient: httpClient, logger: logger}
}

// Ensure *Forgejo satisfies Provider at compile time — the only such
// assertion outside test code, since no consumer package exists yet to
// catch drift.
var _ Provider = (*Forgejo)(nil)

// apiBaseURL builds the Forgejo REST API root for host. host may be a
// bare domain ("forgejo.example.com") or include a scheme (used by
// tests pointing at an httptest server).
func apiBaseURL(host string) string {
	if strings.Contains(host, "://") {
		return strings.TrimSuffix(host, "/") + "/api/v1"
	}
	return "https://" + strings.TrimSuffix(host, "/") + "/api/v1"
}

// ValidateToken confirms token authenticates against host and carries
// the write:repository scope CreatePR/ClosePR need. It does not probe
// GET /user: that endpoint only requires read:user, so a token missing
// every PR-relevant scope still returns 200 there (verified empirically
// against a real Forgejo 9.0.3 instance — see loam-1ao). Instead it
// issues the same request shape as CreatePR (POST .../pulls) against
// probeOwner/probeRepo, a path picked to never exist. Forgejo runs its
// scope check before resolving the owner or repo, so the response is
// unambiguous:
//   - 401 means the token does not authenticate at all: ErrInvalidToken.
//   - 403 means the token authenticates but lacks write:repository:
//     ErrInsufficientScope.
//   - 404 (or any 2xx, which would be a surprise) means the token
//     authenticates and has the scope; the repo not existing is
//     expected and not itself an error.
func (f *Forgejo) ValidateToken(ctx context.Context, host, token string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls", apiBaseURL(host), probeOwner, probeRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("building validate-token request for %s: %w", host, err)
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("validating token for %s: %w", host, err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("validating token for %s: %w", host, ErrInvalidToken)
	}
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("validating token for %s: %w", host, ErrInsufficientScope)
	}
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return fmt.Errorf("validating token for %s: unexpected status %s", host, resp.Status)
}

// forgejoPullRequest is the subset of the Forgejo pull-request response
// this package consumes.
type forgejoPullRequest struct {
	HTMLURL string `json:"html_url"`
	Number  int    `json:"number"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
}

// CreatePR opens a pull request from headBranch into targetBranch on
// repo (a Forgejo "<owner>/<repo>" path).
func (f *Forgejo) CreatePR(ctx context.Context, repo, headBranch, targetBranch, title, description string) (string, int, error) {
	body, err := json.Marshal(map[string]string{"head": headBranch, "base": targetBranch, "title": title, "body": description})
	if err != nil {
		return "", 0, fmt.Errorf("encoding create-PR body for %s: %w", repo, err)
	}
	pr, err := f.doPullRequest(ctx, http.MethodPost, repo, 0, body)
	if err != nil {
		return "", 0, fmt.Errorf("creating PR on %s: %w", repo, err)
	}
	return pr.HTMLURL, pr.Number, nil
}

// GetPRState reports prNumber's current state on repo: "open",
// "merged", or "closed".
func (f *Forgejo) GetPRState(ctx context.Context, repo string, prNumber int) (string, error) {
	pr, err := f.doPullRequest(ctx, http.MethodGet, repo, prNumber, nil)
	if err != nil {
		return "", fmt.Errorf("getting PR %s#%d state: %w", repo, prNumber, err)
	}
	if pr.State == "closed" && pr.Merged {
		return "merged", nil
	}
	return pr.State, nil
}

// ClosePR closes prNumber on repo without merging it.
func (f *Forgejo) ClosePR(ctx context.Context, repo string, prNumber int) error {
	body, err := json.Marshal(map[string]string{"state": "closed"})
	if err != nil {
		return fmt.Errorf("encoding close-PR body for %s#%d: %w", repo, prNumber, err)
	}
	if _, err := f.doPullRequest(ctx, http.MethodPatch, repo, prNumber, body); err != nil {
		return fmt.Errorf("closing PR %s#%d: %w", repo, prNumber, err)
	}
	return nil
}

// GitCredentials returns Forgejo's git-over-HTTPS convention: the token
// as the password, with any username.
func (f *Forgejo) GitCredentials(ctx context.Context, token string) (string, string, error) {
	if token == "" {
		return "", "", fmt.Errorf("git credentials: %w", ErrInvalidToken)
	}
	return gitUsername, token, nil
}

// doPullRequest issues a pull-request REST call against repo using the
// instance's bound host and token. prNumber is ignored (and the URL
// targets the collection) when it is zero, which only happens for
// create.
func (f *Forgejo) doPullRequest(ctx context.Context, method, repo string, prNumber int, body []byte) (*forgejoPullRequest, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls", apiBaseURL(f.host), repo)
	if prNumber != 0 {
		url = fmt.Sprintf("%s/%d", url, prNumber)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("building %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "token "+f.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s %s: %w", method, url, err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrRepoNotFound
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrInvalidToken
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	var pr forgejoPullRequest
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("decoding pull request response: %w", err)
	}
	return &pr, nil
}
