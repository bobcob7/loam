-- Reverses 0002_code_intel.up.sql: drops the derived code-intelligence
-- tables in dependency order (children before parents; graph_edges and
-- symbol_history reference symbols) and the pgvector extension. The HNSW and
-- btree indexes on these tables are dropped implicitly with their tables.

DROP TABLE IF EXISTS chunks;
DROP TABLE IF EXISTS symbol_history;
DROP TABLE IF EXISTS graph_edges;
DROP TABLE IF EXISTS symbol_references;
DROP TABLE IF EXISTS symbols;
DROP EXTENSION IF EXISTS vector;
