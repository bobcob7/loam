package credentialstore

import "errors"

// ErrNotFound means host has no credentials row at all -- distinguishable
// from a transport failure so a caller can tell "never enrolled" apart
// from "the database is unreachable".
//
// Exported, matching reposstore.ErrNotFound, because that distinction is
// only useful to a caller in ANOTHER package: internal/handler/credential's
// GetCredentialStatus must answer features/credentials.feature's
// "Credentials are scoped per host" ("it shows no credential is present")
// with a zero-valued CredentialStatus rather than an error, while still
// failing loudly when the database itself is down. An unexported sentinel
// cannot cross that package boundary, and the only alternative -- matching
// on the error's message text in the handler -- is exactly the fragile
// coupling a sentinel exists to prevent. Every method here that can report
// absence wraps this with fmt.Errorf/%w, so callers match with errors.Is,
// never by comparison.
var ErrNotFound = errors.New("credentials: host not found")

// ErrNoToken means host has a credentials row but token_ciphertext is
// null -- a row can exist with no token yet (docs/persistence-spec.md
// "credentials": "token_ciphertext (bytea, null)"), and GetByHost has
// nothing to decrypt in that case. Distinct from ErrNotFound so a caller
// can tell "no row at all" apart from "row exists, no token set".
//
// Exported for the same cross-package reason as ErrNotFound, and kept
// distinct from it deliberately: collapsing the two would make a host
// whose row exists-but-is-tokenless indistinguishable from an unknown
// host, and the two call for different operator action ("re-run
// SetUpstreamToken for a host you already know about" vs "you have never
// configured this host"). Returned by GetByHost only -- GetStatus and
// ListStatuses report the same condition as HasToken == false, which is
// data, not an error.
var ErrNoToken = errors.New("credentials: host has no token set")
