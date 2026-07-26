-- name: InsertChunk :one
-- Exercises sqlc's pgvector type override (sqlc.yaml) on the write side:
-- $8 must bind as pgvector.Vector.
INSERT INTO chunks (id, repo_id, target_branch, file, start_line, end_line, content, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: SearchChunksByEmbedding :many
-- RAG search per docs/persistence-spec.md:153: nearest-neighbour over the
-- chunks_embedding HNSW index (0002_code_intel.up.sql), ordered by cosine
-- distance. Exercises the pgvector override on the read/parameter side.
SELECT * FROM chunks
WHERE repo_id = $1 AND target_branch = $2
ORDER BY embedding <=> $3
LIMIT $4;
