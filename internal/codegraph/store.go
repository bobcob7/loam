package codegraph

import (
	"context"
	"fmt"
	"log/slog"
	"math"

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// defaultLimit bounds the number of rows Dependents/Deps/History return
// when the caller passes a non-positive limit, so a careless call can never
// issue an effectively unbounded query. This is a pagination default, NOT a
// cycle-safety measure -- the CYCLE clause in Dependents/Deps
// (internal/db/queries/code_graph.sql) is what makes those queries
// terminate at all; this constant only caps how many of the (already
// finite) results come back over the wire.
const defaultLimit = 1000

// SymbolInput is one freshly parsed symbol awaiting insertion. Line is nil
// for a file-level symbol (docs/persistence-spec.md "symbols": "line (null
// for file-level)").
type SymbolInput struct {
	Line *int32
	Name string
	Kind string
}

// Symbol is a persisted symbols row.
type Symbol struct {
	ID           uuid.UUID
	RepoID       uuid.UUID
	TargetBranch string
	File         string
	Line         *int32
	Name         string
	Kind         string
}

// ReferenceInput is one freshly parsed, unresolved reference awaiting
// insertion.
type ReferenceInput struct {
	Name string
	Kind string
	Line int32
}

// Dependency is one entry in a Dependents/Deps transitive result: the
// reached symbol, plus the depth (hop count) at which it was first
// reached. Depth is informational only -- it plays no part in cycle
// termination, which the CYCLE clause enforces independently of it.
type Dependency struct {
	Symbol Symbol
	Depth  int32
}

// HistoryEntryInput is one symbol_history row awaiting insertion, derived
// from git (docs/ingestion-spec.md "Symbol history").
type HistoryEntryInput struct {
	SymbolID uuid.UUID
	Commit   string
	Ref      string
	Message  string
}

// HistoryEntry is a persisted symbol_history row.
type HistoryEntry struct {
	ID       uuid.UUID
	SymbolID uuid.UUID
	Commit   string
	Ref      string
	Message  string
}

// Store implements the code-graph stores over symbols, symbol_references,
// graph_edges, and symbol_history. Every method that mutates more than one
// row expects the caller to have already decided the transactional scope:
// Store itself opens no transaction and commits nothing -- construct it
// over gen.New(tx) for callers that need e.g. ReplaceFileSymbols and a
// subsequent RecomputeGraphEdges to land atomically (docs/ingestion-spec.md
// "Consistency & Failure": "Each ingest is one transaction"), or over
// gen.New(pool) for standalone reads/writes.
type Store struct {
	q      querier
	logger *slog.Logger
}

// New builds a Store backed by q, typically a *gen.Queries constructed over
// either a *pgxpool.Pool (standalone reads/writes) or a pgx.Tx (atomic with
// other stores' writes in the same transaction) -- see NewInTx for the
// latter as a named entry point matching this package's siblings.
func New(q querier, logger *slog.Logger) *Store {
	return &Store{q: q, logger: logger}
}

// NewInTx builds a Store bound to tx, an already-open transaction the
// caller owns and will commit or roll back itself: it is exactly New(gen.
// New(tx), logger), given a name so callers composing several stores' writes
// into one commit -- the atomic swap loam-c94.12 orchestrates -- have one
// consistent constructor to reach for across every store package. Store
// never calls tx.Begin/Commit/Rollback itself (see Store's doc comment), so
// there is no nested-transaction path to guard against here.
func NewInTx(tx pgx.Tx, logger *slog.Logger) *Store {
	return New(gen.New(tx), logger)
}

// ReplaceFileSymbols performs one file's delete-and-replace
// (docs/ingestion-spec.md "Incremental Build"): every existing symbols row
// for (repoID, targetBranch, file) is dropped, then symbols is
// bulk-inserted with a fresh uuid.NewV7 id per row, in that order. It
// returns the inserted rows (with their assigned ids) so a caller can
// immediately reference them, e.g. to attach symbol_history. Passing an
// empty symbols slice deletes the file's existing symbols and inserts
// nothing -- the correct behavior for a file that no longer declares any
// symbols (docs/ingestion-spec.md "Deleted / renamed-away files").
func (s *Store) ReplaceFileSymbols(ctx context.Context, repoID uuid.UUID, targetBranch, file string, symbols []SymbolInput) ([]Symbol, error) {
	if err := s.q.DeleteSymbolsForFile(ctx, gen.DeleteSymbolsForFileParams{
		RepoID:       pgUUID(repoID),
		TargetBranch: targetBranch,
		File:         file,
	}); err != nil {
		return nil, fmt.Errorf("deleting symbols for %s@%s:%s: %w", repoID, targetBranch, file, err)
	}
	if len(symbols) == 0 {
		return nil, nil
	}
	inserted := make([]Symbol, len(symbols))
	params := make([]gen.InsertSymbolsParams, len(symbols))
	for i, sym := range symbols {
		id := uuid.Must(uuid.NewV7())
		params[i] = gen.InsertSymbolsParams{
			ID:           pgUUID(id),
			RepoID:       pgUUID(repoID),
			TargetBranch: targetBranch,
			File:         file,
			Line:         pgInt4(sym.Line),
			Name:         sym.Name,
			Kind:         sym.Kind,
		}
		inserted[i] = Symbol{ID: id, RepoID: repoID, TargetBranch: targetBranch, File: file, Line: sym.Line, Name: sym.Name, Kind: sym.Kind}
	}
	if _, err := s.q.InsertSymbols(ctx, params); err != nil {
		return nil, fmt.Errorf("inserting symbols for %s@%s:%s: %w", repoID, targetBranch, file, err)
	}
	s.logger.DebugContext(ctx, "replaced file symbols", "repo_id", repoID, "target_branch", targetBranch, "file", file, "count", len(inserted))
	return inserted, nil
}

// ReplaceFileReferences performs one file's delete-and-replace for
// symbol_references, mirroring ReplaceFileSymbols. It returns the number of
// references inserted.
func (s *Store) ReplaceFileReferences(ctx context.Context, repoID uuid.UUID, targetBranch, file string, refs []ReferenceInput) (int64, error) {
	if err := s.q.DeleteSymbolReferencesForFile(ctx, gen.DeleteSymbolReferencesForFileParams{
		RepoID:       pgUUID(repoID),
		TargetBranch: targetBranch,
		File:         file,
	}); err != nil {
		return 0, fmt.Errorf("deleting symbol references for %s@%s:%s: %w", repoID, targetBranch, file, err)
	}
	if len(refs) == 0 {
		return 0, nil
	}
	params := make([]gen.InsertSymbolReferencesParams, len(refs))
	for i, ref := range refs {
		params[i] = gen.InsertSymbolReferencesParams{
			ID:           pgUUID(uuid.Must(uuid.NewV7())),
			RepoID:       pgUUID(repoID),
			TargetBranch: targetBranch,
			File:         file,
			Name:         ref.Name,
			Kind:         ref.Kind,
			Line:         ref.Line,
		}
	}
	count, err := s.q.InsertSymbolReferences(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("inserting symbol references for %s@%s:%s: %w", repoID, targetBranch, file, err)
	}
	s.logger.DebugContext(ctx, "replaced file references", "repo_id", repoID, "target_branch", targetBranch, "file", file, "count", count)
	return count, nil
}

// RecomputeGraphEdges rebuilds graph_edges for (repoID, targetBranch) from
// scratch (docs/persistence-spec.md "graph_edges": "Recomputed each
// ingest"): every existing edge for the repo/branch is deleted, then
// candidates are resolved by name from the current symbols/symbol_references
// rows and bulk-inserted with a fresh uuid.NewV7 id per edge. It returns
// the number of edges inserted. Callers that need this atomic with the
// symbol/reference replacement that precedes it must construct Store over
// a transaction-scoped querier (see Store's doc comment).
func (s *Store) RecomputeGraphEdges(ctx context.Context, repoID uuid.UUID, targetBranch string) (int64, error) {
	if err := s.q.DeleteGraphEdgesForRepoBranch(ctx, gen.DeleteGraphEdgesForRepoBranchParams{
		RepoID:       pgUUID(repoID),
		TargetBranch: targetBranch,
	}); err != nil {
		return 0, fmt.Errorf("deleting graph edges for %s@%s: %w", repoID, targetBranch, err)
	}
	candidates, err := s.q.ResolveGraphEdgeCandidates(ctx, gen.ResolveGraphEdgeCandidatesParams{
		RepoID:       pgUUID(repoID),
		TargetBranch: targetBranch,
	})
	if err != nil {
		return 0, fmt.Errorf("resolving graph edge candidates for %s@%s: %w", repoID, targetBranch, err)
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	params := make([]gen.InsertGraphEdgesParams, len(candidates))
	for i, c := range candidates {
		params[i] = gen.InsertGraphEdgesParams{
			ID:           pgUUID(uuid.Must(uuid.NewV7())),
			RepoID:       pgUUID(repoID),
			TargetBranch: targetBranch,
			FromSymbolID: c.FromSymbolID,
			ToSymbolID:   c.ToSymbolID,
			Kind:         "dependency",
		}
	}
	count, err := s.q.InsertGraphEdges(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("inserting graph edges for %s@%s: %w", repoID, targetBranch, err)
	}
	s.logger.DebugContext(ctx, "recomputed graph edges", "repo_id", repoID, "target_branch", targetBranch, "count", count)
	return count, nil
}

// LookupSymbolsByName resolves name to every matching symbols row, scoped
// to repoIDs (plural because `graph def --all` fans out and unions across
// enrolled repos -- see internal/db/queries/code_graph.sql's
// LookupSymbolsByName comment for why that differs from
// Dependents/Deps/History's single-repoID shape without contradicting it)
// and targetBranch, optionally narrowed to one file (empty file means no
// narrowing). Matching is exact-name; ambiguity -- several distinct symbols
// sharing name, e.g. three Logins in three files -- is deliberately not an
// error: docs/cli-spec.md:528-533 requires every match returned as data, so
// a handler can build the per-row `of` disambiguation field itself.
//
// An empty result is the ONLY authoritative not-found signal this package
// can offer a handler (docs/cli-spec.md exit 3, "Not found"): it means no
// symbol named name exists in scope at all. That is genuinely different
// from a real match whose Dependents/Deps/History comes back empty (a
// symbol that exists but happens to have no edges/history) -- a
// distinction the store previously could not make, since Dependents/Deps
// took a uuid.UUID the caller had no way to obtain from a name and simply
// returned (nil, nil, nil) for "no rows" regardless of which case that was.
// A caller resolving `graph def/deps/dependents/history <name>` calls this
// first: zero rows is exit 3, one row is the unambiguous case, and more
// than one is the ambiguous-target case docs/cli-spec.md defines as data.
//
// truncated follows the same limit+1/clampLimit/fetchLimit contract as
// Dependents/Deps/History (limit <= 0 uses defaultLimit): docs/cli-spec.md
// :535-537 requires a capped `graph` response to set truncated: true
// regardless of which subquery it backs, not only the blast-radius ones,
// so a many-way name collision capped by limit must report it exactly like
// a capped blast radius does.
//
// An empty repoIDs matches nothing, mirroring
// internal/chunkstore.Search's treatment of an empty scope as "search
// nothing", not "no filter" -- a caller that forgot to populate its scope
// gets zero results rather than every repo's symbols. Unlike Search,
// though, a non-positive limit here still means "use defaultLimit" (this
// package's own convention), not Search's separate "non-positive limit
// means zero results" rule -- the two packages scope multi-repo lookups
// the same way but do not share every convention, since Search's caller
// picks a limit for pagination page size while this package's callers
// (Dependents/Deps/History call sites) already rely on clampLimit's
// default-when-absent behavior.
func (s *Store) LookupSymbolsByName(ctx context.Context, repoIDs []uuid.UUID, targetBranch, name, file string, limit int32) (symbols []Symbol, truncated bool, err error) {
	if len(repoIDs) == 0 {
		return nil, false, nil
	}
	effectiveLimit := clampLimit(limit)
	ids := make([]pgtype.UUID, len(repoIDs))
	for i, id := range repoIDs {
		ids[i] = pgUUID(id)
	}
	rows, err := s.q.LookupSymbolsByName(ctx, gen.LookupSymbolsByNameParams{
		Column1:      ids,
		TargetBranch: targetBranch,
		Name:         name,
		Column4:      file,
		Limit:        fetchLimit(effectiveLimit),
	})
	if err != nil {
		return nil, false, fmt.Errorf("looking up symbols named %q: %w", name, err)
	}
	all := make([]Symbol, len(rows))
	for i, r := range rows {
		all[i] = fromGenSymbol(r)
	}
	symbols, truncated = trimSymbols(all, effectiveLimit)
	return symbols, truncated, nil
}

// Dependents returns the reverse blast radius of symbolID: every symbol
// that transitively depends on it, deduplicated (by minimum depth),
// nearest-depth-first, up to limit rows (limit <= 0 uses defaultLimit).
// truncated reports whether more than limit rows actually exist -- a
// caller that received exactly limit rows but truncated=false has the
// complete transitive set, never confusing "exactly limit" with "there
// were more" (docs/cli-spec.md's `truncated` envelope field depends on
// this distinction, and clampLimit's own defaultLimit substitution is not
// exempt: a blast radius that silently exceeds 1000 must still report
// truncated=true). See the package doc comment for the cycle-safety
// guarantee this relies on.
func (s *Store) Dependents(ctx context.Context, repoID uuid.UUID, targetBranch string, symbolID uuid.UUID, limit int32) (deps []Dependency, truncated bool, err error) {
	effectiveLimit := clampLimit(limit)
	rows, err := s.q.Dependents(ctx, gen.DependentsParams{
		RepoID:       pgUUID(repoID),
		TargetBranch: targetBranch,
		ToSymbolID:   pgUUID(symbolID),
		Limit:        fetchLimit(effectiveLimit),
	})
	if err != nil {
		return nil, false, fmt.Errorf("querying dependents of %s: %w", symbolID, err)
	}
	deps, truncated = trimDependentsRows(rows, effectiveLimit)
	return deps, truncated, nil
}

// Deps returns the forward blast radius of symbolID: every symbol it
// transitively depends on, deduplicated (by minimum depth), nearest-depth-
// first, up to limit rows (limit <= 0 uses defaultLimit). truncated
// reports whether more than limit rows actually exist -- see Dependents'
// doc comment for the full rationale, which applies identically here. See
// the package doc comment for the cycle-safety guarantee this relies on.
func (s *Store) Deps(ctx context.Context, repoID uuid.UUID, targetBranch string, symbolID uuid.UUID, limit int32) (deps []Dependency, truncated bool, err error) {
	effectiveLimit := clampLimit(limit)
	rows, err := s.q.Deps(ctx, gen.DepsParams{
		RepoID:       pgUUID(repoID),
		TargetBranch: targetBranch,
		FromSymbolID: pgUUID(symbolID),
		Limit:        fetchLimit(effectiveLimit),
	})
	if err != nil {
		return nil, false, fmt.Errorf("querying deps of %s: %w", symbolID, err)
	}
	all := make([]Dependency, len(rows))
	for i, r := range rows {
		all[i] = Dependency{
			Symbol: Symbol{
				ID:           uuidFromPg(r.ID),
				RepoID:       uuidFromPg(r.RepoID),
				TargetBranch: r.TargetBranch,
				File:         r.File,
				Line:         fromPgInt4(r.Line),
				Name:         r.Name,
				Kind:         r.Kind,
			},
			Depth: r.Depth,
		}
	}
	deps, truncated = trimDependencies(all, effectiveLimit)
	return deps, truncated, nil
}

// AppendSymbolHistory bulk-inserts symbol_history rows with a fresh
// uuid.NewV7 id per entry (docs/ingestion-spec.md "Symbol history":
// append-only, derived from git at ingest). It returns the number of rows
// inserted.
func (s *Store) AppendSymbolHistory(ctx context.Context, entries []HistoryEntryInput) (int64, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	params := make([]gen.InsertSymbolHistoryParams, len(entries))
	for i, e := range entries {
		params[i] = gen.InsertSymbolHistoryParams{
			ID:       pgUUID(uuid.Must(uuid.NewV7())),
			SymbolID: pgUUID(e.SymbolID),
			Commit:   e.Commit,
			Ref:      e.Ref,
			Message:  e.Message,
		}
	}
	count, err := s.q.InsertSymbolHistory(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("appending symbol history: %w", err)
	}
	s.logger.DebugContext(ctx, "appended symbol history", "count", count)
	return count, nil
}

// History returns symbolID's history entries, most-recent-first, up to
// limit rows (limit <= 0 uses defaultLimit). truncated reports whether
// more entries exist beyond limit -- see Dependents' doc comment for why
// this distinction matters; it applies identically here.
func (s *Store) History(ctx context.Context, symbolID uuid.UUID, limit int32) (entries []HistoryEntry, truncated bool, err error) {
	effectiveLimit := clampLimit(limit)
	rows, err := s.q.SymbolHistory(ctx, gen.SymbolHistoryParams{
		SymbolID: pgUUID(symbolID),
		Limit:    fetchLimit(effectiveLimit),
	})
	if err != nil {
		return nil, false, fmt.Errorf("querying history for symbol %s: %w", symbolID, err)
	}
	truncated = int32(len(rows)) > effectiveLimit
	if truncated {
		rows = rows[:effectiveLimit]
	}
	entries = make([]HistoryEntry, len(rows))
	for i, r := range rows {
		entries[i] = HistoryEntry{
			ID:       uuidFromPg(r.ID),
			SymbolID: uuidFromPg(r.SymbolID),
			Commit:   r.Commit,
			Ref:      r.Ref,
			Message:  r.Message,
		}
	}
	return entries, truncated, nil
}

// trimDependentsRows converts Dependents rows to the exported Dependency
// type and trims to effectiveLimit, reporting whether the raw fetch (which
// requested effectiveLimit+1 rows, see fetchLimit) actually found more.
// Deps has an identical row shape but a distinct generated type
// (gen.DepsRow vs gen.DependentsRow), so Deps converts inline and shares
// only trimDependencies, not this conversion step.
func trimDependentsRows(rows []gen.DependentsRow, effectiveLimit int32) ([]Dependency, bool) {
	all := make([]Dependency, len(rows))
	for i, r := range rows {
		all[i] = Dependency{
			Symbol: Symbol{
				ID:           uuidFromPg(r.ID),
				RepoID:       uuidFromPg(r.RepoID),
				TargetBranch: r.TargetBranch,
				File:         r.File,
				Line:         fromPgInt4(r.Line),
				Name:         r.Name,
				Kind:         r.Kind,
			},
			Depth: r.Depth,
		}
	}
	return trimDependencies(all, effectiveLimit)
}

// fromGenSymbol converts a sqlc-generated symbols row (the LookupSymbolsByName
// query selects the symbols table's own columns, so sqlc synthesizes the
// built-in gen.Symbol model rather than a query-specific Row type) into this
// package's exported Symbol type.
func fromGenSymbol(r gen.Symbol) Symbol {
	return Symbol{
		ID:           uuidFromPg(r.ID),
		RepoID:       uuidFromPg(r.RepoID),
		TargetBranch: r.TargetBranch,
		File:         r.File,
		Line:         fromPgInt4(r.Line),
		Name:         r.Name,
		Kind:         r.Kind,
	}
}

// trimSymbols trims symbols to effectiveLimit rows, reporting whether it
// held more than that -- LookupSymbolsByName's analogue of
// trimDependencies, over the plain Symbol shape rather than Dependency.
func trimSymbols(symbols []Symbol, effectiveLimit int32) ([]Symbol, bool) {
	if int32(len(symbols)) > effectiveLimit {
		return symbols[:effectiveLimit], true
	}
	return symbols, false
}

// trimDependencies trims deps to effectiveLimit rows, reporting whether it
// held more than that -- the shared truncation-detection step for
// Dependents and Deps once their rows are in the common Dependency shape.
func trimDependencies(deps []Dependency, effectiveLimit int32) ([]Dependency, bool) {
	if int32(len(deps)) > effectiveLimit {
		return deps[:effectiveLimit], true
	}
	return deps, false
}

// clampLimit applies defaultLimit to a non-positive caller-supplied limit.
// This is a pagination safeguard only; see defaultLimit's doc comment.
func clampLimit(limit int32) int32 {
	if limit <= 0 {
		return defaultLimit
	}
	return limit
}

// fetchLimit returns effectiveLimit+1: Dependents, Deps, and History all
// fetch one extra row internally so the Store methods can tell "exactly
// effectiveLimit rows exist" apart from "more were truncated" without a
// second round-trip. Saturates at math.MaxInt32 instead of overflowing
// negative for the (pathological) case of a caller-supplied limit already
// at the int32 ceiling.
func fetchLimit(effectiveLimit int32) int32 {
	if effectiveLimit >= math.MaxInt32 {
		return math.MaxInt32
	}
	return effectiveLimit + 1
}

// pgUUID converts a uuid.UUID to the pgtype.UUID sqlc-generated params
// expect.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// uuidFromPg converts a pgtype.UUID scanned off a NOT NULL uuid column back
// to uuid.UUID. Every id/foreign-key column this package reads is NOT
// NULL, so a non-Valid value here indicates driver/schema corruption, not a
// legitimate null -- it converts to the zero uuid.UUID rather than panic.
func uuidFromPg(id pgtype.UUID) uuid.UUID {
	if !id.Valid {
		return uuid.UUID{}
	}
	return id.Bytes
}

// pgInt4 converts symbols.line's nullable Go representation (*int32) to
// pgtype.Int4.
func pgInt4(line *int32) pgtype.Int4 {
	if line == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *line, Valid: true}
}

// fromPgInt4 converts a scanned pgtype.Int4 back to symbols.line's nullable
// Go representation.
func fromPgInt4(v pgtype.Int4) *int32 {
	if !v.Valid {
		return nil
	}
	line := v.Int32
	return &line
}
