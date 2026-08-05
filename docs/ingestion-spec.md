# Ingestion Pipeline Spec

How Loam turns git content into the queryable code graph and RAG vectors. These are
**rebuildable indexes** derived from the git mirror (see `docs/persistence-spec.md`); this
document specifies how they are built and kept current.

Status: **draft.** The pipeline and its decisions are settled for the MVP; tuning details
(chunk sizes, retry policy) firm up during implementation.

**Retry policy (loam-eean):** a failed job retries with exponential backoff (see "Trigger
& Scheduling" below) up to a **ceiling of 10 attempts**, hardcoded in
`internal/ingest.defaultMaxAttempts` (overridable per-`Pool` via `WithMaxAttempts`, the same
Option-not-environment-variable shape as the existing backoff/poll-interval knobs — this is
a retry-policy tuning value this doc's status line still calls unsettled, not a sizing knob
an operator needs day one the way `LOAM_INGEST_WORKERS` is, so it is not a `LOAM_*`
variable). 10 matches the point the default backoff (1s base, 5-minute cap) itself plateaus:
by the 10th attempt the wait has already reached its cap, so a further attempt would just
repeat the same delay for no new information. A failure the embedder already knows is
permanent (`internal/ingest/embed/ollama.IsPermanent` — a 4xx rejection, including a
context-length overflow that can never fit on retry; a malformed response; a dimension
mismatch) skips retrying immediately, on its very first failure, rather than spending any of
that budget on a request proven to fail identically every time. Once a job stops retrying —
ceiling reached, or a permanent classification — it stays `failed` permanently: `ingest_jobs`
gains no new status for this (its `queued`/`running`/`succeeded`/`failed` CHECK constraint is
unchanged), since a `failed` row whose `attempts` has stopped climbing is already
distinguishable, and `RepoAdminService.ListIngestJobs` already surfaces `status` and
`attempts` verbatim for the Jobs view to read.

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
  intra-repo, intra-language — no cross-repo and no cross-language edges; see README Future Work
  for SCIP). A name colliding across files WITHIN one language can still fan out to more than one
  edge; a name colliding across languages (e.g. fixture-polyglot's Go and TypeScript `Validate`,
  docs/testing-spec.md "Fixtures") must not (loam-w5g). Produces the dependency `graph_edges`;
  `deps` / `dependents` (blast radius) are recursive CTEs over them.
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
  SILENT degrade this pipeline forbids — the one partial-degrade it does permit is counted
  and logged per file (see Consistency & Failure below). The failure rides
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
- **Stale-but-consistent** is the rule: on a failure that reaches the job — including an
  unreachable embedder — nothing commits, so the graph and the chunks never disagree about
  which commit they reflect. The previous index stays live until a retry succeeds.
- **There is exactly one partial-degrade mode, and it is per file on the chunk track**
  (`loam-c94.21`, `loam-c94.24`). If the chunks store rejects one file's write — a type
  error, a constraint, a size limit — that file is skipped and logged at ERROR naming the
  file; the rest of the batch is written and the transaction commits. This is implemented
  with a `SAVEPOINT` around each
  `chunkstore.ReplaceFileChunks` call, because Postgres aborts an entire transaction the
  instant any statement in it errors: without the savepoint the skip happened in Go and
  the commit discarded the batch anyway, which is what the reported production failure
  actually was. What a rejected file leaves behind is precise:
  - Its **previous chunks are kept, whole**, not emptied and not half-replaced — the
    `DELETE` unwinds together with the `INSERT`s. Two different things are true at once
    here, and the rest of this section depends on both: the file's **prior** chunks
    survive the rejection intact, and its **new** chunks never land. So a file that had
    chunks before stays **searchable but stale**, matching the content of an earlier
    commit, while a file whose very first ingest was rejected is **absent** from vector
    search entirely. Neither is corrected until that file is chunked successfully by a
    later ingest. Where text below says a rejected file's chunks are "missing", it means
    this: not that the rows were deleted, but that no row reflects the commit the repo's
    ingested ref now claims.
  - Its **symbols, references and edges are written normally.** The graph track runs
    earlier in the same transaction and `ROLLBACK TO SAVEPOINT` only unwinds statements
    issued after its savepoint. So the degrade is one-sided: the file stays in the graph
    and drops out of RAG search, rather than disappearing from both.
  - The **ingested ref still advances**, so the job is a success and search answers
    correctly report which commit they reflect. A rejected file is not a failed job.
- **A rejected file is counted on three surfaces** (`loam-2d44`): one ERROR log line per
  file from `internal/ingest/vectors.Persist` naming the file and the store's error;
  `ingest_jobs.stats.files_rejected`, which is `ingest.Stats` marshalled verbatim by
  `internal/ingest.Pool.succeed` and is therefore durable and queryable per job; and the
  swap orchestrator's own logging — `files_rejected` on the "ingest committed" line, plus
  a separate WARN line naming the job whenever the count is non-zero, because operators
  alert on level and a field on an INFO line that says the job worked is only findable by
  someone already suspicious. The count reaches `ingest.Stats` from
  `vectors.Stats.FilesRejected`, which before this was computed and discarded one frame
  up, leaving the per-file ERROR log as the only trace a rejection left anywhere.
- **`repos.sync_state` stays `idle` after a partial ingest, deliberately.** A repo whose
  last ingest rejected files shows green in the admin console, and the incompleteness is
  visible only in the job's stats and logs. The alternatives were weighed and rejected:
  - Reusing `error` is the one option the transactional invariant itself forbids.
    `loam-c94.13` writes `repos.sync_state` and `ingest_jobs.status` **in one
    transaction** so the two can never disagree, and `sync_state='error'` beside
    `status='succeeded'` is exactly that disagreement. It also re-conflates "the index
    build blew up" with "this build worked but skipped three files" — the conflation
    `ingest.SyncErrorPrefix` exists to prevent — and nothing would ever clear it, since
    no retry is scheduled for a job that succeeded.
  - A distinct state (`degraded`) is **not** ruled out by that invariant, and being
    precise about this matters because the reasoning is inherited by `loam-qj21`.
    `Pool.succeed` could write `sync_state='degraded'` in the same transaction as
    `ingest_jobs.status='succeeded'` and the two columns would still agree. What rules
    it out is the **clearing rule**. `sync_state` is a property of the REPO;
    `FilesRejected` is a property of one JOB. A rejected file is never re-planned: the
    ingested ref advanced past it (see above), so `git diff ingested_ref..tip` cannot
    name a path nobody touched, and nothing else re-plans it either (`loam-qj21`). So
    the next unrelated incremental ingest, which rejects nothing, writes `idle` and
    clears the flag while that file's chunks are still not the ones the repo's ingested
    ref claims — announcing "fixed" with nothing fixed, which is worse than the green
    badge it replaces because it would be trusted.
  - **Clearing `degraded` only on a `KindFull` ingest** dodges the false-clearing
    problem without any ledger, since a full rebuild genuinely does re-chunk every file.
    It is the cheapest honest option and was weighed rather than overlooked. It is not
    taken here because it makes the STATE honest while leaving the underlying defect —
    a rejected file is never retried — exactly where it is, and because it costs a
    migration plus console work in territory this change does not own. It belongs with
    `loam-qj21`.
  - The fuller fix, which makes the state honest *and* closes the defect, is a **durable
    record of which files are missing current chunks** — a per-repo rejection ledger
    written in the same transaction as the swap, cleared per path when that path's
    chunks are successfully written, with `sync_state` derived from whether the ledger
    is empty. Real work, not a rename; tracked as `loam-qj21`, not part of `loam-2d44`.
- **The graph track has no equivalent mode and takes no savepoint.** Its per-file
  tolerances — no grammar for the extension, an unparseable file, syntax errors — are all
  decided during extraction, before the transaction opens, and skip the store call
  entirely. Once inside the transaction the first failed write aborts the ingest, so
  nothing there continues past a failure that the commit would then discard.
- The transaction also records the **ingested ref** — the commit the index now reflects —
  in `repo_target_branches.ingested_ref` (`docs/persistence-spec.md`). Graph and search
  responses surface it (`docs/cli-spec.md`) so a client can compare it against the
  branch tip and know exactly how stale an answer is.
- On failure the job is marked `failed` with the error recorded, the previous index is left
  intact, and the job is retried with backoff.
- `repos.sync_state` reflects the latest outcome (`idle` / `syncing` / `error`). A
  partially indexed repo is `idle` — see the rejected-file bullets above for why.

## Future Work

- **Incremental edges** — resolve only affected edges instead of recomputing per repo, if the
  in-DB resolution ever becomes a cost.
- **All target branches** — populate the `target_branch` dimension for every enrolled target,
  not just the default.
- **Precise / cross-repo resolution** — SCIP or stack-graphs (see README Future Work), which
  also unlocks cross-repo edges.
- **Chunking & embedding tuning** — overlap, size, and model selection per repo; and tooling
  to re-embed on model changes.
