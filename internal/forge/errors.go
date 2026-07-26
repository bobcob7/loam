package forge

import "errors"

// ErrInvalidToken indicates the forge rejected a token outright: it does
// not authenticate at all (missing, malformed, expired, or revoked).
// Returned by ValidateToken (wrapped, on a 401 from its scope probe) and
// by CreatePR/GetPRState/ClosePR (bare, via doPullRequest, on any
// 401/403 against a real repo — those call sites cannot yet distinguish
// "wrong scope" from "no access to this particular repo," so they still
// fold both into this sentinel; see ErrInsufficientScope for the case
// ValidateToken can now tell apart). Exported: CredentialService
// (loam-ofg.15) matches it with errors.Is to report SetUpstreamToken
// rejection to the admin.
var ErrInvalidToken = errors.New("forge: token is invalid or unauthorized")

// ErrInsufficientScope indicates the token authenticates fine but does
// not carry the write:repository scope needed to open PRs. Returned
// (wrapped) by ValidateToken only, on a 403 from its scope probe —
// verified empirically against a real Forgejo 9.0.3 instance that a
// read:user-only token gets 401/403 there distinctly from an
// unauthenticated request (see loam-1ao), so this is a genuine,
// real-provider-observable case, not a fold-worthy approximation.
// Exported: CredentialService (loam-ofg.15) is expected to match it with
// errors.Is to tell an admin their token is valid but underscoped,
// instead of folding it into "invalid token."
var ErrInsufficientScope = errors.New("forge: token is missing required scope")

// ErrRepoNotFound indicates the repo does not exist, or is not visible
// with the configured credential. Returned by CheckRepo's read probe
// (wrapped) and by CreatePR/GetPRState/ClosePR (bare, via doPullRequest)
// on a 404. Exported: RepoAdminService (loam-ofg.12) matches it with
// errors.Is to reject EnrollRepo with a precise, mapped error.
//
// For the CreatePR/GetPRState/ClosePR call sites specifically, the name
// overstates what a 404 there actually proves: verified empirically
// against a real Forgejo 9.0.3 instance, GET/PATCH .../pulls/{number}
// against a PR number that does not exist in a repo that DOES exist
// returns the identical generic 404 body
// ({"message":"The target couldn't be found."}) as a request against a
// repo that itself does not exist — Forgejo's API gives doPullRequest no
// way to tell "this repo is gone" apart from "this repo is fine, but
// that PR isn't there." Getting a more specific class would require a
// second, separate request (e.g. a repo-existence probe) on every 404,
// which is disproportionate for an error path; callers of
// GetPRState/ClosePR should read ErrRepoNotFound as "the PR I asked
// about could not be resolved," not literally "the repo is missing."
// CreatePR's own 404 for a nonexistent target/base branch (verified as
// {"message":"BaseNotExist"}, distinct from a missing repo) folds into
// this same sentinel for the same reason — see doPullRequest. A
// nonexistent HEAD branch does NOT fold in here: Forgejo 9.0.3 has a
// separate, apparently unintentional bug where that case 500s with a
// leaked git error instead of 404ing (see internal/fakeforge/errors.go,
// which documents why the fake deliberately does not mimic it).
var ErrRepoNotFound = errors.New("forge: repo not found")

// ErrDuplicatePR indicates CreatePR was called for a head/target pair
// that already has an open pull request. Returned (bare) by CreatePR via
// doPullRequest on a 409 — verified empirically against a real Forgejo
// 9.0.3 instance, a second POST .../pulls for a head/base pair with an
// existing open PR returns 409 with a message embedding the existing
// PR's internal id (e.g. "pull request already exists for these
// targets [id: 1, ...]"). That id is undocumented, unstructured text —
// not a stable field to parse — so this sentinel only signals "a PR
// already exists here"; it does not recover the existing PR's number.
// A caller that needs to adopt the existing PR (loam-giq.7's
// CreatePR-succeeded-but-recording-failed retry path) must look it up
// through some other means — the Provider interface has no such
// operation yet, so this sentinel unblocks the retry from tripping
// doPullRequest's generic "unexpected status" branch (loam-hza) without
// by itself fully resolving adoption. Exported so fakeforge's errPRExists
// can wrap it and giq.7 can match it with errors.Is.
var ErrDuplicatePR = errors.New("forge: a pull request already exists for this head/target pair")

// ErrNoWriteAccess indicates the repo exists and is readable with the
// configured credential, but the write (receive-pack) probe was denied
// with a 401/403 — the token can read but not push. Returned (wrapped)
// by CheckRepo's write probe only; other non-2xx statuses and transport
// failures from that probe are deliberately unclassified (see
// forgejo_git.go). Exported: RepoAdminService (loam-ofg.12) matches it
// with errors.Is to distinguish "repo missing" from "token lacks git
// write access" at enrollment (credentials.feature: "A token without
// git access fails enrollment").
var ErrNoWriteAccess = errors.New("forge: token lacks git write access")

// AllSentinels returns every forge-level sentinel error, in no
// particular order. internal/fakeforge's regression guard
// (TestFakeforgeSentinelsMatchOnlyTheirOwnForgeClass) ranges over this
// slice instead of a hand-copied list, precisely because a hand-copied
// list is blind by construction to a sentinel added here after the copy
// was last touched — that blindness is what let errMissingScope silently
// keep wrapping ErrInvalidToken instead of the newly-added
// ErrInsufficientScope (loam-ddv). Add every new sentinel here in the
// same commit it is declared, or fakeforge's guard cannot see it either.
func AllSentinels() []error {
	return []error{ErrInvalidToken, ErrInsufficientScope, ErrRepoNotFound, ErrNoWriteAccess, ErrDuplicatePR}
}
