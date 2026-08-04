// Package forge abstracts the upstream git forge (Forgejo, GitHub, ...):
// the REST calls needed to validate credentials, probe repo access, and
// manage pull requests, plus the forge's git-over-HTTPS credential
// convention. Everything else — URL parsing to <group>/<repo_name>, git
// fetch/push to the upstream, and branch naming — lives in the core,
// outside this package (docs/sync-spec.md → Provider Interface).
package forge

import "context"

// Provider is the forge-specific surface consumed by the credential,
// proposal, and repo-admin services. Two implementations exist: Forgejo
// (forgejo.go) and GitHub (github.go, classic personal-access tokens
// only — see that file's own doc comment for the token-kind and
// Enterprise Server scope decisions). NewProvider (resolve.go) resolves
// a host to the right one; no caller outside this package should ever
// need to know which implementation it is holding. See
// docs/sync-spec.md → Provider Interface for what a third
// implementation would have to supply, and for the concrete differences
// between these two that cost real time to discover. Secrets are
// fetched from the credential store by callers and passed in; the
// provider never reaches into the credential store itself.
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
	// for authenticating git-over-HTTPS with token. Forgejo takes the
	// token as the password with any username; GitHub's classic
	// personal-access tokens (the only kind this package supports —
	// github.go) share that exact convention, verified against GitHub's
	// own docs: "the username is not used to authenticate you." A
	// GitHub App installation token would instead need
	// "x-access-token" as the username, but that token kind is out of
	// scope here.
	GitCredentials(ctx context.Context, token string) (username, password string, err error)
	// FindOpenPR looks up the open pull request (if any) from headBranch
	// into targetBranch on repo, by listing and filtering — never by
	// parsing CreatePR's ErrDuplicatePR message. Forgejo's message
	// embeds an internal id, not the per-repo number this method
	// returns; GitHub's equivalent rejection carries no number at all.
	// found is false, with prURL/prNumber zero and err nil, when no such
	// PR is open; err is non-nil only when the lookup itself failed
	// (wrapping ErrRepoNotFound or ErrInvalidToken on the same terms as
	// CreatePR/GetPRState/ClosePR). Callers use this to adopt the PR a
	// duplicate-PR rejection from CreatePR reported as already existing.
	FindOpenPR(ctx context.Context, repo, headBranch, targetBranch string) (prURL string, prNumber int, found bool, err error)
}

//go:generate go tool moq -out moq_test.go . Provider
