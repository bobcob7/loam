-- Code graph queries (loam-54o.14): symbols, symbol_references, graph_edges,
-- symbol_history -- the derived, rebuildable tables from
-- docs/persistence-spec.md "Code intelligence (derived, rebuildable)".
-- IDs are generated in Go (uuid.NewV7, per persistence-spec "Conventions"),
-- never in SQL, so every insert here takes id as a bound parameter.

-- name: DeleteSymbolsForFile :exec
-- Per-file delete half of "delete-and-replace" (docs/ingestion-spec.md
-- "Incremental Build"): drops every symbols row for one file so the
-- caller can re-insert the freshly parsed set in the same transaction.
DELETE FROM symbols
WHERE repo_id = $1 AND target_branch = $2 AND file = $3;

-- name: InsertSymbols :copyfrom
-- Bulk half of "delete-and-replace" for one file's freshly parsed symbols.
INSERT INTO symbols (id, repo_id, target_branch, file, line, name, kind)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: LookupSymbolsByName :many
-- Name -> symbols resolution (loam-awr, docs/cli-spec.md "Graph DB
-- queries"): backs `graph def` directly, and is the uuid-resolution step
-- `graph deps`/`dependents`/`history` need before they can call
-- Dependents/Deps/SymbolHistory, which all take a symbol id rather than a
-- name. Matching is exact-name, scoped to (repo_id, target_branch) --
-- consistent with ResolveGraphEdgeCandidates' to_symbol_id match above,
-- which is also exact-name within a repo/branch. Neither spec defines
-- "approximate" as fuzzy string matching: docs/cli-spec.md:528-530 glosses
-- it as an ambiguous target matching several symbols whose union is "data,
-- not an error", which is exactly what exact-name-plus-return-every-match
-- implements here. (The edge-building comment below uses the same word for
-- a different thing -- that step's line-proximity from-side heuristic.)
--
-- Scope is repo_id = ANY($1::uuid[]) rather than a single repo_id because a
-- name lookup can legitimately span repos: docs/cli-spec.md:550-551 gives
-- `--all` to run the query across all enrolled repos, and :553-557 defines
-- that as a fan-out that "queries each repo's graph independently and unions
-- the results" -- which is exactly this union. Dependents/Deps' single-repoID
-- shape is equally correct and NOT an inconsistency to reconcile later: those
-- are recursive CTEs whose depth-ordered dedup and LIMIT are only
-- well-defined within one repo. The plural shape also happens to match
-- internal/chunkstore.SearchChunksByEmbeddingScoped (a caller-supplied set of
-- repo ids, one target_branch), so the two store packages agree. See
-- SearchChunksByEmbeddingScoped's comment for why sqlc surfaces the array
-- param as an anonymous Column1 (sqlc#2635): the store layer
-- (internal/codegraph.Store.LookupSymbolsByName) hides it behind a typed
-- repoIDs []uuid.UUID parameter, same as chunkstore.Search does.
--
-- $4 is the optional --file narrowing (docs/cli-spec.md: "--file <path>
-- narrows the target to the definition in one file"); an empty string
-- means "no file filter" rather than "match the empty-string file", since
-- file is NOT NULL text and never legitimately empty (every symbol has a
-- real file path). This mirrors the "empty means no filter, not a
-- distinct value" sentinel already used for other optional-narrowing
-- params in this codebase's Go layer (e.g. IngestedRef's Ok flag,
-- internal/reposstore) rather than a nullable/sqlc.narg param -- sqlc.yaml
-- has no `database:` block (see its own comment), so a param needs no
-- live-connection type inference here, just a plain OR-guarded predicate.
--
-- An empty result is the caller's ONLY not-found signal: this query
-- returns zero rows both when no symbol named $3 exists at all and when
-- one exists with zero graph_edges -- deliberately, so a handler can
-- finally tell "no such symbol" (LookupSymbolsByName returns nothing) from
-- "symbol exists with zero edges" (LookupSymbolsByName returns one match,
-- then Dependents/Deps on its id returns an empty, non-truncated set).
-- Ambiguity is NOT an error: several distinct symbols_id rows sharing $3's
-- name is ordinary data (docs/cli-spec.md:528-533, three `Login`s in three
-- files), so this returns every match, not just one.
--
-- Ordered by file, then line (NULLS LAST for file-level symbols, which
-- carry no line -- SymbolInput's doc comment), then id as a final
-- deterministic tiebreak -- there is no "depth" concept here (unlike
-- Dependents/Deps), so this is a plain, stable ordering for a caller that
-- needs one, not a truncation-quality ordering.
--
-- Callers pass limit+1 (this package's fetchLimit convention) so the Store
-- can detect truncation itself, exactly as Dependents/Deps/History do --
-- docs/cli-spec.md:535-537 requires `truncated: true` in the envelope for
-- every graph subquery's capped response, not only the blast-radius ones.
SELECT s.id, s.repo_id, s.target_branch, s.file, s.line, s.name, s.kind
FROM symbols s
WHERE s.repo_id = ANY($1::uuid[])
  AND s.target_branch = $2
  AND s.name = $3
  AND ($4::text = '' OR s.file = $4::text)
ORDER BY s.file, s.line NULLS LAST, s.id
LIMIT $5;

-- name: LookupReferencesByName :many
-- Name -> symbol_references resolution (loam-4na, docs/cli-spec.md "Graph DB
-- queries"): backs `graph refs` directly, mirroring LookupSymbolsByName
-- above in shape, scoping, and the limit/truncated contract -- deliberately,
-- per this bead's DESIGN CONSTRAINT, so `graph def`'s and `graph refs`'s
-- store seams do not silently diverge just because their target tables
-- happen to be column-for-column compatible.
--
-- symbol_references carries no symbol_id and no FK to symbols
-- (0002_code_intel.up.sql), but that is NOT why this query selects straight
-- from symbol_references rather than joining through symbols on
-- (repo_id, target_branch, name) -- an FK is not required to join on a
-- plain triple, and LEFT JOIN symbols -> symbol_references would compile
-- and run fine. It is the wrong query anyway, for three independent
-- reasons, any one of which rules it out on its own:
--   1. It would silently DROP legitimate reference rows: docs/cli-spec.md
--      :553-557 says the MVP does not resolve cross-repo dependency edges,
--      so a reference to a name defined in another repo, or in a
--      third-party library never present in symbols at all, has no match
--      on the symbols side of the join and would vanish from the result --
--      a real use site returning zero rows instead of the row it should
--      produce, contradicting the refs row shape at :544 and the exit-3
--      "not found" contract at :570-571 (a reference existing is not the
--      same as its target being locally defined).
--   2. Ambiguity is data, not an error (docs/cli-spec.md:528-533): three
--      distinct `Login` definitions joined against N references to
--      "Login" produce a 3*N-row cartesian product before LIMIT ever
--      applies, so a capped call would cap an inflated join rather than
--      the real reference count -- breaking the truncated contract
--      (:535-537) by making it depend on definition-side ambiguity that
--      has nothing to do with how many references actually exist.
--   3. --file becomes ambiguous the moment both sides of a join are in
--      play: does it narrow the definition's file or the reference's
--      file? symbol_references alone has exactly one file column, so the
--      question does not arise.
-- Selecting straight from symbol_references sidesteps all three: it
-- returns exactly the reference rows that exist, one row each, --file
-- narrows the one column it could possibly mean. See
-- internal/codegraph.Reference's doc comment for the analogous reasoning
-- on why the row also gets a distinct Go type rather than reusing Symbol.
--
-- Scope is repo_id = ANY($1::uuid[]) for the identical reason
-- LookupSymbolsByName's is: `graph refs --all` fans out and unions across
-- enrolled repos (docs/cli-spec.md:553-557), and an empty repoIDs matches
-- nothing (internal/codegraph.Store.LookupReferencesByName), mirroring
-- internal/chunkstore.Search's "empty scope means search nothing" rule.
--
-- $4 is the optional --file narrowing, same empty-string-means-no-filter
-- sentinel as LookupSymbolsByName's $4 (symbol_references.file is NOT NULL
-- text and never legitimately empty).
--
-- Ordered by file, then line, then id as a final deterministic tiebreak.
-- Unlike LookupSymbolsByName's `s.line NULLS LAST` (symbols.line is
-- nullable for file-level symbols), symbol_references.line is NOT NULL
-- (0002_code_intel.up.sql) -- every reference is a real use site with a
-- concrete line -- so no NULLS LAST is needed here.
--
-- Callers pass limit+1 (this package's fetchLimit convention) so the Store
-- can detect truncation itself, exactly as LookupSymbolsByName does --
-- docs/cli-spec.md:535-537 requires `truncated: true` in the envelope for
-- every graph subquery's capped response, refs included.
SELECT sr.id, sr.repo_id, sr.target_branch, sr.file, sr.name, sr.kind, sr.line
FROM symbol_references sr
WHERE sr.repo_id = ANY($1::uuid[])
  AND sr.target_branch = $2
  AND sr.name = $3
  AND ($4::text = '' OR sr.file = $4::text)
ORDER BY sr.file, sr.line, sr.id
LIMIT $5;

-- name: DeleteSymbolReferencesForFile :exec
-- Per-file delete half of "delete-and-replace" for symbol_references.
DELETE FROM symbol_references
WHERE repo_id = $1 AND target_branch = $2 AND file = $3;

-- name: InsertSymbolReferences :copyfrom
-- Bulk half of "delete-and-replace" for one file's freshly parsed
-- references.
INSERT INTO symbol_references (id, repo_id, target_branch, file, name, kind, line)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: DeleteGraphEdgesForRepoBranch :exec
-- graph_edges is recomputed from scratch each ingest, not per file
-- (docs/persistence-spec.md "graph_edges": "Recomputed each ingest by
-- resolving symbol_references against symbols"). Callers must run this and
-- InsertGraphEdges (fed by ResolveGraphEdgeCandidates) in the same
-- transaction so readers never see a half-recomputed graph
-- (docs/ingestion-spec.md "Consistency & Failure").
DELETE FROM graph_edges
WHERE repo_id = $1 AND target_branch = $2;

-- name: ResolveGraphEdgeCandidates :many
-- Computes the (from_symbol_id, to_symbol_id) pairs graph_edges should hold
-- for (repo_id, target_branch) by resolving symbol_references against
-- symbols -- "intra-repo, name-based, approximate"
-- (docs/persistence-spec.md "graph_edges"; docs/ingestion-spec.md "Edge
-- resolution"). Returns candidates only; the caller assigns fresh
-- uuid.NewV7 ids and bulk-inserts via InsertGraphEdges, after first calling
-- DeleteGraphEdgesForRepoBranch, all inside one transaction.
--
-- from_symbol_id (the REFERENCING symbol) is approximated as the symbol
-- declared in the SAME file at or before the reference's line -- the
-- closest preceding declaration (MAX(line) <= sr.line). symbol_references
-- carries no explicit enclosing-symbol column (0002_code_intel.up.sql), so
-- line-proximity within the same file is the only containment signal the
-- schema gives us; it is wrong for nested or multi-line-signature
-- declarations, which is exactly why the spec calls this resolution
-- "approximate" rather than precise. A reference with no preceding symbol
-- in its file (e.g. a top-of-file reference before any declaration) has no
-- candidate and is silently skipped -- there is no edge to record without a
-- from side.
--
-- to_symbol_id (the REFERENCED symbol) matches purely by name within the
-- same repo_id/target_branch -- no kind or file/language narrowing. A name
-- colliding across files (or languages, e.g. the fixture-polyglot "Validate"
-- defined in both Go and TypeScript, docs/testing-spec.md "Fixtures") can
-- therefore fan out to more than one edge; tightening that is out of this
-- bead's scope (owned by the ingest test suite, loam-li0.8). Self-reference
-- (a symbol whose body references its own name, e.g. recursion) is a real,
-- legitimate case and is deliberately NOT excluded here -- it is exactly
-- the shape of self-edge the dependents/deps CTE cycle-safety guard must
-- handle.
--
-- DISTINCT is required, not cosmetic: a symbol referencing the same name N
-- times in one file produces N identical symbol_references rows resolving
-- to the same (from_symbol_id, to_symbol_id) pair, and graph_edges has no
-- unique constraint on that pair. Parallel duplicate edges between the
-- same two nodes don't just look redundant in the output -- the
-- Dependents/Deps recursive CTEs join through graph_edges with UNION ALL,
-- so k parallel edges between a pair multiply the branch count k times per
-- hop, and intermediate row counts grow as k^depth. Deduplicating only at
-- the end (e.g. via DISTINCT ON in Dependents/Deps) hides the symptom in
-- the final output while that multiplicative work has already been done;
-- deduplicating here, at the source, is what actually avoids it.
SELECT DISTINCT enclosing.id AS from_symbol_id, target.id AS to_symbol_id
FROM symbol_references sr
JOIN LATERAL (
    SELECT s.id
    FROM symbols s
    WHERE s.repo_id = sr.repo_id
      AND s.target_branch = sr.target_branch
      AND s.file = sr.file
      AND s.line IS NOT NULL
      AND s.line <= sr.line
    ORDER BY s.line DESC
    LIMIT 1
) enclosing ON true
JOIN symbols target
    ON target.repo_id = sr.repo_id
   AND target.target_branch = sr.target_branch
   AND target.name = sr.name
WHERE sr.repo_id = $1 AND sr.target_branch = $2;

-- name: InsertGraphEdges :copyfrom
-- Bulk insert of the edges resolved by ResolveGraphEdgeCandidates, ids
-- assigned by the caller (uuid.NewV7). kind is always 'dependency' -- the
-- only value graph_edges_kind_check allows today
-- (0002_code_intel.up.sql).
INSERT INTO graph_edges (id, repo_id, target_branch, from_symbol_id, to_symbol_id, kind)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: Dependents :many
-- Reverse blast radius (docs/persistence-spec.md "graph_edges";
-- docs/cli-spec.md "dependents"): every symbol that transitively depends
-- on the target symbol, walking graph_edges backwards from
-- to_symbol_id = target toward from_symbol_id.
--
-- Cycle safety is the point of this query (docs/testing-spec.md "Layer 2
-- Integration - Store"; bead loam-54o.14 DESIGN): graph_edges can contain
-- cycles because edges are resolved by name with no acyclicity guarantee
-- (self-edges, mutual recursion, longer cycles). The CYCLE clause is
-- Postgres's built-in equivalent of the hand-rolled "visited id array,
-- stop expansion once a value repeats" idiom -- SET is_cycle USING
-- visited_path makes Postgres track, per recursion branch, every symbol_id
-- already emitted on that branch, and it refuses to expand a branch whose
-- next candidate repeats one already in its own path. That is what
-- actually terminates the recursion; it is not decorative. Available since
-- Postgres 14 (see internal/codegraph package doc for the version floor
-- this relies on). There is deliberately no separate depth/row cap backing
-- this up -- see the package doc for why one is not needed and why adding
-- one risks masking a broken guard instead of catching it. The LIMIT
-- below is a pagination cap, not a cycle guard, and cannot double as one:
-- it sits downstream of DISTINCT ON's dedup subquery, which itself
-- requires a full sort over the CTE's output (both are blocking nodes),
-- so nothing here can short-circuit the recursive term early.
--
-- The dedup subquery picks the MINIMUM depth per symbol_id (ORDER BY
-- symbol_id, depth inside DISTINCT ON), then the outer query orders by
-- depth first, id second, before LIMIT applies -- nearest-depth-first,
-- ties broken deterministically by id. Applying LIMIT directly after
-- DISTINCT ON (s.id) (an earlier version of this query did) truncates in
-- whatever order DISTINCT ON's own dedup happens to leave rows in, i.e.
-- symbol UUID order, which is not depth order -- silently returning an
-- arbitrary 50 symbols instead of the nearest 50 for a capped call.
--
-- Callers ask for one more row than they intend to keep (limit+1) so they
-- can detect truncation themselves: this query has no way to signal "more
-- rows existed" on its own, and the caller must not be left to confuse
-- "exactly limit results" with "truncated at limit" (docs/cli-spec.md's
-- `truncated` envelope field depends on that distinction).
WITH RECURSIVE dependents(symbol_id, depth) AS (
    SELECT ge.from_symbol_id, 1
    FROM graph_edges ge
    WHERE ge.repo_id = $1 AND ge.target_branch = $2 AND ge.to_symbol_id = $3
  UNION ALL
    SELECT ge.from_symbol_id, d.depth + 1
    FROM graph_edges ge
    JOIN dependents d ON ge.to_symbol_id = d.symbol_id
    WHERE ge.repo_id = $1 AND ge.target_branch = $2
) CYCLE symbol_id SET is_cycle USING visited_path
SELECT s.id, s.repo_id, s.target_branch, s.file, s.line, s.name, s.kind, deduped.depth
FROM (
    SELECT DISTINCT ON (symbol_id) symbol_id, depth
    FROM dependents
    ORDER BY symbol_id, depth
) deduped
JOIN symbols s ON s.id = deduped.symbol_id
ORDER BY deduped.depth, s.id
LIMIT $4;

-- name: Deps :many
-- Forward blast radius: every symbol the target symbol transitively
-- depends on, walking graph_edges forwards from from_symbol_id = target
-- toward to_symbol_id. Same CYCLE-clause termination guard, dedup-by-
-- minimum-depth, depth-first ordering, and limit+1-for-truncation-
-- detection convention as Dependents above, mirrored in the opposite
-- direction; see that query's comment for the full rationale.
WITH RECURSIVE deps(symbol_id, depth) AS (
    SELECT ge.to_symbol_id, 1
    FROM graph_edges ge
    WHERE ge.repo_id = $1 AND ge.target_branch = $2 AND ge.from_symbol_id = $3
  UNION ALL
    SELECT ge.to_symbol_id, d.depth + 1
    FROM graph_edges ge
    JOIN deps d ON ge.from_symbol_id = d.symbol_id
    WHERE ge.repo_id = $1 AND ge.target_branch = $2
) CYCLE symbol_id SET is_cycle USING visited_path
SELECT s.id, s.repo_id, s.target_branch, s.file, s.line, s.name, s.kind, deduped.depth
FROM (
    SELECT DISTINCT ON (symbol_id) symbol_id, depth
    FROM deps
    ORDER BY symbol_id, depth
) deduped
JOIN symbols s ON s.id = deduped.symbol_id
ORDER BY deduped.depth, s.id
LIMIT $4;

-- name: InsertSymbolHistory :copyfrom
-- Bulk append for symbol_history, populated per docs/ingestion-spec.md
-- "Symbol history": derived from git (`git log -L`) for changed files at
-- ingest. Append-only -- there is no update/delete query for this table;
-- repo-scoped removal reaches it by FK cascade through symbols (loam-05j:
-- symbol_history itself carries no repo_id/target_branch, only symbol_id).
INSERT INTO symbol_history (id, symbol_id, commit, ref, message)
VALUES ($1, $2, $3, $4, $5);

-- name: SymbolHistory :many
-- Backs the `history` query (docs/cli-spec.md). symbol_history has no
-- timestamp column, so chronological order comes from the UUID v7 primary
-- key (docs/persistence-spec.md "Conventions": UUID v7 ids are
-- time-ordered) -- ORDER BY id DESC is most-recent-first without a
-- separate column to maintain. As with Dependents/Deps, the caller passes
-- limit+1 so it can detect truncation itself (docs/cli-spec.md's
-- `truncated` envelope field) rather than confuse "exactly limit results"
-- with "there were more".
SELECT * FROM symbol_history
WHERE symbol_id = $1
ORDER BY id DESC
LIMIT $2;
