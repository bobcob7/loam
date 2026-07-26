package reposstore

import "errors"

// errNotFound is returned when a repo or repo_target_branches row does not
// exist. Unexported per repo convention; callers match it with errors.Is
// against the wrapped error a Store method returns.
var errNotFound = errors.New("reposstore: not found")
