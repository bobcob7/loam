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

-- name: DeleteChunksByFile :exec
-- Per-file delete-and-replace (loam-54o.15, docs/persistence-spec.md
-- "chunks"): scoped by repo_id, target_branch, file so only the changed
-- file's rows are replaced on an incremental re-embed; every other file's
-- chunks (and embeddings) are left untouched.
DELETE FROM chunks
WHERE repo_id = $1 AND target_branch = $2 AND file = $3;

-- name: SearchChunksByEmbeddingScoped :many
-- RAG search scoped to a caller-supplied set of repo ids (loam-54o.15,
-- docs/persistence-spec.md "chunks": "filtered by the scope's repo ids"),
-- still nearest-neighbour ordered by cosine distance over the
-- chunks_embedding HNSW index. Distinct from SearchChunksByEmbedding (single
-- repo_id) above, which loam-54o.5 added to prove the sqlc pgvector override
-- compiles and runs -- this is the real multi-repo scoped search the store
-- layer exposes.
-- $1 is the scope's repo ids (cast to uuid[] since it is bound as an array,
-- not a single column value); sqlc surfaces it as an anonymous Column1
-- field (github.com/sqlc-dev/sqlc#2635: sqlc.arg(...)::uuid[] cannot be
-- named under the offline/no-live-database analyzer this repo uses --
-- sqlc.yaml has no `database:` block, per loam-54o.5). The store package
-- (internal/chunkstore) is the only caller and hides this field behind
-- its own Search(ctx, repoIDs []uuid.UUID, ...) method, so the awkward name
-- never leaks past internal/db/gen.
SELECT id, repo_id, target_branch, file, start_line, end_line, content, embedding, created_at
FROM chunks
WHERE repo_id = ANY($1::uuid[]) AND target_branch = $2
ORDER BY embedding <=> $3
LIMIT $4;
