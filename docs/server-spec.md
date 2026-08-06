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
| `LOAM_DATABASE_URL` | Postgres DSN (pgx). Required unless the discrete `LOAM_DB_*` variables below are used instead — see the precedence note underneath this table. | yes\* | — |
| `LOAM_DB_HOST` | Postgres host, used only when `LOAM_DATABASE_URL` is unset, to assemble a DSN from discrete parts. | yes\* | — |
| `LOAM_DB_PORT` | Postgres port, discrete-parts form only. | no | `5432` |
| `LOAM_DB_USER` | Postgres user, discrete-parts form only. | yes\* | — |
| `LOAM_DB_PASSWORD` | Postgres password, discrete-parts form only. Percent-encoded into the assembled DSN's userinfo, so `/`, `@`, `:`, and `+` are all safe to use. | yes\* | — |
| `LOAM_DB_NAME` | Postgres database name, discrete-parts form only. | yes\* | — |
| `LOAM_DB_SSLMODE` | `sslmode` query parameter on the assembled DSN, discrete-parts form only. | no | `disable` |
| `LOAM_DATA_DIR` | Root of the server's on-disk state: bare mirrors under `<dir>/mirrors/<group>/<repo_name>.git`, the pre-receive policy socket at `<dir>/hook.sock`. | no | `/var/lib/loam` |
| `LOAM_ENCRYPTION_KEY` | 32-byte AES-GCM key, base64 — encrypts forge tokens at rest (`docs/persistence-spec.md` → Secrets). | yes | — |
| `LOAM_SYNC_INTERVAL` | Upstream poll interval per repo (`docs/sync-spec.md`). Must be a positive duration — zero or negative is rejected at startup, not clamped. | no | `60s` |
| `LOAM_PR_ATTRIBUTION` | Append the "Proposed via Loam." footer to upstream PR bodies (`docs/sync-spec.md`). | no | `true` |
| `LOAM_EMBEDDER_URL` | Ollama-compatible embeddings endpoint (`docs/ingestion-spec.md`). | no | `http://localhost:11434` |
| `LOAM_EMBEDDER_MODEL` | Embedding model; pins the `vector(N)` dimension — changing it forces a full re-embed. | no | `nomic-embed-text` |
| `LOAM_INGEST_WORKERS` | Ingest worker pool size (cross-repo parallelism; ingest is serialized per repo regardless). Must be an integer from 1 to 256 — there is no "disabled" value; 0 and below are rejected at startup rather than silently disabling ingest. | no | `2` |
| `LOAM_LOG_LEVEL` | `debug` / `info` / `warn` / `error`. | no | `info` |
| `LOAM_OTEL_ENDPOINT` | OpenTelemetry collector base URL for OTLP-over-HTTP push, e.g. `http://otel-collector:4318`. **Unset disables telemetry entirely** — no exporter is created, no connection opened, no background goroutine started. Must be an absolute `http://` or `https://` URL: the scheme is what selects TLS, and a bare `host:port` is rejected at startup rather than silently exporting nowhere. | no | — (disabled) |
| `LOAM_OTEL_SERVICE_NAME` | `service.name` resource attribute on every span and metric. Overrides the standard `OTEL_SERVICE_NAME` if both are set. | no | `loam` |
| `LOAM_OTEL_SAMPLE_RATIO` | Head-sampling probability for **root spans**, as a float from 0 to 1 inclusive. Values outside that range are rejected at startup, as are `NaN` and `Inf`. The sampler is parent-based, so a request arriving with an already-sampled trace context is always kept regardless of this value — a sampled trace is never a partially-recorded one. | no | `0.1` |
| `LOAM_OTEL_DB_ACQUIRE_THRESHOLD` | How long a Postgres connection-pool acquire must take before it earns its own span, as a Go duration (e.g. `5ms`). Below it, an acquire is a free hand-off from an idle pool and is not worth one span per query on top of the query's own. Lower it when the pool is chronically saturated and waits are hiding under the default. A **failed** acquire — pool exhaustion — is always recorded regardless of this value, since no query runs and nothing else would record it. Negative values are rejected at startup. | no | `50ms` |

**Postgres DSN: two forms, one required.** The server needs a Postgres DSN,
supplied either as `LOAM_DATABASE_URL` directly or assembled at startup from
the discrete `LOAM_DB_HOST`/`PORT`/`USER`/`PASSWORD`/`NAME`/`SSLMODE`
variables. This exists so a Kubernetes manifest can pass one Postgres
password to both the `postgres` image (which only initializes its
superuser from `POSTGRES_PASSWORD`) and loam, instead of also
hand-embedding that same password into a second, DSN-shaped copy that
nothing keeps in sync — a drift between the two previously surfaced as
loam reporting "database unreachable," an authentication failure that
reads like a networking problem.
- **`LOAM_DATABASE_URL` set, no `LOAM_DB_*` parts set:** used as-is
  (validated, not connected to, at startup). This is the form for an
  externally managed database.
- **No `LOAM_DATABASE_URL`, at least one `LOAM_DB_*` part set:** a DSN is
  assembled from the parts. `LOAM_DB_HOST`, `_USER`, `_PASSWORD`, and
  `_NAME` are required in this form; `_PORT` and `_SSLMODE` fall back to
  their defaults above. The password is percent-encoded into the DSN's
  userinfo, so `/`, `@`, `:`, and `+` in `LOAM_DB_PASSWORD` are safe.
- **Both set:** rejected as a startup error. Preferring one side silently
  would leave the other looking configured when it is actually ignored —
  exactly the kind of half-applied config this form exists to prevent, not
  reproduce.
- **Neither set:** rejected as a startup error on `LOAM_DATABASE_URL`
  (missing required variable), the same failure as before this form
  existed.

**Telemetry is push, and it is off by default.** loam pushes OTLP to a
collector; nothing scrapes loam. There is no `/metrics` endpoint and one is
not planned — `/healthz` and `/readyz` are the only unauthenticated routes
(see Health, below), a property the router enforces in code, and a scrape
endpoint would need a third exemption to it. All three `LOAM_OTEL_*`
variables are optional, and with `LOAM_OTEL_ENDPOINT` unset the process does
no telemetry work at all.

Two things worth knowing before turning it on:

- **`LOAM_OTEL_SAMPLE_RATIO=0` is not an off switch.** Sampling applies to
  traces only; there is no metric-side equivalent, so a ratio of 0 silences
  traces while metrics keep being pushed on their normal interval. To stop
  both, unset `LOAM_OTEL_ENDPOINT`.
- **An unreachable collector degrades, it does not fail — given enough
  termination grace.** The server still boots: the endpoint is validated,
  never dialled, at startup. Shutdown stays bounded rather than hanging, but
  the bound is additive and the deployment has to budget for it. The shutdown
  grace period is 30s and the telemetry flush gets its own 5s on top of it
  (`cmd/server/serve.go`: `defaultShutdownGrace`, `telemetryFlushGrace`), so
  the worst case is **35s** — and it is reached in exactly the slow-shutdown
  case this bullet is about. Kubernetes' default `terminationGracePeriodSeconds`
  of 30 SIGKILLs the pod five seconds short, and what that costs is **the
  telemetry, never the drain**: the flush runs strictly *after* the drain, so
  in both worst-case timelines the drain has already had its full 30s by the
  time the overrun happens. Size the grace period to protect the flush.
  **35 is the floor, not a safe value** — setting exactly 35 restores
  precisely the zero margin this bullet is about, with the SIGKILL racing the
  process exit, so the number has to be floor-plus-margin. Note the margin
  was already zero before telemetry existed (a 30s drain against a 30s
  default), so this is a deployment value to set, not a regression to absorb.

Both deployment artifacts now set it, at **45s**, with the arithmetic derived
in place rather than asserted: `helm/loam/values.yaml`'s
`terminationGracePeriodSeconds` comment shows the full floor-plus-margin
working, and `deploy/docker-compose.yml`'s `stop_grace_period` matches it
(compose's own default is 10s, worse than Kubernetes'). Neither is trusted:
`internal/deploycheck` reads `defaultShutdownGrace` and `telemetryFlushGrace`
out of `cmd/server`'s source and fails if either artifact — or
`helm/loam/values.schema.json`'s enforced floor — stops clearing their sum
with margin, so tuning a timeout here cannot leave a stale number there.

Helm values and compose wiring for these variables landed with `loam-uwus`;
`docs/observability-spec.md` is still outstanding on that bead. This table
documents the configuration surface itself.

**Fail fast.** The server validates configuration at startup and exits on the first
problem: missing required variables, a key that isn't 32 bytes after decoding, an
unreachable database, an unwritable data dir, or `LOAM_SYNC_INTERVAL` /
`LOAM_INGEST_WORKERS` / `LOAM_OTEL_SAMPLE_RATIO` outside the range documented
in the table above. A misconfigured server never half-starts.

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
