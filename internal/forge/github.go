package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// GitHub implements Provider against GitHub's REST API
// (docs.github.com/en/rest, API version 2022-11-28). An instance is
// bound to one host and the token currently on record for it, mirroring
// Forgejo's own binding rule (see NewForgejo's doc comment):
// CheckRepo/CreatePR/GetPRState/ClosePR/FindOpenPR use the bound
// credential, while ValidateToken and GitCredentials take their own
// host/token explicitly.
//
// # Token kind: classic personal access token, and only that
//
// GitHub has three token kinds that can authenticate git operations and
// the REST pulls API: classic PATs, fine-grained PATs, and GitHub App
// installation tokens. This type supports classic PATs only. Reasoning:
// classic PATs are the smallest kind whose scope model
// (X-OAuth-Scopes, a flat list) and git-over-HTTPS convention (any
// username, token as password -- see gitCredentialsConvention) are
// fully documented and fixed. Fine-grained PATs use a per-repository
// permission model with no equivalent scope-listing header this
// package could probe generically, and App installation tokens use a
// different git-over-HTTPS convention (x-access-token as username) and
// a different lifecycle (short-lived, minted per installation) that
// this bead's scope boundary (loam-tmds epic: "not about GitHub Apps
// as an authentication product") excludes. Supporting all three in one
// bead was explicitly called out as unreviewable in loam-tmds.2's own
// notes.
//
// # Enterprise Server: out of scope
//
// GitHub Enterprise Server's REST API lives at
// https://<host>/api/v3, not https://api.github.com. This package does
// not derive that base at all: KindForHost (resolve.go) rejects any
// host that looks like GitHub but is not exactly github.com or
// api.github.com, so a *GitHub instance is never even constructed for
// an Enterprise Server host in production. apiBaseURLForGitHub below
// therefore only has two cases to handle: the real github.com/
// api.github.com host, and a scheme-qualified test double.
type GitHub struct {
	host       string
	token      string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewGitHub constructs a Provider for GitHub, authenticated with token
// for repo and pull-request operations. httpClient must not be nil.
// host and token may be empty when the instance is only ever used for
// ValidateToken and GitCredentials, which take their own host/token
// explicitly and never read these bound fields — see NewForgejo's doc
// comment for the identical rule this mirrors.
func NewGitHub(host, token string, httpClient *http.Client, logger *slog.Logger) *GitHub {
	return &GitHub{host: host, token: token, httpClient: httpClient, logger: logger}
}

// Ensure *GitHub satisfies Provider at compile time.
var _ Provider = (*GitHub)(nil)

// githubAPIRoot is GitHub's fixed REST API root for the real service —
// unlike Forgejo, this does not vary with the web/git host, which is
// always github.com regardless of which of its two accepted aliases
// KindForHost matched.
const githubAPIRoot = "https://api.github.com"

// apiBaseURLForGitHub builds the API root for host. A scheme-qualified
// host (used by tests pointing at an httptest server, and the only
// shape internal/fakeforge's GitHub-shaped surface is ever addressed
// through) is used as-is, with no suffix appended: GitHub's real API
// paths (/repos/..., /user) hang directly off api.github.com's root,
// unlike Forgejo's /api/v1 prefix, so a test double's mux can mount
// those same paths directly. Every other host (github.com,
// api.github.com, or empty) resolves to the real githubAPIRoot.
func apiBaseURLForGitHub(host string) string {
	if strings.Contains(host, "://") {
		return strings.TrimSuffix(host, "/")
	}
	return githubAPIRoot
}

// githubOAuthScopesHeader and githubAcceptedScopesHeader are the
// response headers GitHub attaches to authenticated REST requests:
// "X-OAuth-Scopes lists the scopes your token has authorized.
// X-Accepted-OAuth-Scopes lists the scopes that the action checks for."
// (docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-
// oauth-apps, fetched 2026-08-04). Only X-OAuth-Scopes is read here:
// ValidateToken probes GET /user, which accepts ANY authenticated
// token and so reports an empty X-Accepted-OAuth-Scopes, unlike a
// scope-gated endpoint.
const githubOAuthScopesHeader = "X-OAuth-Scopes"

// githubRequiredScope is the scope ValidateToken requires: "repo" grants
// "full access to public and private repositories including read and
// write access to code" (same page as above), which is what CreatePR/
// ClosePR need against a repo of either visibility. A token carrying
// only "public_repo" is accepted by GitHub itself but is insufficient
// for a private enrolled repo, and ValidateToken has no way to know at
// validation time whether the repos this token will be used against are
// private — so, like Forgejo's write:repository probe, it requires the
// broader scope unconditionally rather than guessing.
const githubRequiredScope = "repo"

// ValidateToken confirms token authenticates against host (github.com or
// api.github.com — KindForHost's contract) and carries the "repo" scope
// CreatePR/ClosePR need, by issuing GET /user: an endpoint that accepts
// any authenticated token regardless of scope (verified against GitHub's
// own REST reference: no scope is documented as required for it), so a
// 200 response's presence alone confirms authentication, and its
// X-OAuth-Scopes header reports what the token can actually do.
//
//   - 401 means the token does not authenticate at all: ErrInvalidToken.
//   - A rate-limited 403/429 (see githubRateLimitError) is reported
//     unclassified, explicitly NOT as ErrInvalidToken — the naive
//     mapping loam-tmds.2's own notes warn against, since it would tell
//     an operator their credential is bad when it is merely throttled.
//   - 200 with "repo" present in X-OAuth-Scopes means the token
//     authenticates and has the scope this provider requires.
//   - 200 without "repo" in X-OAuth-Scopes means the token authenticates
//     but lacks it: ErrInsufficientScope.
//
// An empty token is rejected before any request is sent, matching
// Forgejo.ValidateToken's identical guard and for the identical reason:
// an unauthenticated request should never be confused with a genuine
// 401 from the forge.
func (g *GitHub) ValidateToken(ctx context.Context, host, token string) error {
	if token == "" {
		return fmt.Errorf("validating token for %s: %w", host, ErrInvalidToken)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURLForGitHub(host)+"/user", nil)
	if err != nil {
		return fmt.Errorf("validating token for %s: building request: %w", host, err)
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("validating token for %s: %w", host, err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("validating token for %s: %w", host, ErrInvalidToken)
	}
	if err := githubRateLimitError(resp); err != nil {
		return fmt.Errorf("validating token for %s: %w", host, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("validating token for %s: unexpected status %s", host, resp.Status)
	}
	if !githubScopesInclude(resp.Header.Get(githubOAuthScopesHeader), githubRequiredScope) {
		return fmt.Errorf("validating token for %s: %w", host, ErrInsufficientScope)
	}
	return nil
}

// githubScopesInclude reports whether want appears in scopesHeader, a
// comma-separated list with optional surrounding whitespace around each
// entry (X-OAuth-Scopes' documented format, e.g. "repo, gist, notifications").
func githubScopesInclude(scopesHeader, want string) bool {
	for _, scope := range strings.Split(scopesHeader, ",") {
		if strings.TrimSpace(scope) == want {
			return true
		}
	}
	return false
}

// errGitHubRateLimited is returned (wrapped, via githubRateLimitError)
// when GitHub answers a request with a rate-limit rejection rather than
// an authentication or authorization failure. It deliberately matches
// none of internal/forge's Provider-contract sentinels: rate limiting is
// not a case any of the seven interface methods' doc comments name, and
// inventing an eighth cross-provider sentinel for a GitHub-only concern
// (Forgejo does not meaningfully rate-limit — see the epic's own "what
// will bite") would be exactly the growth loam-tmds.2's scope boundary
// warns against. It exists so a test can assert what MUST NOT happen
// (mapping to ErrInvalidToken) without this package inventing a new
// public contract to do it.
var errGitHubRateLimited = errors.New("forge: github: rate limited")

// githubRateLimitError inspects resp for GitHub's two documented
// rate-limit signals and returns a wrapped errGitHubRateLimited if
// either is present, or nil otherwise. Per GitHub's REST API
// documentation (docs.github.com/en/rest/using-the-rest-api/
// troubleshooting-the-rest-api, fetched 2026-08-04): "If you exceed
// your primary rate limit, you will receive a 403 Forbidden or 429 Too
// Many Requests response, and the x-ratelimit-remaining header will be
// 0"; secondary rate limits answer the same two statuses but do NOT
// guarantee that header, only "an error message that indicates that you
// exceeded a secondary rate limit" — so a 429 is always treated as rate
// limiting (GitHub has no other documented use for that status on this
// API), and a 403 is treated as rate limiting if EITHER
// x-ratelimit-remaining is "0" OR the response mentions "rate limit" in
// its body, read once and returned to the caller so it is not lost.
func githubRateLimitError(resp *http.Response) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%w: retry-after=%s", errGitHubRateLimited, resp.Header.Get("Retry-After"))
	}
	if resp.StatusCode != http.StatusForbidden {
		return nil
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return fmt.Errorf("%w: x-ratelimit-reset=%s", errGitHubRateLimited, resp.Header.Get("X-RateLimit-Reset"))
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if strings.Contains(strings.ToLower(string(body)), "rate limit") {
		return fmt.Errorf("%w: secondary rate limit", errGitHubRateLimited)
	}
	return nil
}

// githubPullWire is the subset of GitHub's pull-request response shape
// (docs.github.com/en/rest/pulls/pulls#get-a-pull-request, fetched
// 2026-08-04) this package consumes: html_url, number, state ("open" or
// "closed" only — GitHub layers "merged" on top as a separate boolean,
// unlike Forgejo which also folds the two into one field the same way
// this package does on read, see GetPRState), merged, and the head/base
// ref names FindOpenPR filters on client-side.
type githubPullWire struct {
	HTMLURL string        `json:"html_url"`
	Number  int           `json:"number"`
	State   string        `json:"state"`
	Merged  bool          `json:"merged"`
	Head    githubRefWire `json:"head"`
	Base    githubRefWire `json:"base"`
}

// githubRefWire is the branch-name subset of GitHub's PR head/base
// object.
type githubRefWire struct {
	Ref string `json:"ref"`
}

// githubCreatePullRequest is the create-PR body CreatePR encodes. head
// is a plain branch name here, never "owner:branch": GitHub's own docs
// state that format is for a CROSS-repository PR ("username:branch"),
// and loam always pushes its proposal branch onto the upstream repo
// itself before opening a PR (docs/sync-spec.md → Upstream Transport),
// so every PR this package opens is same-repository by construction.
type githubCreatePullRequest struct {
	Head  string `json:"head"`
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// githubPatchPullRequest is the update-PR body ClosePR sends: GitHub's
// update endpoint also accepts title/body/base/maintainer_can_modify,
// none of which this package ever changes.
type githubPatchPullRequest struct {
	State string `json:"state"`
}

// githubErrorEnvelope is GitHub's standard validation-error body
// (docs.github.com/en/rest/using-the-rest-api/troubleshooting-the-rest-
// api, fetched 2026-08-04): "The response body will include an errors
// property, which includes a code property to help you diagnose the
// problem." Message is also read directly: this package's own
// duplicate-PR detection (githubIsDuplicatePR) matches on message text
// rather than a specific errors[].code, because the docs fetched for
// this bead confirm the errors[] shape and its documented codes
// (missing, missing_field, invalid, already_exists, unprocessable,
// custom) but do NOT confirm which code GitHub assigns to a duplicate
// pull request specifically — flagged here rather than guessed; see
// githubIsDuplicatePR's own doc comment.
type githubErrorEnvelope struct {
	Message string `json:"message"`
	Errors  []struct {
		Resource string `json:"resource"`
		Field    string `json:"field"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	} `json:"errors"`
}

// githubIsDuplicatePR reports whether a 422 response body represents
// CreatePR's duplicate-PR case, by matching "pull request already
// exists" (case-insensitive) against the envelope's top-level message
// or any per-error message. This is a message-text match, not a
// structured errors[].code match, and that is a deliberate, flagged gap:
// GitHub's troubleshooting docs (fetched 2026-08-04) document the
// errors[].code values already_exists, custom, invalid, missing,
// missing_field, and unprocessable in general, but do not state which
// one this specific case uses, and this package found no other GitHub
// REST reference page that pins it definitively. Matching on the
// documented, human-readable message text is deliberately the more
// conservative choice — it is exactly the string GitHub Support and
// GitHub's own CLI tooling are known to key on for this error — and any
// OTHER 422 (a genuinely invalid branch name, no commits between head
// and base, etc.) falls through to CreatePR's generic "unexpected
// status" branch rather than being misreported as a duplicate.
func githubIsDuplicatePR(envelope githubErrorEnvelope) bool {
	const marker = "pull request already exists"
	if strings.Contains(strings.ToLower(envelope.Message), marker) {
		return true
	}
	for _, e := range envelope.Errors {
		if strings.Contains(strings.ToLower(e.Message), marker) {
			return true
		}
	}
	return false
}

// CreatePR opens a pull request from headBranch into targetBranch on
// repo (a GitHub "<owner>/<repo>" path, identical shape to Forgejo's).
func (g *GitHub) CreatePR(ctx context.Context, repo, headBranch, targetBranch, title, description string) (string, int, error) {
	body, err := json.Marshal(githubCreatePullRequest{Head: headBranch, Base: targetBranch, Title: title, Body: description})
	if err != nil {
		return "", 0, fmt.Errorf("encoding create-PR body for %s: %w", repo, err)
	}
	pr, err := g.doPullRequest(ctx, http.MethodPost, repo, 0, body)
	if err != nil {
		return "", 0, fmt.Errorf("creating PR on %s: %w", repo, err)
	}
	return pr.HTMLURL, pr.Number, nil
}

// GetPRState reports prNumber's current state on repo: "open", "merged",
// or "closed". GitHub's own state field is only ever "open" or "closed"
// — merged is a separate boolean layered on top — so a closed PR that
// was merged is folded into "merged" here, the same fold
// Forgejo.GetPRState performs, and the one this method's own doc
// comment calls the most damaging to get wrong: conflating "merged"
// with "closed" would make loam treat a merged proposal as abandoned.
func (g *GitHub) GetPRState(ctx context.Context, repo string, prNumber int) (string, error) {
	pr, err := g.doPullRequest(ctx, http.MethodGet, repo, prNumber, nil)
	if err != nil {
		return "", fmt.Errorf("getting PR %s#%d state: %w", repo, prNumber, err)
	}
	if pr.State == "closed" && pr.Merged {
		return "merged", nil
	}
	return pr.State, nil
}

// ClosePR closes prNumber on repo without merging it, by PATCHing
// state=closed. GitHub's REST reference documents no distinct status
// code for closing an already-merged PR the way Forgejo's 412
// Precondition Failed does (checked against
// docs.github.com/en/rest/pulls/pulls#update-a-pull-request, fetched
// 2026-08-04: only 200/403/422 are listed, with no case described for
// this scenario) — a merged PR's state is already "closed", so a PATCH
// requesting state=closed is plausibly just an idempotent no-op there,
// not a rejection. Rather than assume a status code this bead could not
// confirm, ClosePR instead inspects the DOCUMENTED response body after
// any 2xx: if merged is true, that reflects the PR being merged either
// before or as a result of this call (merging is one-way, so the
// distinction is moot), and this method reports ErrPRAlreadyMerged
// exactly as Forgejo.ClosePR does for its own 412 case.
func (g *GitHub) ClosePR(ctx context.Context, repo string, prNumber int) error {
	body, err := json.Marshal(githubPatchPullRequest{State: "closed"})
	if err != nil {
		return fmt.Errorf("encoding close-PR body for %s#%d: %w", repo, prNumber, err)
	}
	pr, err := g.doPullRequest(ctx, http.MethodPatch, repo, prNumber, body)
	if err != nil {
		return fmt.Errorf("closing PR %s#%d: %w", repo, prNumber, err)
	}
	if pr.Merged {
		return fmt.Errorf("closing PR %s#%d: %w", repo, prNumber, ErrPRAlreadyMerged)
	}
	return nil
}

// GitCredentials returns the git-over-HTTPS convention this provider's
// chosen token kind (classic PAT) shares with Forgejo's — see
// gitCredentialsConvention's doc comment for the GitHub-docs citation
// this relies on.
func (g *GitHub) GitCredentials(ctx context.Context, token string) (string, string, error) {
	return gitCredentialsConvention(token)
}

// CheckRepo confirms upstreamURL exists and is accessible for both git
// read and git write, using the instance's bound token, by delegating
// to checkRepoOverGit — see Forgejo.CheckRepo's doc comment for why
// this is shared byte for byte rather than reimplemented: neither probe
// touches a forge's REST API, and this provider's token-kind decision
// (classic PAT) makes its git-over-HTTPS convention identical to
// Forgejo's.
func (g *GitHub) CheckRepo(ctx context.Context, upstreamURL string) error {
	return checkRepoOverGit(ctx, g.host, g.token, g.httpClient, g.logger, upstreamURL)
}

// FindOpenPR looks up the open pull request (if any) from headBranch
// into targetBranch on repo, by listing and filtering — never by
// parsing CreatePR's 422 error body, exactly as the interface requires.
// Unlike Forgejo (which, per its own doc comment, ignores head/base
// query parameters entirely and requires walking every open PR
// client-side), GitHub's list endpoint filters server-side on both:
// base as a plain branch name, and head as "<owner>:<branch>" — a
// cross-repository-shaped filter GitHub's own docs require even for a
// same-repository PR (docs.github.com/en/rest/pulls/pulls#list-pull-
// requests, fetched 2026-08-04: "Filter pulls by head user or head
// organization and branch name in the format of user:ref-name or
// organization:ref-name"). owner is split from repo ("<owner>/<name>",
// the same shape CreatePR/GetPRState/ClosePR take) to build that filter.
// The head/base match is still verified client-side against the
// returned row before reporting found=true, as defence in depth against
// any server-side filtering quirk this package has not observed.
func (g *GitHub) FindOpenPR(ctx context.Context, repo, headBranch, targetBranch string) (string, int, bool, error) {
	owner, _, ok := strings.Cut(repo, "/")
	if !ok || owner == "" {
		return "", 0, false, fmt.Errorf("finding open PR for %s %s->%s: repo must be \"<owner>/<name>\"", repo, headBranch, targetBranch)
	}
	url := fmt.Sprintf("%s/repos/%s/pulls?state=open&head=%s&base=%s",
		apiBaseURLForGitHub(g.host), repo, owner+":"+headBranch, targetBranch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, false, fmt.Errorf("finding open PR for %s %s->%s: building request: %w", repo, headBranch, targetBranch, err)
	}
	req.Header.Set("Authorization", "token "+g.token)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", 0, false, fmt.Errorf("finding open PR for %s %s->%s: %w", repo, headBranch, targetBranch, err)
	}
	defer drainAndClose(resp.Body)
	if err := githubClassifyNonOKStatus(resp); err != nil {
		return "", 0, false, fmt.Errorf("finding open PR for %s %s->%s: %w", repo, headBranch, targetBranch, err)
	}
	var prs []githubPullWire
	if err := json.NewDecoder(resp.Body).Decode(&prs); err != nil {
		return "", 0, false, fmt.Errorf("finding open PR for %s %s->%s: decoding pull request list: %w", repo, headBranch, targetBranch, err)
	}
	for _, pr := range prs {
		if pr.Head.Ref == headBranch && pr.Base.Ref == targetBranch {
			return pr.HTMLURL, pr.Number, true, nil
		}
	}
	return "", 0, false, nil
}

// doPullRequest issues a pull-request REST call against repo using the
// instance's bound host and token, mirroring Forgejo's doPullRequest.
// prNumber is ignored (and the URL targets the collection) when it is
// zero, which only happens for CreatePR.
func (g *GitHub) doPullRequest(ctx context.Context, method, repo string, prNumber int, body []byte) (*githubPullWire, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls", apiBaseURLForGitHub(g.host), repo)
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
	req.Header.Set("Authorization", "token "+g.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s %s: %w", method, url, err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode == http.StatusUnprocessableEntity {
		var envelope githubErrorEnvelope
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(bodyBytes, &envelope)
		if githubIsDuplicatePR(envelope) {
			return nil, ErrDuplicatePR
		}
		return nil, fmt.Errorf("unexpected status %s: %s", resp.Status, strconv.Quote(envelope.Message))
	}
	if err := githubClassifyNonOKStatus(resp); err != nil {
		return nil, err
	}
	var pr githubPullWire
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("decoding pull request response: %w", err)
	}
	return &pr, nil
}

// githubClassifyNonOKStatus maps resp's status to a Provider sentinel
// when it is not a success, or nil when resp.StatusCode is 2xx. Rate
// limiting is checked first and deliberately never falls through to the
// 401/403 branch below it: a rate-limited 403 must never present as
// ErrInvalidToken (loam-tmds.2's own notes). 401 and non-rate-limited
// 403 are both folded into ErrInvalidToken, mirroring
// ErrInvalidToken's own doc comment on why Forgejo's doPullRequest does
// the same: this call site cannot yet distinguish "wrong scope" from
// "no access to this particular repo."
func githubClassifyNonOKStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if err := githubRateLimitError(resp); err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrInvalidToken
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrRepoNotFound
	}
	return fmt.Errorf("unexpected status %s", resp.Status)
}
