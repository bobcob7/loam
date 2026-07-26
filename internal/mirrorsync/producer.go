package mirrorsync

import (
	"context"
	"fmt"
)

// StoreRepoLister adapts a repoNameLister (in production,
// *reposstore.Store via its unpaginated Store.ListAllRepoNames) to
// RepoLister (loam-13z). See RepoLister's doc comment for why this
// producer is deliberately unpaginated rather than a paging adapter over
// reposstore.Store.ListRepos.
type StoreRepoLister struct {
	store repoNameLister
}

// NewStoreRepoLister builds a StoreRepoLister over store.
func NewStoreRepoLister(store repoNameLister) *StoreRepoLister {
	return &StoreRepoLister{store: store}
}

// ListRepos satisfies RepoLister: it lists every enrolled repo's name and
// converts each to a RepoID, preserving store's ordering. RepoID is
// repos.name (loam-54o.7 NOTES), so this conversion never touches
// repos.id.
func (l *StoreRepoLister) ListRepos(ctx context.Context) ([]RepoID, error) {
	names, err := l.store.ListAllRepoNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing enrolled repo names: %w", err)
	}
	repos := make([]RepoID, len(names))
	for i, name := range names {
		repos[i] = RepoID(name)
	}
	return repos, nil
}
