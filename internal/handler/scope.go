package handler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/reposstore"
)

// ScopedRepo is one concrete enrolled repo resolved from a loam.v1.QueryScope,
// carrying everything GraphService and SearchService need to query it: the
// id internal/codegraph and internal/chunkstore key their scoping on, the
// name responses report back to the caller, and the indexed branch the
// derived graph/vector indexes for this repo are built from
// (docs/persistence-spec.md "repos": indexed_branch).
type ScopedRepo struct {
	ID            uuid.UUID
	Name          string
	IndexedBranch string
}

// Ingested is the ingest-state provenance (docs/cli-spec.md's "ingested"
// envelope field) for one repo in scope: which branch, at which commit, and
// when it was last ingested. Ref and At are both empty when the repo's
// indexed branch has never completed an ingest (repo_target_branches.
// ingested_ref/ingested_at both NULL) -- a caller must not mistake an empty
// Ref for a real (empty) one, mirroring reposstore.IngestedRef's own Ok
// convention.
type Ingested struct {
	Repo   string
	Target string
	Ref    string
	At     string
}

// ScopeResolver expands a loam.v1.QueryScope's repo names into concrete
// enrolled repos -- GraphService and SearchService both need this before
// calling their own store seams, since internal/codegraph.Store and
// internal/chunkstore.Store both deliberately treat an EMPTY repo-id slice
// as "match nothing" (see LookupSymbolsByName's and Search's own doc
// comments), not as "all repos". QueryScope's own proto comment says the
// opposite for an empty `repos` field ("empty = all enrolled repos"); that
// contradiction is resolved in the stores' favor (a reviewed decision
// recorded on this bead), so expanding an empty scope into a concrete
// []uuid.UUID of every enrolled repo happens HERE, once, before either
// store is ever called. Passing an empty slice through to either store
// would silently turn a legitimate `--all`/no-flag request into a "no
// results" response -- exactly the bug this type exists to rule out.
type ScopeResolver struct {
	repos ScopeStore
}

// NewScopeResolver builds a ScopeResolver over repos.
func NewScopeResolver(repos ScopeStore) *ScopeResolver {
	return &ScopeResolver{repos: repos}
}

// Resolve expands names -- a loam.v1.QueryScope's repos field -- into
// concrete ScopedRepo rows. An empty names means "all enrolled repos"
// (ListAllRepoNames, then one GetRepoByName per name); a non-empty names
// resolves each entry directly, in the order given. A name that does not
// resolve to an enrolled repo wraps ErrInvalidArgument -- an unresolvable
// scope (docs/cli-spec.md's exit-2 case for both `graph` and `search`) --
// not ErrNotFound, which this package's callers reserve for a target/symbol
// absent INSIDE an already-resolved scope. An enrollment with zero repos at
// all resolves to an empty, non-error ScopedRepo slice: the caller's
// subsequent store calls correctly see an empty (never nil-vs-populated-
// ambiguous) scope and return no results, not an error.
func (r *ScopeResolver) Resolve(ctx context.Context, names []string) ([]ScopedRepo, error) {
	if len(names) == 0 {
		all, err := r.repos.ListAllRepoNames(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing enrolled repos for empty scope: %w", err)
		}
		names = all
	}
	scoped := make([]ScopedRepo, len(names))
	for i, name := range names {
		repo, err := r.repos.GetRepoByName(ctx, name)
		if err != nil {
			if errors.Is(err, reposstore.ErrNotFound) {
				return nil, fmt.Errorf("repo %q: %w", name, ErrInvalidArgument)
			}
			return nil, fmt.Errorf("resolving repo %q: %w", name, err)
		}
		scoped[i] = ScopedRepo{ID: repo.ID, Name: repo.Name, IndexedBranch: repo.IndexedBranch}
	}
	return scoped, nil
}

// Ingested returns the ingest-state provenance for every repo in scoped, one
// entry per repo (docs/cli-spec.md: "ingested": the commit each queried
// repo's index was built from) -- shared by GraphService and SearchService
// so the "ingested" envelope field is built identically by both. A repo
// whose indexed branch has no matching repo_target_branches row (should not
// happen for a genuinely enrolled repo, but handled defensively rather than
// panicking) contributes no entry rather than a zero-valued one.
func (r *ScopeResolver) Ingested(ctx context.Context, scoped []ScopedRepo) ([]Ingested, error) {
	result := make([]Ingested, 0, len(scoped))
	for _, repo := range scoped {
		branches, err := r.repos.ListTargetBranches(ctx, repo.ID)
		if err != nil {
			return nil, fmt.Errorf("listing target branches for repo %s: %w", repo.Name, err)
		}
		for _, b := range branches {
			if b.Branch != repo.IndexedBranch {
				continue
			}
			ingested := Ingested{Repo: repo.Name, Target: repo.IndexedBranch}
			if b.IngestedRef.Ok {
				ingested.Ref = b.IngestedRef.Ref
			}
			if b.IngestedAt != nil {
				ingested.At = b.IngestedAt.UTC().Format(time.RFC3339)
			}
			result = append(result, ingested)
			break
		}
	}
	return result, nil
}
