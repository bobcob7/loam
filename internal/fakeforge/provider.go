package fakeforge

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type validateTokenRequest struct {
	Host  string `json:"host"`
	Token string `json:"token"`
}

type createPRRequest struct {
	Repo         string `json:"repo"`
	HeadBranch   string `json:"head_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
}

type createPRResponse struct {
	URL    string `json:"url"`
	Number int    `json:"number"`
}

type prStateResponse struct {
	State string `json:"state"`
}

// requireProviderAuth checks the Authorization: token <token> header used
// by the provider REST surface (distinct from the git surface's Basic
// auth), writing a 401 and returning false on failure.
func (s *Server) requireProviderAuth(w http.ResponseWriter, r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if !ok || !s.hasToken(token) {
		writeJSONError(w, http.StatusUnauthorized, errUnauthorized)
		return false
	}
	return true
}

func (s *Server) handleValidateToken(w http.ResponseWriter, r *http.Request) {
	var req validateTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Host == "" || req.Token == "" || !s.hasToken(req.Token) {
		writeJSONError(w, http.StatusUnauthorized, errUnauthorized)
		return
	}
	if !s.tokenHasPRScope(req.Token) {
		writeJSONError(w, http.StatusForbidden, errMissingScope)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCreatePR(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderAuth(w, r) {
		return
	}
	var req createPRRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := os.Stat(s.repoDir(req.Repo)); err != nil {
		writeJSONError(w, http.StatusNotFound, errRepoNotFound)
		return
	}
	pr := s.prs.create(req.Repo, req.HeadBranch, req.TargetBranch, req.Title, req.Description)
	writeJSON(w, http.StatusCreated, createPRResponse{URL: fmt.Sprintf("http://%s/%s/pulls/%d", r.Host, req.Repo, pr.number), Number: pr.number})
}

func (s *Server) handleGetPRState(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderAuth(w, r) {
		return
	}
	var req prActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	pr, ok := s.prs.get(req.Repo, req.Number)
	if !ok {
		writeJSONError(w, http.StatusNotFound, errPRNotFound)
		return
	}
	writeJSON(w, http.StatusOK, prStateResponse{State: pr.state})
}

func (s *Server) handleProviderClosePR(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderAuth(w, r) {
		return
	}
	var req prActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, ok := s.prs.get(req.Repo, req.Number); !ok {
		writeJSONError(w, http.StatusNotFound, errPRNotFound)
		return
	}
	s.prs.setState(req.Repo, req.Number, "closed")
	w.WriteHeader(http.StatusNoContent)
}

// Client is a Provider-shaped REST client for one fake forge Server. Its
// method set mirrors internal/forge's real Provider interface exactly
// (ValidateToken/CheckRepo/CreatePR/GetPRState/ClosePR/GitCredentials) so
// tests can use a Client wherever code expects a Provider, without either
// package importing the other.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a Client bound to one fake forge Server at baseURL
// (e.g. an httptest.Server's URL), authenticating provider REST calls with
// token.
func NewClient(baseURL, token string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{}}
}

// ValidateToken confirms token is accepted by the forge at host. It
// validates the given token explicitly rather than the Client's own,
// mirroring CredentialService.SetUpstreamToken validating a candidate
// credential before it is stored.
func (c *Client) ValidateToken(ctx context.Context, host, token string) error {
	if err := c.call(ctx, http.MethodPost, "/provider/validate-token", validateTokenRequest{Host: host, Token: token}, nil, false); err != nil {
		return fmt.Errorf("validating token for %s: %w", host, err)
	}
	return nil
}

// CheckRepo confirms upstreamURL exists and is accessible with the
// Client's own credential, the same way a real forge's provider does it
// (docs/sync-spec.md → Upstream Transport): an authenticated ls-remote-
// style probe against the git surface for read, and a receive-pack probe
// for write, rather than a side-channel REST call. From outside, a read
// probe that fails for any reason (missing repo, bad credential) is
// indistinguishable from a repo that doesn't exist, so it is classified
// the same way; a read that succeeds but a write probe that is denied
// means the repo exists but the token lacks push access.
func (c *Client) CheckRepo(ctx context.Context, upstreamURL string) error {
	if err := c.probeInfoRefs(ctx, upstreamURL, "git-upload-pack"); err != nil {
		return fmt.Errorf("checking repo %s: %w", upstreamURL, errRepoNotFound)
	}
	if err := c.probeInfoRefs(ctx, upstreamURL, "git-receive-pack"); err != nil {
		return fmt.Errorf("checking repo %s: %w", upstreamURL, errNoWriteAccess)
	}
	return nil
}

// probeInfoRefs issues the smart-HTTP ref advertisement request for
// service against upstreamURL, authenticated with the Client's token as
// the Basic password (any username, per Forgejo's convention), returning
// an error unless the forge answers 200 OK.
func (c *Client) probeInfoRefs(ctx context.Context, upstreamURL, service string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL+"/info/refs?service="+service, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.SetBasicAuth("fakeforge-client", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probing %s: status %d", service, resp.StatusCode)
	}
	return nil
}

// CreatePR opens a pull request from headBranch into targetBranch on repo.
func (c *Client) CreatePR(ctx context.Context, repo, headBranch, targetBranch, title, description string) (string, int, error) {
	var resp createPRResponse
	req := createPRRequest{Repo: repo, HeadBranch: headBranch, TargetBranch: targetBranch, Title: title, Description: description}
	if err := c.call(ctx, http.MethodPost, "/provider/create-pr", req, &resp, true); err != nil {
		return "", 0, fmt.Errorf("creating pr for %s: %w", repo, err)
	}
	return resp.URL, resp.Number, nil
}

// GetPRState reports whether prNumber on repo is "open", "merged", or
// "closed".
func (c *Client) GetPRState(ctx context.Context, repo string, prNumber int) (string, error) {
	var resp prStateResponse
	if err := c.call(ctx, http.MethodPost, "/provider/pr-state", prActionRequest{Repo: repo, Number: prNumber}, &resp, true); err != nil {
		return "", fmt.Errorf("getting pr state for %s#%d: %w", repo, prNumber, err)
	}
	return resp.State, nil
}

// ClosePR asks the forge to close prNumber on repo without merging it.
func (c *Client) ClosePR(ctx context.Context, repo string, prNumber int) error {
	if err := c.call(ctx, http.MethodPost, "/provider/close-pr", prActionRequest{Repo: repo, Number: prNumber}, nil, true); err != nil {
		return fmt.Errorf("closing pr %s#%d: %w", repo, prNumber, err)
	}
	return nil
}

// GitCredentials returns the forge's token-authenticated HTTPS git
// convention: any username, the token as the password. This is a fixed
// convention, not a network call, matching how a real Forgejo provider
// would implement it (docs/sync-spec.md → Provider Interface).
func (c *Client) GitCredentials(_ context.Context, token string) (string, string, error) {
	return "fakeforge", token, nil
}

// call issues one JSON request/response round trip against the fake
// forge's provider REST surface, attaching the Client's token as an
// "Authorization: token <token>" header when authed is true, and
// reconstructing a sentinel error from the response's wire code on
// failure.
func (c *Client) call(ctx context.Context, method, path string, reqBody, respBody any, authed bool) error {
	body, err := marshalBody(reqBody)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authed {
		req.Header.Set("Authorization", "token "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	if respBody == nil {
		return nil
	}
	return decodeBody(resp, respBody)
}
