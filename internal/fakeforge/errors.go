package fakeforge

import (
	"errors"
	"fmt"

	"github.com/bobcob7/loam/internal/forge"
)

// Sentinel errors returned by the fake forge, both from direct Go method
// calls (control API) and reconstructed by Client from REST error codes.
//
// errUnauthorized, errMissingScope, errRepoNotFound, and errNoWriteAccess
// wrap the corresponding internal/forge sentinel, so a caller holding a
// fakeforge.Client wherever a forge.Provider is expected can use the same
// errors.Is(err, forge.ErrX) assertion against the fake and the real
// Forgejo provider (loam-li0.9's shared contract suite; see loam-4k7).
// The other sentinels have no forge-level equivalent: they are raised
// only by the control API and the git smart-HTTP surface, neither of
// which is part of the forge.Provider contract.
//
// errMissingScope wraps ErrInvalidToken, not a scope-specific sentinel:
// forge.Provider's ValidateToken contract folds "does not authenticate"
// and "authenticates but lacks the scopes needed to open PRs" into one
// error path (see forge/interfaces.go and Forgejo.ValidateToken, which
// maps both 401 and 403 to ErrInvalidToken). There is no fourth
// forge-level class for "valid token, wrong scope" to map onto.
//
// errPRExists deliberately does not wrap a forge sentinel either, but for
// the opposite reason from errPRNotFound (see loam-hy4, which tracks that
// gap separately and is not fixed here): Forgejo.doPullRequest
// (internal/forge/forgejo.go) only classifies 404/401/403 into sentinels;
// a 409 from a duplicate PR falls through to its generic "unexpected
// status" branch today, so there is no forge-level class yet to map onto.
// If forge later grows one, this should be revisited alongside loam-hy4.
var (
	errUnauthorized    = fmt.Errorf("fakeforge: unauthorized: %w", forge.ErrInvalidToken)
	errRepoNotFound    = fmt.Errorf("fakeforge: repo not found: %w", forge.ErrRepoNotFound)
	errRepoExists      = errors.New("fakeforge: repo already exists")
	errBranchNotFound  = errors.New("fakeforge: branch not found")
	errPRNotFound      = errors.New("fakeforge: pull request not found")
	errPRExists        = errors.New("fakeforge: an open pull request already exists for this head/target pair")
	errInvalidBranch   = errors.New("fakeforge: invalid branch name")
	errMergeConflict   = errors.New("fakeforge: merge conflict")
	errGitUnavailable  = errors.New("fakeforge: git binary not available")
	errInvalidUpstream = errors.New("fakeforge: invalid upstream url")
	errNoWriteAccess   = fmt.Errorf("fakeforge: token has no write access: %w", forge.ErrNoWriteAccess)
	errMissingScope    = fmt.Errorf("fakeforge: token missing required scope: %w", forge.ErrInvalidToken)
)

// errorCodes maps sentinel errors to the stable string codes carried over
// the wire in errorEnvelope.Code, and back again on the Client side.
var errorCodes = []struct {
	err  error
	code string
}{
	{errUnauthorized, "unauthorized"},
	{errRepoNotFound, "repo_not_found"},
	{errRepoExists, "repo_exists"},
	{errBranchNotFound, "branch_not_found"},
	{errPRNotFound, "pr_not_found"},
	{errPRExists, "pr_exists"},
	{errInvalidBranch, "invalid_branch"},
	{errMergeConflict, "merge_conflict"},
	{errGitUnavailable, "git_unavailable"},
	{errInvalidUpstream, "invalid_upstream"},
	{errNoWriteAccess, "no_write_access"},
	{errMissingScope, "missing_scope"},
}

// codeForError returns the wire code for a known sentinel, or "" if err
// does not match one.
func codeForError(err error) string {
	for _, entry := range errorCodes {
		if errors.Is(err, entry.err) {
			return entry.code
		}
	}
	return ""
}

// errorForCode reconstructs a sentinel error from a wire code, or nil if the
// code is unrecognized.
func errorForCode(code string) error {
	for _, entry := range errorCodes {
		if entry.code == code {
			return entry.err
		}
	}
	return nil
}
