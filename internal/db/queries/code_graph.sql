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
SELECT enclosing.id AS from_symbol_id, target.id AS to_symbol_id
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
-- Postgres 14 (see internal/codegraph package doc for why this, not the
-- older hand-rolled array idiom, was chosen against the pinned
-- pgvector/pgvector:pg16 image). There is deliberately no separate
-- depth/row cap backing this up -- see the package doc for why one is not
-- needed and why adding one risks masking a broken guard instead of
-- catching it.
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
SELECT DISTINCT ON (s.id) s.id, s.repo_id, s.target_branch, s.file, s.line, s.name, s.kind, d.depth
FROM dependents d
JOIN symbols s ON s.id = d.symbol_id
ORDER BY s.id, d.depth
LIMIT $4;

-- name: Deps :many
-- Forward blast radius: every symbol the target symbol transitively
-- depends on, walking graph_edges forwards from from_symbol_id = target
-- toward to_symbol_id. Same CYCLE-clause termination guard as Dependents
-- above, mirrored in the opposite direction; see that query's comment for
-- the full rationale.
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
SELECT DISTINCT ON (s.id) s.id, s.repo_id, s.target_branch, s.file, s.line, s.name, s.kind, d.depth
FROM deps d
JOIN symbols s ON s.id = d.symbol_id
ORDER BY s.id, d.depth
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
-- separate column to maintain.
SELECT * FROM symbol_history
WHERE symbol_id = $1
ORDER BY id DESC
LIMIT $2;
