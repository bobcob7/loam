package reposstore

import "errors"

// ErrNotFound is returned when a repo or repo_target_branches row does not
// exist. Exported (loam-ofg.11): RepoService.GetRepo is the first
// out-of-package consumer that needs to branch on "not found" -- a repo
// genuinely not enrolled must map to connect.CodeNotFound, distinguishable
// from every other failure -- so unexported-by-default is no longer the
// right call here (mirroring loam-ai4's reasoning for the same kind of
// export elsewhere: export AT THE POINT a real consumer needs to branch on
// it, not speculatively before one exists).
var ErrNotFound = errors.New("reposstore: not found")
