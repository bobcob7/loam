package fakeforge

import (
	"errors"
	"fmt"

	"github.com/bobcob7/loam/internal/forge"
)

// Sentinel errors returned by the fake forge, both from direct Go method
// calls (control API) and reconstructed by Client from REST error codes.
//
// errUnauthorized, errMissingScope, errRepoNotFound, errTargetBranchNotFound,
// errPRNotFound, errPRExists, and errNoWriteAccess wrap the corresponding
// internal/forge sentinel, so a caller holding a fakeforge.Client wherever
// a forge.Provider is expected can use the same errors.Is(err, forge.ErrX)
// assertion against the fake and the real Forgejo provider (loam-li0.9's
// shared contract suite; see loam-4k7). The other sentinels have no
// forge-level equivalent: they are raised only by the control API and the
// git smart-HTTP surface, neither of which is part of the forge.Provider
// contract.
//
// errMissingScope wraps ErrInsufficientScope, matching Forgejo.ValidateToken
// (internal/forge/forgejo.go), which distinguishes a 403 ("authenticates,
// wrong scope") from a 401 ("does not authenticate at all") — verified
// empirically against a real Forgejo 9.0.3 instance (loam-1ao). It used to
// wrap ErrInvalidToken, on the correct-at-the-time premise that
// ValidateToken folded both cases together; loam-ddv found that premise
// stale (forge grew ErrInsufficientScope) and the fake had silently
// re-diverged. See TestFakeforgeSentinelsMatchOnlyTheirOwnForgeClass in
// errors_test.go for how the regression guard now makes that class of
// drift structurally harder to miss.
//
// errPRNotFound and errTargetBranchNotFound both wrap ErrRepoNotFound, not
// because a PR or a branch is a repo, but because that is genuinely what
// real Forgejo 9.0.3 returns for both: doPullRequest maps every 404 from
// the pulls endpoints to ErrRepoNotFound, and a nonexistent PR number
// against an existing repo, and a nonexistent target/base branch on
// CreatePR, both 404 there indistinguishably from the repo itself missing
// (verified empirically; see ErrRepoNotFound's godoc in
// internal/forge/errors.go). errBranchNotFound (the HEAD-branch and
// control-API case) deliberately does NOT wrap it: a nonexistent HEAD
// branch on CreatePR is Forgejo's one genuine wire-level divergence in this
// area — a leaked-git-error 500, not a 404 — so mapping it to
// ErrRepoNotFound would make the fake claim a parity that does not exist
// (loam-9qu). See CreatePR's branch-validation call sites in provider.go.
//
// errPRExists wraps ErrDuplicatePR: real Forgejo 9.0.3 returns 409 for a
// repeat CreatePR against an already-open head/target pair (verified
// empirically), and doPullRequest now classifies that status explicitly
// instead of folding it into "unexpected status" (loam-hza; the fold was
// accurate when this comment last said otherwise, before ErrDuplicatePR
// existed).
//
// errPRMerged now wraps ErrPRAlreadyMerged, the forge-level equivalent
// loam-giq.8 added: Forgejo's 412 from closing an already-merged PR is
// classified in doPullRequest now, instead of falling through its generic
// "unexpected status" branch. statusForErr (control.go) already mapped
// this sentinel to 412, so the fake's wire behavior is unchanged by that
// bead — only the class a Client reconstructs from it on the way back.
var (
	errUnauthorized         = fmt.Errorf("fakeforge: unauthorized: %w", forge.ErrInvalidToken)
	errRepoNotFound         = fmt.Errorf("fakeforge: repo not found: %w", forge.ErrRepoNotFound)
	errRepoExists           = errors.New("fakeforge: repo already exists")
	errBranchNotFound       = errors.New("fakeforge: branch not found")
	errTargetBranchNotFound = fmt.Errorf("fakeforge: target branch not found: %w", forge.ErrRepoNotFound)
	errPRNotFound           = fmt.Errorf("fakeforge: pull request not found: %w", forge.ErrRepoNotFound)
	errPRExists             = fmt.Errorf("fakeforge: an open pull request already exists for this head/target pair: %w", forge.ErrDuplicatePR)
	errPRMerged             = fmt.Errorf("fakeforge: pull request is already merged: %w", forge.ErrPRAlreadyMerged)
	errInvalidBranch        = errors.New("fakeforge: invalid branch name")
	errMergeConflict        = errors.New("fakeforge: merge conflict")
	errGitUnavailable       = errors.New("fakeforge: git binary not available")
	errInvalidUpstream      = errors.New("fakeforge: invalid upstream url")
	errNoWriteAccess        = fmt.Errorf("fakeforge: token has no write access: %w", forge.ErrNoWriteAccess)
	errMissingScope         = fmt.Errorf("fakeforge: token missing required scope: %w", forge.ErrInsufficientScope)
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
	{errTargetBranchNotFound, "target_branch_not_found"},
	{errPRNotFound, "pr_not_found"},
	{errPRExists, "pr_exists"},
	{errPRMerged, "pr_merged"},
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
