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
	// ErrInvalidToken if the forge rejects the token.
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
	// Tracking).
	ClosePR(ctx context.Context, repo string, prNumber int) error
	// GitCredentials returns the forge's username/password convention
	// for authenticating git-over-HTTPS with token (e.g. Forgejo takes
	// the token as the password with any username).
	GitCredentials(ctx context.Context, token string) (username, password string, err error)
}

//go:generate go tool moq -out moq_test.go . Provider
