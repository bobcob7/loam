// Package codegraph implements the derived, rebuildable code-graph stores
// over symbols, symbol_references, graph_edges, and symbol_history
// (docs/persistence-spec.md "Code intelligence (derived, rebuildable)";
// 0002_code_intel.up.sql). Store is the only exported type: construct one
// with New over a *github.com/bobcob7/loam/internal/db/gen.Queries backed
// by either a pool or a transaction (see Store's doc comment on
// transactional scope), and wire it into the ingest orchestrator
// (dependents/deps consumers: loam-li0.6, loam-ejr, loam-ofg.10).
//
// # Cycle safety
//
// graph_edges is resolved from symbol_references purely by name, with no
// acyclicity guarantee (docs/ingestion-spec.md "Edge resolution") -- a
// mutually-recursive pair of functions, or even a single self-recursive
// one, produces a real cycle in the graph. The Dependents and Deps
// recursive CTEs (internal/db/queries/code_graph.sql) guard against this
// with Postgres's native CYCLE ... SET ... USING ... clause (SQL:1999,
// supported since Postgres 14) rather than the older hand-rolled
// "accumulate a visited-id array column, WHERE NOT id = ANY(visited)"
// idiom. Both express the same termination property -- track every id
// visited on the current recursion branch and refuse to re-expand one
// already seen -- but CYCLE is chosen here because:
//
//  1. The pinned integration-test image is internal/testdb.PostgresImage
//     ("pgvector/pgvector:pg16"), i.e. Postgres 16, comfortably past the
//     Postgres 14 floor CYCLE requires. docs/persistence-spec.md
//     "Deployment" only says Postgres runs "as its own container (a plain
//     image under testcontainers-go for tests; an operator/chart under
//     Argo CD in prod)" -- it names no image or version and leaves
//     production's Postgres version unpinned; that bullet is NOT the
//     source of this floor and should not be cited as one (a prior version
//     of this comment did). Production Postgres must be >= 14 for the
//     CYCLE clause these queries rely on to be available at all; that
//     floor is not yet written down in persistence-spec and is being
//     tracked separately, not by this bead.
//  2. CYCLE is Postgres's own implementation of that idiom (it is
//     documented as being rewritten internally into exactly the
//     hand-rolled array-and-filter form), so it is not a *different*,
//     less-proven mechanism being substituted for safety -- it is the same
//     mechanism, minus the chance to get the visited-array bookkeeping or
//     the "WHERE NOT already_visited" filter wrong by hand in every query
//     that needs it.
//  3. It keeps the recursive term's WHERE clause focused on the actual join
//     condition (docs/persistence-spec.md's graph shape), rather than
//     interleaving cycle bookkeeping into the same predicate.
//
// There is deliberately no additional depth or row cap layered on top of
// CYCLE in the Dependents/Deps queries. A cap is a legitimate
// belt-and-braces measure in general, but it is not a substitute for a
// termination guard, and combining the two invites exactly the failure
// mode this package's tests are written to catch: a small, non-branching
// cycle (e.g. a two-function mutual-recursion pair) run through a
// depth-capped-but-unguarded query terminates anyway, just because the cap
// happens to be larger than the cycle -- so a broken CYCLE clause could
// silently pass a test that only checks "did it terminate" without also
// checking "did it terminate for the right reason." This package's
// integration tests prove the CYCLE clause itself is load-bearing by
// removing it (not just weakening a cap) and observing the identical
// fixture hang under a bounded context instead of returning -- see
// integration_test.go's guard-removal subtests.
//
// # Paths versus nodes (loam-9xx)
//
// CYCLE-guarded termination is necessary but was not, on its own,
// sufficient: UNION ALL in the recursive term enumerates every simple PATH
// reaching a symbol, not every reachable symbol, and CYCLE bounds path
// LENGTH, not path COUNT. A dense DAG with zero cycles and zero duplicate
// edges -- several callers converging on one shared utility symbol, or a
// diamond -- still produces one row per parallel path, and because each
// iteration joins against the previous iteration's rows, that multiplies
// hop over hop: k parallel paths reaching a symbol at depth d become k
// parallel continuations at depth d+1, so intermediate row counts grow as
// (branching factor)^depth even though the final, deduplicated answer is
// small. Dependents/Deps fix this with a node-level SELECT DISTINCT inside
// the recursive term (internal/db/queries/code_graph.sql), collapsing every
// arrival at the same (symbol_id, depth) within one iteration to a single
// row before it feeds the next iteration -- deduplicating reached NODES
// rather than accumulating PATHS, which is what these queries mean to
// answer in the first place ("which symbols can reach this one," a set).
// This is NOT a depth cap and does not change CYCLE's role: a genuine
// cycle revisits the same symbol_id at a strictly larger depth each pass,
// so its rows are never literal (symbol_id, depth) duplicates within a
// single iteration and SELECT DISTINCT has nothing to collapse there --
// CYCLE alone still terminates it, and SELECT DISTINCT alone would not
// (TestDependentsCTE_GuardRemovedHangs proves this by removing only CYCLE
// from an otherwise-identical, DISTINCT-bearing copy of the query and
// observing the same hang). TestDependentsCTE_DiamondFixture_
// BoundedIntermediateRows proves the DISTINCT half separately: a fixture
// with a known combinatorial blast radius (no cycles at all) completes
// with a bounded intermediate row count, and reverting the recursive
// term's DISTINCT back to plain UNION ALL makes that row-count assertion
// fail.
package codegraph

import (
	"context"

	"github.com/bobcob7/loam/internal/db/gen"
)

//go:generate go tool moq -out moq_test.go . querier

// querier is the subset of *gen.Queries this package calls, defined here at
// the consumer per repo convention so Store can be unit-tested against a
// moq mock instead of a live database; *gen.Queries satisfies it in
// production without modification, whether constructed over a
// *pgxpool.Pool or a pgx.Tx (gen.New / (*gen.Queries).WithTx).
type querier interface {
	DeleteSymbolsForFile(ctx context.Context, arg gen.DeleteSymbolsForFileParams) error
	DeleteSymbolsForRepoBranch(ctx context.Context, arg gen.DeleteSymbolsForRepoBranchParams) error
	InsertSymbols(ctx context.Context, arg []gen.InsertSymbolsParams) (int64, error)
	DeleteSymbolReferencesForFile(ctx context.Context, arg gen.DeleteSymbolReferencesForFileParams) error
	DeleteSymbolReferencesForRepoBranch(ctx context.Context, arg gen.DeleteSymbolReferencesForRepoBranchParams) error
	InsertSymbolReferences(ctx context.Context, arg []gen.InsertSymbolReferencesParams) (int64, error)
	DeleteGraphEdgesForRepoBranch(ctx context.Context, arg gen.DeleteGraphEdgesForRepoBranchParams) error
	ResolveGraphEdgeCandidates(ctx context.Context, arg gen.ResolveGraphEdgeCandidatesParams) ([]gen.ResolveGraphEdgeCandidatesRow, error)
	LookupSymbolsByName(ctx context.Context, arg gen.LookupSymbolsByNameParams) ([]gen.Symbol, error)
	LookupReferencesByName(ctx context.Context, arg gen.LookupReferencesByNameParams) ([]gen.SymbolReference, error)
	InsertGraphEdges(ctx context.Context, arg []gen.InsertGraphEdgesParams) (int64, error)
	Dependents(ctx context.Context, arg gen.DependentsParams) ([]gen.DependentsRow, error)
	Deps(ctx context.Context, arg gen.DepsParams) ([]gen.DepsRow, error)
	InsertSymbolHistory(ctx context.Context, arg []gen.InsertSymbolHistoryParams) (int64, error)
	SymbolHistory(ctx context.Context, arg gen.SymbolHistoryParams) ([]gen.SymbolHistory, error)
}
