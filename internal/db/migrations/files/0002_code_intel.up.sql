-- Derived code-intelligence schema (loam-54o.4), per docs/persistence-spec.md
-- "Code intelligence (derived, rebuildable)". These tables are rebuildable
-- indexes re-derived from the git mirrors on ingest (docs/ingestion-spec.md);
-- only the metadata tables in 0001_init and git itself are authoritative.
--
-- This is a NEW migration, not a replacement of 0001_init: 0001 now carries
-- the real metadata schema and may already be applied to a live database
-- (loam-54o.3's NOTES), so it is append-only from here forward.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE symbols (
    id              uuid PRIMARY KEY,
    repo_id         uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    target_branch   text NOT NULL,
    file            text NOT NULL,
    line            integer,
    name            text NOT NULL,
    kind            text NOT NULL
);

CREATE INDEX symbols_repo_id_target_branch_idx ON symbols (repo_id, target_branch);

CREATE TABLE symbol_references (
    id              uuid PRIMARY KEY,
    repo_id         uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    target_branch   text NOT NULL,
    file            text NOT NULL,
    name            text NOT NULL,
    kind            text NOT NULL,
    line            integer NOT NULL
);

CREATE INDEX symbol_references_repo_id_target_branch_idx ON symbol_references (repo_id, target_branch);

-- graph_edges.kind is a closed vocabulary today (only 'dependency'), unlike
-- symbols.kind / symbol_references.kind which stay open text per
-- docs/persistence-spec.md ("function/type/module/..."). Recomputed in full
-- each ingest by resolving symbol_references against symbols; the
-- `dependents`/`deps` recursive CTEs walk this table and MUST guard against
-- cycles (internal/testfixture's is_even/is_odd mutual recursion is the
-- fixture that exercises this -- see docs/testing-spec.md "cycle safety").
CREATE TABLE graph_edges (
    id              uuid PRIMARY KEY,
    repo_id         uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    target_branch   text NOT NULL,
    from_symbol_id  uuid NOT NULL REFERENCES symbols (id) ON DELETE CASCADE,
    to_symbol_id    uuid NOT NULL REFERENCES symbols (id) ON DELETE CASCADE,
    kind            text NOT NULL
                        CONSTRAINT graph_edges_kind_check
                        CHECK (kind IN ('dependency'))
);

CREATE INDEX graph_edges_repo_id_target_branch_idx ON graph_edges (repo_id, target_branch);
CREATE INDEX graph_edges_from_symbol_id_idx ON graph_edges (from_symbol_id);
CREATE INDEX graph_edges_to_symbol_id_idx ON graph_edges (to_symbol_id);

CREATE TABLE symbol_history (
    id          uuid PRIMARY KEY,
    symbol_id   uuid NOT NULL REFERENCES symbols (id) ON DELETE CASCADE,
    commit      text NOT NULL,
    ref         text NOT NULL,
    message     text NOT NULL
);

CREATE INDEX symbol_history_symbol_id_idx ON symbol_history (symbol_id);

-- chunks.embedding is vector(768) -- 768 is the ONE documented constant for
-- the MVP embedding width, chosen because it is both production Ollama
-- nomic-embed-text's dimension (docs/ingestion-spec.md) AND
-- internal/testembed.Dimension, so the test schema this migration produces
-- under testcontainers is byte-identical to what production runs. Changing
-- the embedding model changes this literal and requires a full re-embed plus
-- a dimension migration (docs/ingestion-spec.md "Chunk -> Embed -> Vectors").
-- This SQL literal can't reference the Go constant directly -- the enforced
-- invariant lives in code instead:
-- internal/db/migrations/code_intel_integration_test.go asserts the live
-- atttypmod against internal/testembed.Dimension, so a Dimension change
-- fails that test instead of silently drifting from this comment.
CREATE TABLE chunks (
    id              uuid PRIMARY KEY,
    repo_id         uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    target_branch   text NOT NULL,
    file            text NOT NULL,
    start_line      integer NOT NULL,
    end_line        integer NOT NULL,
    content         text NOT NULL,
    embedding       vector(768) NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX chunks_repo_id_target_branch_idx ON chunks (repo_id, target_branch);

-- RAG search is `ORDER BY embedding <=> :q LIMIT :n` (docs/persistence-spec.md
-- :153-158) -- an HNSW index with vector_cosine_ops backs that operator.
-- pgvector's HNSW indexes cap at 2000 dimensions; 768 is safely inside.
CREATE INDEX chunks_embedding ON chunks USING hnsw (embedding vector_cosine_ops);
