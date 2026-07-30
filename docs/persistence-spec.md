# Persistence Spec

How Loam stores its data. Covers the storage topology, the relational data model, the
rebuildable code-intelligence indexes, and how git data and secrets are handled.

Status: **draft.** The topology and data model are settled for the MVP; column-level detail
firms up alongside the migrations.

## Topology

Three stores, with a clear source-of-truth split:

| Store | Holds | Authority |
| --- | --- | --- |
| **Postgres** | metadata (repos, credentials, roles, work branches, verdicts, threads, comments) | **source of truth** |
| **Git mirrors** (filesystem / PVC) | the actual repo objects and refs | **source of truth** for code |
| **Postgres** (graph tables + pgvector) | code graph + RAG embeddings | **derived / rebuildable** |

For the MVP everything database-shaped lives in **one Postgres** — metadata, the code graph
(adjacency tables + recursive CTEs), and vectors (`pgvector`). The graph and vector data are
**rebuildable indexes**, re-derived from the git mirrors on ingest; only the metadata tables
and git are authoritative. Splitting the derived stores onto dedicated services is Future
Work.

- **Driver**: `pgx` (pure Go — no cgo needed for the metadata path).
- **Deployment**: Postgres as its own container (a plain image under testcontainers-go for
  tests; an operator/chart under Argo CD in prod). Git mirrors on a PersistentVolume.
- **Repository seams**: domain logic talks to store interfaces (one per aggregate), so a
  derived store can later move to Qdrant / a graph DB without touching domain code.
- **Conventions**: UUID (v7) primary keys; `timestamptz` `created_at`/`updated_at`; enum-like
  columns as `text` + `CHECK` (evolvable without `ALTER TYPE`); offset pagination via
  `LIMIT/OFFSET` + a `COUNT(*)` for `PageInfo.total`.

## Metadata (source of truth)

### repos
`id`, `name` (unique, `<group>/<repo_name>`), `upstream_url`, `forge_host`,
`indexed_branch` (the target branch the derived indexes are built from), `sync_state`
(`idle`/`syncing`/`error`), `last_synced_at` (null), `sync_error` (null), timestamps.
- `forge_host` links to `credentials.host` (soft reference; a working credential exists before
  enrollment). `RepoAdminService.EnrollRepo` derives it from `upstream_url`: bare
  (`host:port`) for the default `https` scheme, scheme-qualified (`http://host:port`) for a
  plaintext-HTTP upstream -- the one case a bare host cannot be dialled over correctly
  (`internal/forge`'s `apiBaseURL` always defaults a scheme-less host to `https`). A
  credential meant to back an enrollment must be set under the identical string; there is no
  normalization reconciling a mismatched pair (loam-4kz).

### repo_target_branches
`repo_id` (fk → repos), `branch`, `ingested_ref` (null), `ingested_at` (null),
`ingested_versions` (jsonb, null). PK `(repo_id, branch)`. The branches eligible as
work-branch targets. `ingested_ref` is the commit the derived indexes reflect for this
branch — written inside the ingest transaction (`docs/ingestion-spec.md`), null until
first ingest. `ingested_versions` records the grammar/pipeline/embedding-model versions
the current index was built with; the ingest planner compares them against the binary's
versions to trigger the full-rebuild fallback.

### credentials
`id`, `host` (unique, e.g. `github.com`, or `http://forge.internal:3000` for a
plaintext-HTTP forge -- see `repos.forge_host` above), `token_ciphertext` (bytea, null),
`validated` (bool), timestamps. The token covers both forge REST calls and git-over-HTTPS
transport to the upstream (`docs/sync-spec.md`). Secrets encrypted at rest (§Secrets).

### roles
`id`, `name` (unique), `instructions` (text), `builtin` (bool), timestamps. Built-in `author`
and `reviewer` are seeded by migration and cannot be deleted.

### role_operations
`role_id` (fk → roles), `operation` (text, from the capability vocabulary). PK
`(role_id, operation)`.

### work_branches
`id`, `repo_id` (fk → repos), `name` (the randomly generated branch name), `target`, `title`
(null until set), `description` (null), `state`
(`draft`/`reviewable`/`reviewed`/`complete`/`closed`), `author` (agent identifier),
`upstream_pr_url` (null), `upstream_pr_number` (null), `accepted_tip` (null), `conflict`
(`none`/`flagged`/`reset`), `close_reason` (null — set by the admin's `CloseWorkBranch`),
timestamps.
- `UNIQUE (repo_id, name)` — identity is `(repo, name)`.
- `conflict` tracks the mergeability check (`docs/git-spec.md` → Target Advances &
  Catch-Up): `flagged` when a target advance no longer merges cleanly into the branch,
  `reset` when that demoted a `reviewable`/`reviewed` branch to `draft`. A catch-up push
  returns it to `none` — and flips a `reset` branch back to `reviewable`.
- `upstream_pr_number` is the forge-native PR number the sync uses to poll PR state;
  `upstream_pr_url` is display-only (`docs/sync-spec.md`).
- `accepted_tip` is the commit SHA the most recent `AcceptProposal` pushed to
  `loam/<name>` upstream — written by `mirrorsync.StoreProposalAccepter` on every
  accept (a first accept and a re-accept fast-forward alike), never cleared. `null` on
  every row accepted before this column existed. `ProposalService.ListProposals`
  compares it against a live resolve of the work branch's own ref to decide whether an
  already-recorded PR's branch is behind (`docs/web-spec.md` → ProposalService): equal
  means caught up, different (or `null`, treated as "not proven caught up", never as
  "up to date") means it stays in the queue.
- The diff is **not** stored; it is computed from git (`target...name`). The row only points
  at the git ref by `name`. `accepted_tip` is the one exception: it records a point-in-time
  commit identity, not a live pointer, precisely so a later push can be compared against it.

### review_rounds
`id`, `work_branch_id` (fk → work_branches), `number` (1, 2, …), `requested_by` (agent
identifier, the admin, or the server for a conflict catch-up restore), `created_at`.
`UNIQUE (work_branch_id, number)`. A row is created on **every** transition into
`reviewable` — author `request-review`, admin send-back, and the catch-up auto-restore
(`docs/git-spec.md`). The branch's **current round** is its highest `number`. There is no
request comment — discussion lives in threads and replies, each tied to its round.

### verdicts
`id`, `round_id` (fk → review_rounds), `reviewer` (agent identifier), `outcome`
(`approve`/`disapprove`/`neutral`), timestamps.
- `UNIQUE (round_id, reviewer)` — one verdict per reviewer per round; re-submitting
  replaces it.
- **Staleness is derived, not stored**: a verdict is stale iff its round is not the work
  branch's current round. The proposal queue and approval bar count only current-round
  `approve` verdicts.

### threads
`id`, `work_branch_id` (fk → work_branches), `round_id` (fk → review_rounds — the round
the thread was raised in), `author` (who opened it), `file` (null), `line` (null),
`resolved` (bool), timestamps. `file`/`line` are the optional anchor.

### comments
`id`, `thread_id` (fk → threads), `round_id` (fk → review_rounds — the branch's round
when the comment was posted; a reply can land in a later round than its thread),
`author` (agent identifier), `body`, `created_at`. The opening comment creates the
thread; replies add comments.
- **Staged comments are not here** — they live locally in the CLI's `.loam` until the
  reviewer's verdict publishes them (replies are immediate and never staged).

> There is no `agents` table in the MVP: agent identity is trusted from the environment, so
> `author`/`reviewer` are stored as identifier strings, not foreign keys. Server-issued agent
> credentials (an `agents` table) come with the deferred authentication work.

### ingest_jobs
`id`, `repo_id` (fk → repos), `target_branch`, `kind` (`incremental`/`full`), `status`
(`queued`/`running`/`succeeded`/`failed`), `attempts`, `error` (null), `stats` (jsonb —
files parsed, chunks embedded), `queued_at`, `started_at` (null), `finished_at` (null).
Drives the ingest worker queue (one running job per repo) and the web Jobs view. Operational
state, not rebuildable. See `docs/ingestion-spec.md`.

## Code intelligence (derived, rebuildable)

Maintained per repo by the ingestion pipeline (`docs/ingestion-spec.md`): **incrementally** on
a target-branch advance (only changed files re-parsed/re-embedded), or fully on first ingest
and the fallback cases. All tables below carry `repo_id` and `target_branch` — the MVP indexes
only the designated indexed branch; the column leaves room to index all target branches
later.

### symbols
`id`, `repo_id` (fk), `target_branch`, `file`, `line` (null for file-level), `name`, `kind`
(`function`/`type`/`module`/…).

### symbol_references
`id`, `repo_id` (fk), `target_branch`, `file`, `name` (referenced symbol), `kind`, `line`.
The unresolved references the parser emits per file; edges are resolved from these.

### graph_edges
`id`, `repo_id` (fk), `target_branch`, `from_symbol_id` (fk → symbols), `to_symbol_id`
(fk → symbols), `kind` (`dependency`). **Recomputed each ingest** by resolving
`symbol_references` against `symbols` (intra-repo, name-based, approximate).
`dependents`/`deps` (blast radius) are recursive CTEs over this table; MVP edges never cross
repos (fan-out only) and, since loam-w5g, never cross languages either: the to-side match is
narrowed to files whose extension maps to the same language as the referencing file (see
`ResolveGraphEdgeCandidates` in `internal/db/queries/code_graph.sql`), so a name colliding
across languages (e.g. fixture-polyglot's Go and TypeScript `Validate`) resolves to only the
same-language edge.

### symbol_history
`id`, `symbol_id` (fk → symbols), `commit`, `ref`, `message`. Backs the `history` query;
derived from git (`git log -L`) at ingest for the affected files.

### chunks
`id`, `repo_id` (fk), `target_branch`, `file`, `start_line`, `end_line`, `content`,
`embedding vector(N)`, `created_at`. Only changed files are re-embedded on an incremental
ingest. RAG search is `ORDER BY embedding <=> :q LIMIT :n`, filtered by the scope's repo ids,
over an HNSW index:

```sql
CREATE INDEX chunks_embedding ON chunks USING hnsw (embedding vector_cosine_ops);
```

## Git mirrors

Each enrolled repo is a bare mirror on the PersistentVolume (path derived from `repos.name`).
The mirror is the source of truth for code:

- Work branches are git refs; commits arrive via the smart-HTTP git endpoint
  (`docs/git-spec.md`). Each mirror carries the server-written pre-receive hook and
  `receive.denyNonFastForwards` / `receive.denyDeletes` config that enforce push policy,
  rewritten idempotently at enrollment and startup.
- Diffs, blame, and file contents are read from git, not the DB.
- The server syncs from upstream (upstream-wins) and, on target-branch advance, triggers
  ingestion (`docs/ingestion-spec.md`) to update the derived indexes above.

## Secrets

`credentials.token_ciphertext` is encrypted at rest with an app-level key (AES-GCM), the
key supplied to the server via config/env (`LOAM_ENCRYPTION_KEY`) or a KMS. Only
ciphertext is stored; validation status is stored in the clear.

## Migrations, Queries & Testing

- **Migrations**: versioned SQL via **golang-migrate** (`NNNN_name.up.sql` / `.down.sql`),
  embedded with `iofs` + `embed.FS` and applied on startup; built-in roles seeded here.
  SQL-only — the migrations are the single schema of record.
- **Queries**: **sqlc** generates type-safe Go from hand-written SQL, reading the schema
  straight from the migrations directory (so there is no separate schema file to keep in
  sync). Configured for pgx (`sql_package: pgx/v5`). A type override maps the `pgvector`
  `vector` column to `pgvector-go`'s type; enum-like `text` + `CHECK` columns map to Go
  strings.
- **Testing**: integration and godog acceptance tests bring up real Postgres (and the git
  server) via `testcontainers-go`, so specs run against actual infrastructure and the
  sqlc-generated queries. The full test strategy — layers, doubles, harness — is
  `docs/testing-spec.md`.

## Future Work

- **Dedicated vector store.** Move `chunks`/embeddings out of Postgres to a dedicated vector
  DB (e.g. Qdrant) for independent scaling/tuning of RAG. The repository seam makes this a
  store swap, not a domain change.
- **Dedicated graph store.** If traversal depth/scale outgrows recursive CTEs — or when
  precise, cross-repo code intelligence lands (SCIP / stack-graphs, see root README) — move
  the graph to a dedicated store.
