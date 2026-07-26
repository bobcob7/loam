// Package repo implements loam.v1.RepoService: GetRepo, the metadata
// lookup `loam clone` runs pre-flight to decide whether a repo is
// enrolled before it ever touches the git smart-HTTP endpoint
// (docs/cli-spec.md -> clone; internal/cli/clone.go).
package repo

import (
	"context"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/reposstore"
)

//go:generate go tool moq -out moq_test.go . RepoStore

// RepoStore is the internal/reposstore.Store surface GetRepo needs,
// defined here at the consumer per repo convention. *reposstore.Store
// satisfies this directly (structurally) in production; tests drive a moq
// mock instead. GetRepoByName resolves the enrolled repo (or a wrapped
// reposstore.ErrNotFound); ListTargetBranches resolves the branches
// eligible as work-branch targets the response reports.
type RepoStore interface {
	GetRepoByName(ctx context.Context, name string) (reposstore.Repo, error)
	ListTargetBranches(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error)
}
