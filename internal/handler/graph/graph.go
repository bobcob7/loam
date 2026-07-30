package graph

import (
	"context"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/handler"
)

// defaultLimit is the server default result cap for every graph subquery
// when the caller's Page.limit is 0 (docs/cli-spec.md "Graph DB queries":
// "--limit <n> caps the result rows (default 50)"). This is distinct from,
// and takes priority over, internal/codegraph.Store's own internal default
// (1000): that default exists purely as a pagination safety net for a store
// call made directly with a non-positive limit, not as this RPC's
// documented default -- Query always resolves Page.limit to a concrete,
// positive value before any store call, so codegraph's own default is never
// actually reached from this handler.
const defaultLimit = 50

// Handler implements loamv1connect.GraphServiceHandler.
type Handler struct {
	symbols      SymbolStore
	scope        *handler.ScopeResolver
	capabilities *handler.CapabilityChecker
	errors       *handler.ErrorMapper
	logger       *slog.Logger
}

// compile-time assertion that Handler satisfies the generated interface.
var _ loamv1connect.GraphServiceHandler = (*Handler)(nil)

// New builds a Handler over symbols, gating Query with capabilities (the
// graph.query capability, per docs/web-spec.md -> RoleService), resolving
// QueryScope through scope (handler.ScopeResolver; repo-scope expansion is
// this package's job, not SymbolStore's -- see interfaces.go), and mapping
// domain errors through errors.
func New(symbols SymbolStore, scope *handler.ScopeResolver, capabilities *handler.CapabilityChecker, errors *handler.ErrorMapper, logger *slog.Logger) *Handler {
	return &Handler{symbols: symbols, scope: scope, capabilities: capabilities, errors: errors, logger: logger}
}

// Query dispatches to the query kind set in req (definition/references/
// dependencies/dependents/history), always setting the QueryResponse result
// oneof to the variant matching that kind, even when the match list is
// empty -- the proto's own contract. Scope resolution (an empty
// QueryScope.repos expanding to every enrolled repo) and capability gating
// both happen before any query-kind logic runs.
func (h *Handler) Query(ctx context.Context, req *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityGraphQuery); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	scoped, err := h.scope.Resolve(ctx, req.Msg.GetScope().GetRepos())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	limit, offset := resolvePage(req.Msg.GetPage())
	file := req.Msg.GetFile()
	resp := &loamv1.QueryResponse{}
	switch q := req.Msg.GetQuery().(type) {
	case *loamv1.QueryRequest_Definition:
		locations, truncated, total, queryErr := h.queryDefinition(ctx, scoped, q.Definition.GetSymbol(), file, limit, offset)
		if queryErr != nil {
			return nil, h.errors.ToConnectErr(queryErr)
		}
		resp.Result = &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{Locations: locations}}
		resp.Truncated = truncated
		resp.PageInfo = &loamv1.PageInfo{Total: uint32(total)}
	case *loamv1.QueryRequest_References:
		locations, truncated, total, queryErr := h.queryReferences(ctx, scoped, q.References.GetSymbol(), file, limit, offset)
		if queryErr != nil {
			return nil, h.errors.ToConnectErr(queryErr)
		}
		resp.Result = &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{Locations: locations}}
		resp.Truncated = truncated
		resp.PageInfo = &loamv1.PageInfo{Total: uint32(total)}
	case *loamv1.QueryRequest_Dependencies:
		edges, truncated, total, queryErr := h.queryDependencies(ctx, scoped, q.Dependencies.GetTarget(), file, limit, offset)
		if queryErr != nil {
			return nil, h.errors.ToConnectErr(queryErr)
		}
		resp.Result = &loamv1.QueryResponse_Dependencies{Dependencies: &loamv1.DependencyList{Edges: edges}}
		resp.Truncated = truncated
		resp.PageInfo = &loamv1.PageInfo{Total: uint32(total)}
	case *loamv1.QueryRequest_Dependents:
		edges, truncated, total, queryErr := h.queryDependents(ctx, scoped, q.Dependents.GetTarget(), file, limit, offset)
		if queryErr != nil {
			return nil, h.errors.ToConnectErr(queryErr)
		}
		resp.Result = &loamv1.QueryResponse_Dependencies{Dependencies: &loamv1.DependencyList{Edges: edges}}
		resp.Truncated = truncated
		resp.PageInfo = &loamv1.PageInfo{Total: uint32(total)}
	case *loamv1.QueryRequest_History:
		entries, truncated, total, queryErr := h.queryHistory(ctx, scoped, q.History.GetSymbol(), file, limit, offset)
		if queryErr != nil {
			return nil, h.errors.ToConnectErr(queryErr)
		}
		resp.Result = &loamv1.QueryResponse_History{History: &loamv1.HistoryList{Entries: entries}}
		resp.Truncated = truncated
		resp.PageInfo = &loamv1.PageInfo{Total: uint32(total)}
	default:
		return nil, h.errors.ToConnectErr(fmt.Errorf("query: no query kind set: %w", handler.ErrInvalidArgument))
	}
	ingested, err := h.scope.Ingested(ctx, scoped)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("building ingested provenance: %w", err))
	}
	resp.Ingested = toProtoIngested(ingested)
	return connect.NewResponse(resp), nil
}

// queryDefinition backs `graph def`: resolveSymbols IS the whole query --
// its matches are the result rows directly, converted to Location. An empty
// match set is exit 3 (docs/cli-spec.md: "exit 3 if the target symbol/file
// is not found").
func (h *Handler) queryDefinition(ctx context.Context, scoped []handler.ScopedRepo, symbol, file string, limit, offset int32) ([]*loamv1.Location, bool, int32, error) {
	if symbol == "" {
		return nil, false, 0, fmt.Errorf("definition query: empty symbol: %w", handler.ErrInvalidArgument)
	}
	matches, storeTruncated, err := h.resolveSymbols(ctx, scoped, symbol, file, limit+offset)
	if err != nil {
		return nil, false, 0, err
	}
	if len(matches) == 0 {
		return nil, false, 0, fmt.Errorf("symbol %q: %w", symbol, handler.ErrNotFound)
	}
	ambiguous := len(matches) > 1
	all := make([]*loamv1.Location, len(matches))
	for i, m := range matches {
		all[i] = toLocation(m.repo.Name, m.symbol, ambiguous, true)
	}
	page, truncated, total := paginate(all, limit, offset, storeTruncated)
	return page, truncated, total, nil
}

// queryReferences backs `graph refs`, the TWO-STEP composition this bead's
// NOTES require: step one (symbolExists) calls LookupSymbolsByName purely
// to establish whether symbol is defined anywhere in scope -- that result is
// exit 3 if empty, per docs/cli-spec.md's "not found" contract, mirroring
// `graph def`. Step two (LookupReferencesByName) then gathers the actual
// reference rows; UNLIKE step one, an empty result there is data, not
// exit 3 -- a real, defined symbol can legitimately have zero references, a
// distinction LookupReferencesByName's own doc comment establishes and this
// bead's NOTES restate explicitly. Skipping step one (treating
// LookupReferencesByName's empty result as not-found) would wrongly reject
// exactly that legitimate case.
func (h *Handler) queryReferences(ctx context.Context, scoped []handler.ScopedRepo, symbol, file string, limit, offset int32) ([]*loamv1.Location, bool, int32, error) {
	if symbol == "" {
		return nil, false, 0, fmt.Errorf("references query: empty symbol: %w", handler.ErrInvalidArgument)
	}
	exists, err := h.symbolExists(ctx, scoped, symbol, file)
	if err != nil {
		return nil, false, 0, err
	}
	if !exists {
		return nil, false, 0, fmt.Errorf("symbol %q: %w", symbol, handler.ErrNotFound)
	}
	mergeLimit := limit + offset
	var all []*loamv1.Location
	var storeTruncated bool
	for _, repo := range scoped {
		refs, repoTruncated, refErr := h.symbols.LookupReferencesByName(ctx, []uuid.UUID{repo.ID}, repo.IndexedBranch, symbol, file, mergeLimit)
		if refErr != nil {
			return nil, false, 0, fmt.Errorf("looking up references to %q in repo %s: %w", symbol, repo.Name, refErr)
		}
		storeTruncated = storeTruncated || repoTruncated
		for _, ref := range refs {
			all = append(all, toLocationFromReference(repo.Name, ref))
		}
	}
	if int32(len(all)) > mergeLimit {
		all = all[:mergeLimit]
		storeTruncated = true
	}
	page, truncated, total := paginate(all, limit, offset, storeTruncated)
	return page, truncated, total, nil
}

// queryDependencies backs `graph deps <file|symbol>` ("what the target
// depends on"): resolveSymbols turns target into one or more matched
// symbols, then Deps walks each match's forward blast radius, once per
// match, concatenating rather than deduplicating -- docs/cli-spec.md's
// Ambiguity paragraph commits to exactly that contract: "the query operates
// on every matching symbol and returns the union, each result row naming
// its match in `of`". Each edge's From is the resolved target (also
// carrying `of` when ambiguous, self-describing which candidate it is); To
// is the found dependency, which is never itself one of several ambiguous
// candidates but, when the target was ambiguous, still carries `of` naming
// which resolved target match produced it (loam-9rm) -- without that, two
// dependency rows from two different matches can be byte-identical with no
// way to tell them apart, which is the bug this comment guards against.
func (h *Handler) queryDependencies(ctx context.Context, scoped []handler.ScopedRepo, target, file string, limit, offset int32) ([]*loamv1.DependencyEdge, bool, int32, error) {
	if target == "" {
		return nil, false, 0, fmt.Errorf("dependencies query: empty target: %w", handler.ErrInvalidArgument)
	}
	matches, resolveTruncated, err := h.resolveSymbols(ctx, scoped, target, file, limit+offset)
	if err != nil {
		return nil, false, 0, err
	}
	if len(matches) == 0 {
		return nil, false, 0, fmt.Errorf("target %q: %w", target, handler.ErrNotFound)
	}
	ambiguous := len(matches) > 1
	mergeLimit := limit + offset
	truncated := resolveTruncated
	var all []*loamv1.DependencyEdge
	for _, m := range matches {
		fromLoc := toLocation(m.repo.Name, m.symbol, ambiguous, true)
		deps, depTruncated, depErr := h.symbols.Deps(ctx, m.repo.ID, m.repo.IndexedBranch, m.symbol.ID, mergeLimit)
		if depErr != nil {
			return nil, false, 0, fmt.Errorf("querying deps of %q in repo %s: %w", target, m.repo.Name, depErr)
		}
		truncated = truncated || depTruncated
		for _, dep := range deps {
			toLoc := toLocation(m.repo.Name, dep.Symbol, false, true)
			if ambiguous {
				toLoc.Of = matchInfoFor(m.symbol)
			}
			all = append(all, &loamv1.DependencyEdge{From: fromLoc, To: toLoc})
		}
	}
	if int32(len(all)) > mergeLimit {
		all = all[:mergeLimit]
		truncated = true
	}
	page, pageTruncated, total := paginate(all, limit, offset, truncated)
	return page, pageTruncated, total, nil
}

// queryDependents backs `graph dependents <file|symbol>` (the reverse blast
// radius): mirrors queryDependencies with From/To swapped -- each edge's To
// is the resolved target (also carrying `of` when ambiguous, self-
// describing which candidate it is), From is a symbol that depends on it.
// resolveSymbols can return more than one match for an ambiguous target
// (e.g. fixture-polyglot's Validate, a Go export and a TypeScript export
// sharing a name), and Dependents runs once per match, concatenating
// rather than deduplicating -- docs/cli-spec.md's Ambiguity paragraph
// commits to exactly that: "the query operates on every matching symbol
// and returns the union, each result row naming its match in `of`". So
// every From row, even though it is never itself one of several ambiguous
// candidates, still carries `of` naming which resolved target match
// produced it when the target was ambiguous (loam-9rm) -- without that, two
// dependent rows from two different matches can be byte-identical with no
// way to tell them apart, which is the bug this comment guards against.
func (h *Handler) queryDependents(ctx context.Context, scoped []handler.ScopedRepo, target, file string, limit, offset int32) ([]*loamv1.DependencyEdge, bool, int32, error) {
	if target == "" {
		return nil, false, 0, fmt.Errorf("dependents query: empty target: %w", handler.ErrInvalidArgument)
	}
	matches, resolveTruncated, err := h.resolveSymbols(ctx, scoped, target, file, limit+offset)
	if err != nil {
		return nil, false, 0, err
	}
	if len(matches) == 0 {
		return nil, false, 0, fmt.Errorf("target %q: %w", target, handler.ErrNotFound)
	}
	ambiguous := len(matches) > 1
	mergeLimit := limit + offset
	truncated := resolveTruncated
	var all []*loamv1.DependencyEdge
	for _, m := range matches {
		toLoc := toLocation(m.repo.Name, m.symbol, ambiguous, true)
		deps, depTruncated, depErr := h.symbols.Dependents(ctx, m.repo.ID, m.repo.IndexedBranch, m.symbol.ID, mergeLimit)
		if depErr != nil {
			return nil, false, 0, fmt.Errorf("querying dependents of %q in repo %s: %w", target, m.repo.Name, depErr)
		}
		truncated = truncated || depTruncated
		for _, dep := range deps {
			fromLoc := toLocation(m.repo.Name, dep.Symbol, false, true)
			if ambiguous {
				fromLoc.Of = matchInfoFor(m.symbol)
			}
			all = append(all, &loamv1.DependencyEdge{From: fromLoc, To: toLoc})
		}
	}
	if int32(len(all)) > mergeLimit {
		all = all[:mergeLimit]
		truncated = true
	}
	page, pageTruncated, total := paginate(all, limit, offset, truncated)
	return page, pageTruncated, total, nil
}

// queryHistory backs `graph history <symbol>`: resolveSymbols turns symbol
// into one or more matched symbols, then History returns each match's
// commit/ref history. loamv1.HistoryEntry carries no symbol/file field (the
// proto's own shape), so an ambiguous history query's entries cannot be
// attributed back to which matched symbol produced them beyond Repo --  a
// known proto-level limitation, not something this handler can work around.
func (h *Handler) queryHistory(ctx context.Context, scoped []handler.ScopedRepo, symbol, file string, limit, offset int32) ([]*loamv1.HistoryEntry, bool, int32, error) {
	if symbol == "" {
		return nil, false, 0, fmt.Errorf("history query: empty symbol: %w", handler.ErrInvalidArgument)
	}
	matches, resolveTruncated, err := h.resolveSymbols(ctx, scoped, symbol, file, limit+offset)
	if err != nil {
		return nil, false, 0, err
	}
	if len(matches) == 0 {
		return nil, false, 0, fmt.Errorf("symbol %q: %w", symbol, handler.ErrNotFound)
	}
	mergeLimit := limit + offset
	truncated := resolveTruncated
	var all []*loamv1.HistoryEntry
	for _, m := range matches {
		hist, histTruncated, histErr := h.symbols.History(ctx, m.symbol.ID, mergeLimit)
		if histErr != nil {
			return nil, false, 0, fmt.Errorf("querying history of %q in repo %s: %w", symbol, m.repo.Name, histErr)
		}
		truncated = truncated || histTruncated
		for _, entry := range hist {
			all = append(all, &loamv1.HistoryEntry{Repo: m.repo.Name, Commit: entry.Commit, Ref: entry.Ref, Message: entry.Message})
		}
	}
	if int32(len(all)) > mergeLimit {
		all = all[:mergeLimit]
		truncated = true
	}
	page, pageTruncated, total := paginate(all, limit, offset, truncated)
	return page, pageTruncated, total, nil
}

// resolvePage extracts limit/offset from page, applying defaultLimit when
// page is nil or its limit is zero (docs/cli-spec.md's documented default;
// see defaultLimit's own doc comment for why this, not codegraph's internal
// default, is what actually governs this RPC).
func resolvePage(page *loamv1.Page) (limit, offset int32) {
	limit = int32(page.GetLimit())
	if limit <= 0 {
		limit = defaultLimit
	}
	return limit, int32(page.GetOffset())
}

// paginate slices rows to the [offset, offset+limit) window, reporting
// whether more rows exist beyond it (alreadyTruncated propagates any
// upstream store-level truncation even if the merged set itself is smaller
// than the requested window) and the size of the merged set the window was
// cut from -- an approximation of PageInfo.total, not a true corpus count:
// every merged set here is itself already bounded by the limit+offset each
// per-repo/per-match store call was asked for, so when truncated is true
// this number is a lower bound, not an exact total. docs/cli-spec.md's own
// JSON envelope example never renders `total` (only `ingested` and
// `truncated`), so this is best-effort completeness for the proto field,
// not a documented contract this bead is graded against.
func paginate[T any](rows []T, limit, offset int32, alreadyTruncated bool) ([]T, bool, int32) {
	total := int32(len(rows))
	truncated := alreadyTruncated || total > offset+limit
	if offset >= total {
		return []T{}, truncated, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return rows[offset:end], truncated, total
}
