package fakeforge

import "errors"

// Sentinel errors returned by the fake forge, both from direct Go method
// calls (control API) and reconstructed by Client from REST error codes.
var (
	errUnauthorized    = errors.New("fakeforge: unauthorized")
	errRepoNotFound    = errors.New("fakeforge: repo not found")
	errRepoExists      = errors.New("fakeforge: repo already exists")
	errBranchNotFound  = errors.New("fakeforge: branch not found")
	errPRNotFound      = errors.New("fakeforge: pull request not found")
	errInvalidBranch   = errors.New("fakeforge: invalid branch name")
	errMergeConflict   = errors.New("fakeforge: merge conflict")
	errGitUnavailable  = errors.New("fakeforge: git binary not available")
	errInvalidUpstream = errors.New("fakeforge: invalid upstream url")
	errNoWriteAccess   = errors.New("fakeforge: token has no write access")
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
	{errInvalidBranch, "invalid_branch"},
	{errMergeConflict, "merge_conflict"},
	{errGitUnavailable, "git_unavailable"},
	{errInvalidUpstream, "invalid_upstream"},
	{errNoWriteAccess, "no_write_access"},
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
