// Package refpolicy implements docs/git-spec.md "Ref Policy (push)": the
// three rules a pre-receive hook must check against Postgres before a push
// may land -- the ref names a registered work branch, the caller is that
// branch's author, and the branch is in a non-terminal state -- as one Go
// function, EvaluatePush, evaluated atomically for a whole push (one bad
// ref rejects everything).
//
// This package is deliberately transport-free: it has no socket, no
// process, no stdin parsing. loam-ofg.18's own instructions call this out
// explicitly ("a table-driven test of the Go evaluation function, NOT the
// socket transport"), and internal/hooksocket (the unix-socket server that
// wraps EvaluatePush for a real pre-receive hook to call) and cmd/loamhook
// (the hook process itself) both depend on this package, never the other
// way around.
package refpolicy

import (
	"context"

	"github.com/bobcob7/loam/internal/workbranchstore"
)

//go:generate go tool moq -out moq_test.go . WorkBranchPolicyStore

// WorkBranchPolicyStore is the read seam EvaluatePush consumes to resolve
// rule 1's "registered work branch" question and, once resolved, the row
// rules 2 (author) and 3 (non-terminal state) check. It intentionally
// takes repoName + branchName rather than a repo ID: EvaluatePush's own
// caller (a pre-receive hook, ultimately) never has a repo ID either, only
// LOAM_REPO's trusted repo-name string (internal/handler/git's serveRPC
// doc comment: LOAM_REPO comes from the store's own repo.Name, not
// attacker-influenced request input) and the pushed ref's branch name.
// Production wiring (cmd/server) composes reposstore.Store (name -> repo
// id) and workbranchstore.Store.GetByName (repo id + name -> WorkBranch)
// behind a single adapter satisfying this interface; tests drive a moq
// mock instead.
//
// GetWorkBranch returns workbranchstore.ErrNotFound (re-exported by that
// package, checked here via errors.Is, never redefined) when repoName has
// no work branch named branchName -- EvaluatePush's rule-1 "unknown ref" /
// "read-only ref" classification depends on being able to tell that case
// apart from every other error, which fails the whole push closed instead
// (see EvaluatePush's own doc comment).
type WorkBranchPolicyStore interface {
	GetWorkBranch(ctx context.Context, repoName, branchName string) (workbranchstore.WorkBranch, error)
}
