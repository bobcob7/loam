# Ingestion Pipeline Spec

How Loam turns git content into the queryable code graph and RAG vectors. These are
**rebuildable indexes** derived from the git mirror (see `docs/persistence-spec.md`); this
document specifies how they are built and kept current.

Status: **draft.** The pipeline and its decisions are settled for the MVP; tuning details
(chunk sizes, retry policy) firm up during implementation.

## Overview

```
upstream sync (poll)  →  indexed branch advanced?  →  enqueue ingest job
                                                            │
                              ┌─────────────────────────────┴─────────────────────────────┐
                        parse changed files (Tree-sitter)                    chunk + embed changed files
                        → symbols, references                                → chunks + vectors
                              └─────────────────────────────┬─────────────────────────────┘
                                        resolve edges (in-DB) + transactional swap
```

Ingestion is **incremental by default** (only changed files are re-parsed and re-embedded),
**per repo**, and **transactional** (queries keep serving the previous index until the swap
commits).

## Trigger & Scheduling

- The mirror **sync** (already the server's job) polls upstream on a schedule. When the
  repo's **indexed branch** advances — or on first enrollment — it enqueues an ingest job.
- Jobs are rows in Postgres (`ingest_jobs`, see persistence spec): `queued → running →
  succeeded | failed`. An **in-process worker pool** runs them, **serialized per repo** (a
  repo has at most one running ingest; a new trigger while one runs coalesces into a single
  follow-up job). Failures **retry with backoff**.
- Job state feeds `repos.sync_state` and is surfaced to the admin in the web interface (a
  Jobs/Activity view, backed by `RepoAdminService.ListIngestJobs`).

## Indexed Scope

For the MVP, Loam indexes each repo's designated **indexed branch** only — one of its
target branches, chosen at enrollment — and queries reflect it. The derived tables carry
a `target_branch` column (set to the indexed branch now) so indexing **all** enrolled
target branches later is an additive change, not a schema rewrite (Future Work — this
was the original intent, deferred for embedding cost).

## Incremental Build

On an advance from `old_ref` to `new_ref`, the job runs `git diff --name-status old..new`:

- **Deleted / renamed-away** files → drop that file's `symbols`, `symbol_references`, and
  `chunks`.
- **Added / modified** files → re-parse (symbols + references) and re-embed (chunks) for that
  file only.
- **Unchanged** files → left untouched; their symbols, references, chunks, and embeddings are
  reused.
- **Edges** are then **recomputed** for the whole repo by resolving `symbol_references`
  against `symbols` in-DB — cheap (a join, no model inference), and correct against the
  current symbol set even when an unchanged file referenced a symbol that moved.

Everything happens in one transaction; on success it swaps atomically, so a reader never sees
a half-built index.

**Full rebuild** is the fallback, used when incremental isn't safe or possible:
- first ingest of a repo,
- no valid diff base (force-push, history rewrite, shallow/reset ref),
- a Tree-sitter grammar / pipeline version bump, or an embedding-model change,
- the admin changing the repo's indexed branch,
- a manual "reindex" requested by the admin.

Rationale: embedding is the expensive step and is per-file, so skipping unchanged files
avoids re-embedding an entire repo on every small merge; parsing and edge resolution are
cheap enough to keep simple.

## Parse → Graph (Tree-sitter)

- **Tree-sitter** via its C bindings (this is where **cgo** is used), one grammar per
  language. MVP starter grammars: TypeScript/JavaScript, Python, Go. Files with no grammar
  are skipped for the graph (still chunked for RAG if they are text/docs).
- Per file the parser emits **`symbols`** (functions, types, modules — with file + line) and
  **`symbol_references`** (an unresolved name + kind + line).
- **Edge resolution** matches references to symbols within the repo (approximate, name-based,
  intra-repo — no cross-repo edges in the MVP; see README Future Work for SCIP). Produces the
  dependency `graph_edges`; `deps` / `dependents` (blast radius) are recursive CTEs over them.
- **Symbol history** (`history` query) is derived from git for changed files at ingest
  (`git log -L` over the symbol's line range, approximate) and stored.

## Chunk → Embed → Vectors

- **Chunking** reuses the same parse: **code is chunked by symbol** (function/type/class
  boundaries), **docs (markdown) by section** (headings), with a **sliding-window** fallback
  for other text files. Binary/non-text files are skipped.
- **Embedding** goes through a pluggable `Embedder` interface. The MVP implementation calls a
  **local Ollama** embeddings API. Default model **`nomic-embed-text`** (768-dim);
  alternatives include `mxbai-embed-large` (1024-dim, higher quality), `bge-m3` (1024-dim,
  multilingual/long), and `all-minilm` (384-dim, light).
- The chosen model **pins `vector(N)`**. Changing the model requires a full re-embed and a
  migration of the `chunks.embedding` dimension — hence a full rebuild.
- Only changed files are re-embedded (incremental); embeddings land in `pgvector`.
- **Oversized chunks fail loudly, not silently.** Ollama's embed request always sends
  `truncate: false` (its default is `true`), so a chunk exceeding the model's context window
  errors instead of being embedded from silently truncated text — a vector that stopped
  representing the persisted chunk, undetectable downstream, is exactly the kind of
  partial-degrade this pipeline forbids (see Consistency & Failure below). The failure rides
  the same embedder error path as any other embed failure, so the enclosing ingest
  transaction aborts and the previous index stays live, per the stale-but-consistent rule.
- **The chunker keeps chunks under that budget so the failure above is a rare defensive
  backstop, not the normal path (loam-zoa).** `Embedder.ContextWindow()`
  (`internal/ingest/embed`) is the budget's source: the Ollama `Embedder` reports it from a
  `knownModelContextWindows` table (`internal/ingest/embed/ollama`) keyed by model, alongside
  the existing `knownModelDimensions` table, so the budget follows whichever model is
  configured rather than being a constant. The same table's values are also sent as
  `options.num_ctx` on every embed request, so the window `truncate:false` is actually
  enforced against is the one the chunker was told to respect, not whatever Ollama's
  per-version default would otherwise apply — which also means an under-estimate in that
  table now costs real served context, not just internal headroom (see the table's own doc
  comment). `internal/ingest/chunk.EnforceBudget` is the chunk-time enforcement point: given
  chunk units already produced by symbol/section/sliding-window chunking, it splits any unit
  exceeding the budget into sequential pieces on line boundaries where possible, so an
  oversized file is chunked into embeddable pieces instead of failing ingest. Concatenating
  the pieces reproduces the original content exactly, except that a piece that would
  otherwise be pure whitespace (no search value) is folded into its neighbor instead of
  emitted as its own chunk. Because chunking is a pure function over file bytes with no
  tokenizer available, the token budget is converted to a byte budget via a conservative
  estimate (`bytesPerTokenBudget = 2.0` bytes/token, versus ~3.0-3.5 bytes/token measured for
  source code — denser than the ~4 bytes/token typical of English prose, roughly 40%
  headroom) — deliberately not tight enough to be safe for every input (dense CJK text,
  base64 blobs, and minified JS can all run closer to 1 byte/token, defeating the estimate),
  which is why the `truncate:false` rejection above remains as the backstop for that residual
  risk rather than being removed now that the chunker respects the budget in the common case.

## Consistency & Failure

- Each ingest is one transaction; readers see the prior index until it commits.
- **Stale-but-consistent** is the rule: on any failure — including an unreachable
  embedder — nothing commits, so the graph and the chunks never disagree about which
  commit they reflect. The previous index stays live until a retry succeeds; there is no
  partial-degrade mode.
- The transaction also records the **ingested ref** — the commit the index now reflects —
  in `repo_target_branches.ingested_ref` (`docs/persistence-spec.md`). Graph and search
  responses surface it (`docs/cli-spec.md`) so a client can compare it against the
  branch tip and know exactly how stale an answer is.
- On failure the job is marked `failed` with the error recorded, the previous index is left
  intact, and the job is retried with backoff.
- `repos.sync_state` reflects the latest outcome (`idle` / `syncing` / `error`).

## Future Work

- **Incremental edges** — resolve only affected edges instead of recomputing per repo, if the
  in-DB resolution ever becomes a cost.
- **All target branches** — populate the `target_branch` dimension for every enrolled target,
  not just the default.
- **Precise / cross-repo resolution** — SCIP or stack-graphs (see README Future Work), which
  also unlocks cross-repo edges.
- **Chunking & embedding tuning** — overlap, size, and model selection per repo; and tooling
  to re-embed on model changes.
