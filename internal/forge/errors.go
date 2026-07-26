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
var ErrRepoNotFound = errors.New("forge: repo not found")

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
