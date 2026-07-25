package forge

import "errors"

// ErrInvalidToken indicates the forge rejected a token as invalid,
// expired, or lacking the required scopes. Returned by ValidateToken
// (wrapped) and by CreatePR/GetPRState/ClosePR (bare, via doPullRequest)
// on a 401/403. Exported: CredentialService (loam-ofg.15) matches it
// with errors.Is to report SetUpstreamToken rejection to the admin.
var ErrInvalidToken = errors.New("forge: token is invalid or unauthorized")

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
