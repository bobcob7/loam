// Package forge abstracts the upstream git forge (Forgejo, GitHub, ...):
// the REST calls needed to validate credentials, probe repo access, and
// manage pull requests, plus the forge's git-over-HTTPS credential
// convention. Everything else — URL parsing to <group>/<repo_name>, git
// fetch/push to the upstream, and branch naming — lives in the core,
// outside this package (docs/sync-spec.md → Provider Interface).
package forge

import "context"

// Provider is the forge-specific surface consumed by the credential,
// proposal, and repo-admin services. Forgejo is the only MVP
// implementation (GitHub is Future Work). Secrets are fetched from the
// credential store by callers and passed in; the provider never reaches
// into the credential store itself.
type Provider interface {
	// ValidateToken confirms token authenticates against host and has
	// the REST scopes needed to open PRs. Returns an error wrapping
	// ErrInvalidToken if the token does not authenticate at all (empty,
	// malformed, expired, or revoked), or ErrInsufficientScope if it
	// authenticates but lacks the required scope.
	ValidateToken(ctx context.Context, host, token string) error
	// CheckRepo confirms upstreamURL exists and is accessible for both
	// git read and git write, via an authenticated `git ls-remote` and
	// a dry-run receive-pack probe, before the clone starts. Returns an
	// error wrapping ErrRepoNotFound if the repo does not exist (or is
	// not readable), or ErrNoWriteAccess if it can be read but not
	// pushed to.
	CheckRepo(ctx context.Context, upstreamURL string) error
	// CreatePR opens a pull request from headBranch into targetBranch
	// on repo, with the given title and description, returning its URL
	// and number.
	CreatePR(ctx context.Context, repo, headBranch, targetBranch, title, description string) (prURL string, prNumber int, err error)
	// GetPRState reports the pull request's current state: "open",
	// "merged", or "closed".
	GetPRState(ctx context.Context, repo string, prNumber int) (state string, err error)
	// ClosePR closes an open pull request without merging it. Callers
	// treat failures as best-effort (docs/sync-spec.md → PR State
	// Tracking). Returns an error wrapping ErrPRAlreadyMerged when the
	// PR has already merged and therefore cannot be closed — a
	// success-equivalent outcome (the PR is already terminal), not a
	// failure to retry; see that sentinel's godoc.
	ClosePR(ctx context.Context, repo string, prNumber int) error
	// GitCredentials returns the forge's username/password convention
	// for authenticating git-over-HTTPS with token (e.g. Forgejo takes
	// the token as the password with any username).
	GitCredentials(ctx context.Context, token string) (username, password string, err error)
	// FindOpenPR looks up the open pull request (if any) from headBranch
	// into targetBranch on repo, by listing and filtering — never by
	// parsing CreatePR's ErrDuplicatePR message, whose embedded "id" is
	// the PR's internal id, not the per-repo number this method returns.
	// found is false, with prURL/prNumber zero and err nil, when no such
	// PR is open; err is non-nil only when the lookup itself failed
	// (wrapping ErrRepoNotFound or ErrInvalidToken on the same terms as
	// CreatePR/GetPRState/ClosePR). Callers use this to adopt the PR a
	// 409 from CreatePR reported as already existing.
	FindOpenPR(ctx context.Context, repo, headBranch, targetBranch string) (prURL string, prNumber int, found bool, err error)
}

//go:generate go tool moq -out moq_test.go . Provider
