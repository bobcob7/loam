// Package graph implements loam.v1.GraphService: Query, the single RPC
// backing `loam graph def/refs/deps/dependents/history` (docs/cli-spec.md
// "Graph DB queries"). QueryRequest is a oneof over the five query kinds
// sharing one QueryScope + Page; QueryResponse's result oneof is always set
// to the variant matching the requested kind, even when the match list is
// empty, per the proto's own contract.
//
// `graph refs` needs a two-step composition that the other four kinds do
// not (see Handler.queryReferences's doc comment): internal/codegraph.Store.
// LookupSymbolsByName's empty result is the ONLY authoritative not-found
// signal this package has (backing exit 3); LookupReferencesByName's empty
// result is not, because a real, defined symbol can legitimately have zero
// references. def/deps/dependents/history all resolve a name to symbol
// row(s) via LookupSymbolsByName first regardless (the same id-resolution
// step Dependents/Deps/History already require), so only refs's SECOND
// step -- LookupReferencesByName -- is the one query in this package whose
// empty result must never be read as not-found.
package graph

import (
	"context"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/codegraph"
)

//go:generate go tool moq -out moq_test.go . SymbolStore

// SymbolStore is the internal/codegraph.Store surface Query needs, defined
// here at the consumer per repo convention. *codegraph.Store satisfies it
// structurally in production; tests drive a moq mock instead of a live
// database.
type SymbolStore interface {
	// LookupSymbolsByName resolves name to every matching symbols row in
	// scope -- the definition lookup backing `graph def`, and the name ->
	// id resolution step `graph refs/deps/dependents/history` all need
	// before they can call the id-based methods below. An empty result is
	// this package's only not-found signal; see codegraph.Store's own doc
	// comment.
	LookupSymbolsByName(ctx context.Context, repoIDs []uuid.UUID, targetBranch, name, file string, limit int32) ([]codegraph.Symbol, bool, error)
	// LookupReferencesByName resolves name to every matching
	// symbol_references row in scope -- backing `graph refs`'s second
	// step. An empty result here is NOT a not-found signal by itself; see
	// codegraph.Store's own doc comment and this package's doc comment.
	LookupReferencesByName(ctx context.Context, repoIDs []uuid.UUID, targetBranch, name, file string, limit int32) ([]codegraph.Reference, bool, error)
	// Dependents returns symbolID's reverse blast radius within one repo.
	Dependents(ctx context.Context, repoID uuid.UUID, targetBranch string, symbolID uuid.UUID, limit int32) ([]codegraph.Dependency, bool, error)
	// Deps returns symbolID's forward blast radius within one repo.
	Deps(ctx context.Context, repoID uuid.UUID, targetBranch string, symbolID uuid.UUID, limit int32) ([]codegraph.Dependency, bool, error)
	// History returns symbolID's commit/ref history.
	History(ctx context.Context, symbolID uuid.UUID, limit int32) ([]codegraph.HistoryEntry, bool, error)
}
