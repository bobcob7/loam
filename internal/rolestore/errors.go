package rolestore

import "errors"

// errNotFound is returned when no role by the requested name exists.
// Unexported per repo convention: no out-of-package caller needs to branch
// on it today. A trusted Loam-Agent-Role header naming an unrecognized
// role is an operator/configuration anomaly (docs/httpauth: "no signature
// or credential backs these values"), not a condition RepoService or
// MetaService's callers act on differently from any other store failure --
// both let it fall through to ErrorMapper's unmapped-error branch
// (CodeInternal, logged) rather than misreport it as a normal not-found or
// a permission denial. Export it (mirroring loam-ai4's reasoning) if a
// future caller genuinely needs to distinguish it.
var errNotFound = errors.New("rolestore: not found")
