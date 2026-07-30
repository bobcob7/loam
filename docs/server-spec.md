# Server Spec

Configuration and process model for the Loam server — the single binary that hosts
everything server-side. Consolidates the environment surface that the other specs
reference piecemeal (`LOAM_ENCRYPTION_KEY`, `LOAM_SYNC_INTERVAL`, `LOAM_PR_ATTRIBUTION`,
admin credentials, …). The *CLI's* environment is separate and specified in
`docs/cli-spec.md`.

Status: **settled for the MVP.** The embedder-outage question is resolved as
stale-but-consistent — ingest stays atomic, and queries surface the ingested ref so
staleness is visible (`docs/ingestion-spec.md` → Consistency & Failure).

## Configuration

Configuration is **environment variables only** — no config file, no flags. This mirrors
the CLI's convention, and deploys cleanly under Kubernetes (values and secretRefs). All
variables are prefixed `LOAM_`; durations use Go syntax (`60s`, `5m`).

| Variable | Purpose | Required | Default |
| --- | --- | --- | --- |
| `LOAM_HTTP_ADDR` | Listen address for the single HTTP port: Connect RPC (CLI + admin), the embedded SPA, and git smart HTTP (`/git/*`). | no | `:8080` |
| `LOAM_ADMIN_USER` | Basic-auth admin username. | no | `admin` |
| `LOAM_ADMIN_PASSWORD` | Basic-auth admin password. Compared constant-time; plaintext env is accepted for the MVP (inject via a secret). | yes | — |
| `LOAM_DATABASE_URL` | Postgres DSN (pgx). | yes | — |
| `LOAM_DATA_DIR` | Root of the server's on-disk state: bare mirrors under `<dir>/mirrors/<group>/<repo_name>.git`, the pre-receive policy socket at `<dir>/hook.sock`. | no | `/var/lib/loam` |
| `LOAM_ENCRYPTION_KEY` | 32-byte AES-GCM key, base64 — encrypts forge tokens at rest (`docs/persistence-spec.md` → Secrets). | yes | — |
| `LOAM_SYNC_INTERVAL` | Upstream poll interval per repo (`docs/sync-spec.md`). Must be a positive duration — zero or negative is rejected at startup, not clamped. | no | `60s` |
| `LOAM_PR_ATTRIBUTION` | Append the "Proposed via Loam." footer to upstream PR bodies (`docs/sync-spec.md`). | no | `true` |
| `LOAM_EMBEDDER_URL` | Ollama-compatible embeddings endpoint (`docs/ingestion-spec.md`). | no | `http://localhost:11434` |
| `LOAM_EMBEDDER_MODEL` | Embedding model; pins the `vector(N)` dimension — changing it forces a full re-embed. | no | `nomic-embed-text` |
| `LOAM_INGEST_WORKERS` | Ingest worker pool size (cross-repo parallelism; ingest is serialized per repo regardless). Must be an integer from 1 to 256 — there is no "disabled" value; 0 and below are rejected at startup rather than silently disabling ingest. | no | `2` |
| `LOAM_LOG_LEVEL` | `debug` / `info` / `warn` / `error`. | no | `info` |

**Fail fast.** The server validates configuration at startup and exits on the first
problem: missing required variables, a key that isn't 32 bytes after decoding, an
unreachable database, an unwritable data dir, or `LOAM_SYNC_INTERVAL` /
`LOAM_INGEST_WORKERS` outside the range documented in the table above. A
misconfigured server never half-starts.

Logging is structured JSON on stdout via `slog`; level from `LOAM_LOG_LEVEL`. No log
files — the platform captures stdout.

## Process Model

One binary, one process, five long-lived components:

- **HTTP listener** (`LOAM_HTTP_ADDR`) — routes by path per `docs/web-spec.md`: CLI
  Connect services, admin Connect services, git smart HTTP, embedded SPA. Auth
  middleware per path prefix; the identity interceptor feeds both RPC and git.
- **Policy socket** (`<data>/hook.sock`) — the unix socket serving pre-receive ref-policy
  decisions to the hook stubs in the mirrors (`docs/git-spec.md` → Enforcement
  Mechanics). Filesystem-local by construction; never exposed on the network.
- **Sync scheduler** — ticks every `LOAM_SYNC_INTERVAL`, running the per-repo sync cycle
  (`docs/sync-spec.md`), serialized per repo. Total in-flight cycles are capped across
  all repos combined, so a large enrollment does not issue one concurrent git fetch per
  enrolled repo per tick; repos over the cap queue and run as slots free, and a sweep
  that outlasts the tick interval simply stretches the effective interval rather than
  piling up. The cap is a fixed build-time value, not a configuration knob for the MVP.
- **Ingest worker pool** (`LOAM_INGEST_WORKERS`) — consumes `ingest_jobs`, serialized per
  repo (`docs/ingestion-spec.md`).
- **Embedder client** — talks to `LOAM_EMBEDDER_URL`; a plain dependency of ingest, not
  a listener.

The MVP is **single-replica**: per-repo serialization for sync and ingest is in-process,
and mirrors live on one volume. Multi-replica coordination is Future Work; nothing in the
external surface (HTTP + Postgres + volume) precludes it later.

## Startup

In order, failing fast at each step:

1. Load and validate configuration.
2. Run **migrations** against the database DSN (golang-migrate, embedded; seeds
   built-in roles — `docs/persistence-spec.md`), **then** connect the Postgres
   connection pool. Migrations must run first: the pool's `AfterConnect` hook
   registers pgvector's `vector` type, which fails until migration `0002`'s
   `CREATE EXTENSION vector` has run, and pgxpool fails every connection
   acquisition (including its own readiness ping) while `AfterConnect` errors —
   connecting the pool before migrating would deadlock permanently on a virgin
   database (`internal/db/pool.go`'s `NewPool` doc comment).
3. **Verify the encryption key against stored credentials**: for every host with a
   stored token, attempt to decrypt it with the just-loaded `LOAM_ENCRYPTION_KEY`.
   Unlike the four checks above, this cannot run from configuration alone — it needs
   a row to test the key against — which is why it runs here, immediately after the
   pool connects, rather than as a fifth item in that list. A key that cannot decrypt
   an existing row exits the process rather than starting: without this,
   `CredentialService.GetCredentialStatus`/`ListCredentials` report every such host as
   present and validated (`has_token`/`validated` never decrypt anything — see their
   own doc comments), while every real use of the credential — `EnrollRepo`, git
   fetch/push, mirror-sync PR creation — fails. A fresh database with no credentials
   yet is unaffected: there is nothing to verify.
4. **Reconcile mirrors**: for every enrolled repo, idempotently rewrite the pre-receive
   hook stub and `receive.denyNonFastForwards` / `receive.denyDeletes` config
   (`docs/git-spec.md`). A mirror missing from disk is re-cloned by the next sync cycle
   (it is derived state; Postgres is the record of enrollment).
5. **Re-queue orphaned jobs**: `ingest_jobs` stuck in `running` (a previous crash) are
   reset to `queued` — safe because ingest is transactional, so a crashed job left no
   partial index.
6. Start the policy socket, sync scheduler, worker pool, and HTTP listener — in that
   order, so git pushes are never accepted while the policy socket is down.

## Shutdown

On SIGTERM: stop accepting new HTTP connections and let in-flight HTTP requests drain
first; only once that drain completes does the policy socket stop accepting — the
mirror image of Startup step 6's ordering, so a push already in flight over HTTP can
still reach the policy socket for the whole time its request is draining, instead of
finding it closed out from under it. The current sync/ingest jobs drain alongside HTTP
(bounded by the same grace period, default `30s`), then exit. Jobs that don't finish in
time are killed and follow the crash path — re-queued on next startup. Nothing requires
cleanup beyond that; all durable state is in Postgres and the mirrors.

## Health

Two unauthenticated endpoints, exempt from basic auth (the only such exemption):

- `GET /healthz` — liveness: the process is up.
- `GET /readyz` — readiness: Postgres reachable and migrations current. The embedder is
  deliberately **not** part of readiness — its failures affect ingest jobs (which retry),
  not request serving.

## Future Work

- **Metrics & tracing** — Prometheus metrics and OTel spans for sync cycles, ingest
  jobs, and RPC latencies.
- **Multi-replica** — move per-repo serialization to Postgres advisory locks and mirrors
  to shared/replicated storage.
- **Hashed admin credential** — accept a bcrypt hash in place of the plaintext password.

## Open Questions

None currently open.
