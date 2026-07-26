package reposstore

import "errors"

// errNotFound is returned when a repo or repo_target_branches row does not
// exist. Unexported per repo convention: only this package's own tests can
// match it with errors.Is today, since no out-of-package caller can name
// an unexported identifier. Exporting it is deferred until a consumer
// actually needs to branch on "not found" (mirroring loam-ai4's reasoning
// for the same call elsewhere) rather than exported speculatively now.
var errNotFound = errors.New("reposstore: not found")
