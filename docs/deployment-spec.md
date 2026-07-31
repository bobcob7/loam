# Deployment Spec

How Loam is packaged, configured, and operated as a running service — the container
image, the kustomize manifest set in `deploy/k8s/`, and the operational procedures
(backup/restore, rollback, readiness) that the manifests' own comments don't fully
carry. This document explains *why* and complements the manifests; it does not restate
their YAML. Agent↔server transport, RPC surface, and the sync/ingest pipelines
themselves are `docs/server-spec.md`, `docs/sync-spec.md`, and `docs/ingestion-spec.md`.

Status: **partial.** The image (`loam-ytt2.1`), its registry publication
(`loam-ytt2.2`), and the kustomize set (`loam-ytt2.3`) are built and merged. Three
pieces of the epic (`loam-ytt2`) are **not yet done**, and this document says so
plainly rather than describing them as if they existed:

- **The Secret is not provisioned anywhere.** `deploy/k8s` references a Secret named
  `loam-secrets` (namespace `loam`) and never creates it — see
  `deploy/k8s/kustomization.yaml`'s header comment for the full key contract. Sealing
  it as a vega-infra sealed-secret is `loam-ytt2.4`, still open.
- **The ArgoCD Application is not merged.** Nothing in `sudo-core/vega-infra` points
  at this manifest set yet; that PR is `loam-ytt2.8`, still open. Loam is not, as of
  this writing, running anywhere.
- **The nightly-only test suites (provider contract, e2e, Playwright) don't run in
  this instance's CI yet.** `.forgejo/workflows/` has `ci.yaml` and `build.yaml` only
  — porting the GitHub-era nightly workflow is `loam-ytt2.10`, still open. Treat
  anything below that leans on "CI already proved X" as leaning on `ci.yaml`'s
  per-PR gate only.

## Env Vars

`internal/config/config.go` and `internal/config/env.go` are the source of truth;
`docs/server-spec.md` documents the same table for the application's own purposes.
This table adds the deployment angle: where each value comes from in
`deploy/k8s/`.

| Variable | Required | Secret | Default | In `deploy/k8s` |
| --- | --- | --- | --- | --- |
| `LOAM_ADMIN_PASSWORD` | yes | yes | — | `loam-secrets` Secret key, same name |
| `LOAM_DATABASE_URL` | yes | yes | — | `loam-secrets` Secret key, same name |
| `LOAM_ENCRYPTION_KEY` | yes | yes | — | `loam-secrets` Secret key, same name |
| `LOAM_HTTP_ADDR` | no | no | `:8080` | `loam-config` ConfigMap |
| `LOAM_ADMIN_USER` | no | no | `admin` | `loam-config` ConfigMap |
| `LOAM_DATA_DIR` | no | no | `/var/lib/loam` | `loam-config` ConfigMap **and** the Dockerfile's `ENV`; the two must agree with each other and with `loam-pvc.yaml`'s mount path — see that manifest's comment |
| `LOAM_SYNC_INTERVAL` | no | no | `60s` | `loam-config` ConfigMap |
| `LOAM_PR_ATTRIBUTION` | no | no | `true` | `loam-config` ConfigMap |
| `LOAM_EMBEDDER_URL` | no | no | `http://localhost:11434` | `loam-config` ConfigMap, **overridden** to `http://ollama.ollama.svc.cluster.local:11434` — the vega cluster's existing ollama addon, confirmed live and already pulling `nomic-embed-text` |
| `LOAM_EMBEDDER_MODEL` | no | no | `nomic-embed-text` | `loam-config` ConfigMap |
| `LOAM_INGEST_WORKERS` | no | no | `2` | `loam-config` ConfigMap |
| `LOAM_LOG_LEVEL` | no | no | `info` | `loam-config` ConfigMap |

Two things worth being explicit about, since the manifests only hint at them:

- **All three secrets live in one Secret object**, not three, and the Postgres
  StatefulSet draws `POSTGRES_USER`/`POSTGRES_DB`/`POSTGRES_PASSWORD` from that same
  object — six keys total. `postgres-statefulset.yaml`'s comment flags the one
  invariant Kubernetes cannot enforce for you: `POSTGRES_PASSWORD` and the password
  embedded in `LOAM_DATABASE_URL`'s DSN describe the same credential from two sides.
  A mismatch doesn't fail to apply — it surfaces later, as `/readyz` reporting
  `database unreachable` with an authentication error in the log.
- **`LOAM_DATA_DIR` is set twice** (image `ENV` default, ConfigMap override) and both
  currently agree on `/var/lib/loam`. If you ever need to change it, change both, plus
  the PVC mount path — nothing checks these three stay in sync.

## The Stateful Surface

`LOAM_DATA_DIR` is **not a cache.** It holds the bare git mirrors
(`<dir>/mirrors/<group>/<repo_name>.git`) that the git smart-HTTP endpoints serve
`upload-pack`/`receive-pack` out of directly, and that every enrolled repo's work
branches live on as refs (`docs/git-spec.md`, `docs/persistence-spec.md` → Git
mirrors). Losing the volume has two different costs, and they are not the same size:

- **The mirrors themselves are recoverable.** They are re-fetched from upstream —
  worst case, `EnrollRepo` runs again per repo. Cost: time proportional to the number
  of enrolled repos and their history size, and every *derived* index
  (`repo_target_branches.ingested_ref` et al.) goes stale until the next ingest cycle
  rebuilds it — not silent data loss, just a rebuild window.
- **Work branches that exist only as unpushed refs in the mirror are not
  recoverable.** A work branch's commits are agent-pushed git objects
  (`docs/git-spec.md`); Postgres's `work_branches` row records the branch's *name*,
  *state*, and metadata, but the diff and the commits themselves live only in git
  (`docs/persistence-spec.md` → work_branches: "The diff is not stored; it is
  computed from git"). A branch that was never `AcceptProposal`'d (never pushed
  upstream as `loam/<name>`) has no copy anywhere else. Losing the PVC before that
  push loses that work branch's actual content — the Postgres row survives, pointing
  at a ref that no longer exists.

This is why `loam-pvc.yaml` pins `storageClassName: iscsi` rather than the cluster
default (`nfs`) — see that manifest's comment for the git-on-NFS locking hazard — and
why `loam-deployment.yaml` is a single-replica `Deployment` with `strategy: Recreate`
rather than a `StatefulSet` or a scaled-out Deployment: per-repo sync/ingest
serialization is an in-process invariant today (`docs/server-spec.md` → Process
Model), not merely a capacity choice, so there is no safe way to run two pods against
this volume regardless of access mode.

## Backup & Restore

Two stores, two different recovery stories, and a coupling between them that is the
single easiest thing to get wrong in an actual incident.

**Neither store has automated backup wired up yet in this repo or in the
vega-infra reconnaissance this epic did** (`loam-ytt2`'s description surveys
`addons/docs` in detail and does not mention a backup mechanism for its own
Postgres). What follows describes the correct manual procedure and its ordering
constraints; provisioning it as a recurring job is unscoped work, not yet filed as
its own bead.

### Git mirrors

Ordinary filesystem/PVC backup (volume snapshot, or `tar` off a live mount) is
sufficient — nothing about the mirrors requires application-level coordination to
back up, since the server never writes to a mirror outside a single git process
invocation (fetch, or a client's push through the smart-HTTP handler). Restoring an
**older** snapshot loses any work-branch pushes or upstream fetches that happened
after it was taken; the next sync cycle re-fetches upstream state (upstream-wins,
`docs/sync-spec.md`), so target branches self-heal. Work-branch refs pushed by agents
after the snapshot do not self-heal — they're gone, the same loss as above.

### Postgres

Standard `pg_dump`/`pg_restore` (or a volume snapshot of the StatefulSet's PVC)
against the `postgres` Service. This is authoritative for everything under
`docs/persistence-spec.md` → Metadata — repos, credentials, roles, work branches,
verdicts, threads, comments — and also holds the derived code-graph/vector tables,
which restoring drags along even though they didn't strictly need backing up (they're
rebuildable from the mirrors by re-ingest; restoring them from a backup is just
faster than a full re-ingest, not a correctness requirement).

### The coupling: `LOAM_ENCRYPTION_KEY`

This is the part most likely to be gotten wrong in an emergency, so it gets stated
loudly: **a Postgres backup restored under any key other than the one that encrypted
it leaves every stored forge credential undecryptable.** `credentials.token_ciphertext`
is AES-GCM under `LOAM_ENCRYPTION_KEY` (`docs/persistence-spec.md` → Secrets); the
key lives only in the `loam-secrets` Secret (once `loam-ytt2.4` provisions it) and
nowhere in Postgres. A Postgres backup carries the ciphertext; it does not carry the
key that opens it.

The key is **not rotatable in place**, and the actual failure mode today is worth
being precise about, because it's better than the naive expectation: `cmd/server`'s
`verifyEncryptionKeyAgainstStoredCredentials` (`cmd/server/credentialcheck.go`, run
right after the Postgres pool connects — `docs/server-spec.md` → Startup, step 3)
attempts to decrypt every stored credential with the just-loaded key **before the
HTTP listener ever binds**. A mismatched key — a restored backup with the wrong key,
a rotated key that was never used to re-encrypt existing rows — does not degrade
quietly into "every repo fails to sync." **It refuses to start at all.** The pod
goes into `CrashLoopBackOff`, `/healthz` and `/readyz` never come up, and the log
carries `LOAM_ENCRYPTION_KEY does not match the key that encrypted it (or the row is
corrupt)` for the first offending host. This is deliberate — see that file's own doc
comment on why a static misconfiguration like this is treated as fail-fast rather
than a `/readyz` degradation (a single-replica MVP has no other instance to route
around, and a wrong key never fixes itself on a bare restart).

**Recovery path** (re-entering every forge credential, as the epic anticipates,
plus the one step that check forces first):

1. The server cannot boot far enough to serve `CredentialService`, and that service
   has no "clear a broken credential" RPC in the first place (`SetUpstreamToken` only
   sets/replaces — `docs/web-spec.md` → CredentialService) — so this step has to be
   direct SQL against Postgres: null out (or delete) `token_ciphertext` for the
   affected `credentials` rows. `repos.forge_host` is a soft reference to
   `credentials.host` (`docs/persistence-spec.md` → repos), so deleting the row
   outright is safe — no enrolled repo needs to change.
2. Let the pod restart (it already is, via `CrashLoopBackOff`) or force one. With no
   token left to fail decrypting, `verifyEncryptionKeyAgainstStoredCredentials` has
   nothing to check and startup proceeds.
3. Re-enter each forge credential via `CredentialService.SetUpstreamToken` (the admin
   UI or CLI) for every host that lost its token. This re-encrypts under the
   **current** `LOAM_ENCRYPTION_KEY` and re-validates.
4. Enrolled repos resume sync/push without re-enrollment — the credential is
   host-keyed and shared, not per-repo.

The takeaway for provisioning `loam-secrets` in the first place (`loam-ytt2.4`'s own
description says the same): write down how the key was generated and where a copy is
kept outside git and outside the cluster, because the only other recovery path is
the one above.

## Rollback

`deploy/k8s/loam-deployment.yaml` pins the container image to an **immutable,
commit-sha manifest list** —
`registry.bobcob7.com/loam/server:<full-git-sha>` — never `:latest`. `:latest` is
also published (`.forgejo/workflows/build.yaml`'s `merge` job pushes both tags), and
deliberately is **not** what gets pinned: floating a manifest reference means the
YAML in git never changes while the image underneath it does, so a `git revert`
reverts nothing, ArgoCD reports `Synced` with no commit anywhere saying what's
actually running, and there is nothing to roll back *to*. With the sha pinned,
bumping the version is an ordinary reviewable diff, and rolling back the image is
`git revert` on that diff, same as any other change ArgoCD reconciles.

**That only rolls back the binary. It does not roll back the schema**, and the two
are not symmetric:

- Migrations run automatically, forward-only, as part of every server startup
  (`internal/db/migrations.Migrate`, called before the pool connects —
  `docs/server-spec.md` → Startup, step 2). There is no separate migration job or
  step in `deploy/k8s` to skip; every pod start re-runs `Migrate`, and it's a no-op
  once current.
- `internal/db/migrations.Down` — which actually reverts, running each
  `NNNN_name.down.sql` in reverse — exists and is exercised by the integration suite
  (`internal/storesuite/migrations_integration_test.go`), but **nothing in this
  repo's shipped binaries calls it.** `cmd/server` only ever calls `Migrate` (up).
  Rolling the schema back for real means running `Down` yourself against the live
  DSN — there is no `task migrate:down` or equivalent packaged today. Budget for
  writing that throwaway invocation (or wiring a proper one) before you need it under
  pressure, not during the incident.
- The current head migration, **`0004_work_branches_accepted_tip`, is additive** — it
  adds a nullable column and back-fills nothing (`docs/persistence-spec.md`:
  "`null` on every row accepted before this column existed"). Rolling the image back
  to a pre-`0004` binary while the schema stays at `0004` is safe: the old binary's
  queries simply never reference the new column. **This is a property of `0004`
  specifically, not a guarantee about migrations in general.** A future migration
  that drops a column, tightens a `NOT NULL`, or renames something an old binary's
  sqlc-generated queries still reference would break the old binary immediately on
  rollback. Before rolling back past any future migration, check whether it's
  additive; if it isn't, the schema has to move too (via `Down`, per the caveat
  above), and the two rollbacks need to happen in the right order — schema forward
  compatibility with the old binary, then the image swap, not the reverse.

## The Readiness Caveat

**A green `/readyz` does not mean sync or ingest are alive.** This is deliberate, not
an oversight, and `internal/health`'s package doc says exactly why: readiness checks
only what makes *this process* unable to answer *any* request correctly — Postgres
reachability and migration currency — and deliberately excludes the embedder, the
forge, the policy socket, the ingest pool, and the sync scheduler, because each of
those failing only degrades a subset of the surface, and folding a partial-dependency
failure into readiness is exactly the cascade shape that package's doc warns against.
`loam-deployment.yaml`'s `readinessProbe`/`livenessProbe` comments say the same thing
inline; this section says *why it matters operationally*.

The concrete way this bites: `loam-lae` found that a dead `multiRunner` member (the
sync scheduler or the ingest pool's own run loop panicking and being recovered)
produces **exactly one error log line and nothing else** — no metric, no readiness
flip, nothing that would page anyone. A dead scheduler simply stops advancing
`repos.sync_state`; a dead ingest pool simply stops draining `ingest_jobs`. Both look,
from the outside, identical to "quiet because there's nothing to do." `loam-ymyq`
found a third, related gap: the policy socket's own accept loop isn't even inside
`multiRunner`'s guard, so a panic there is still fully fatal to the process (which,
perversely, *would* trip liveness and restart — the dangerous case is the recovered
one that leaves the process up but the subsystem dead).

**How an operator actually checks these two subsystems**, since `/readyz` won't tell
you:

- **Sync scheduler**: call `RepoAdminService.ListRepos`/`GetRepo`
  (`docs/web-spec.md` → RepoAdminService) and look at each repo's `SyncStatus` —
  `state` and `last_synced_at`. A healthy scheduler advances `last_synced_at` on
  every repo roughly every `LOAM_SYNC_INTERVAL`. If it has stopped moving across
  every repo at once (not just one repo stuck in `error` for its own reason — that's
  the ordinary, expected failure mode `docs/sync-spec.md` already covers), the
  scheduler itself is the suspect. Check the server's stdout log for the one line
  `loam-lae` says to expect around the time it stopped.
- **Ingest pool**: call `RepoAdminService.ListIngestJobs`
  (`docs/web-spec.md` → RepoAdminService) filtered by `status=queued`. A healthy pool
  drains `queued` jobs into `running` and then `succeeded`/`failed` within roughly a
  sync interval of being enqueued (`docs/ingestion-spec.md` → Trigger & Scheduling).
  A `queued` count that only grows, with nothing ever transitioning to `running`, is
  the pool being dead, not merely busy or backed up — a merely-busy pool still shows
  jobs in `running`.

Neither check is currently automatable from outside the process (no metric exists —
`loam-lae`'s own close notes flag this as a known, deliberate gap rather than a
missed one, to avoid quietly contradicting `internal/health`'s documented design
before that design itself is revisited). Until that changes, "is sync/ingest alive"
is a manual RPC check, not a probe.

## Running Locally

There is no local Kubernetes story in this repo — no `kind`/`k3d` target, no
`kubectl apply -k` walkthrough — because nothing has actually run the manifests
against a real cluster from this repo yet (that first real run is `loam-ytt2.8`).
`kubectl kustomize deploy/k8s` renders the manifest set without starting anything and
is safe to run any time as a lint/sanity check; it's the closest thing to a "does
this compose" check available without a cluster.

For an actual running comparison, `deploy/docker-compose.e2e.yml` is the closest
local analogue to the deployed shape: Postgres/pgvector on the same image and tag
pinned in `postgres-statefulset.yaml`'s comment (`pgvector/pgvector:pg16` — the two
are pinned together deliberately, "no version drift in the extension between the
two"), plus a seeded real Forgejo (`docs/testing-spec.md` → Layer 3). It does **not**
containerize the server itself — `task test:e2e` runs the host-built binary
(`task build:bin`) against the two compose services over their published host ports,
rather than adding container-to-container networking this repo doesn't otherwise
need.

To exercise the **actual container image** (the one the k8s manifests deploy) rather
than a host binary, `task docker:build` builds it locally from the same `Dockerfile`
CI uses (single-arch, whatever architecture invokes it — see that task's own
description for how it relates to `.forgejo/workflows/build.yaml`'s multi-arch
matrix). Running that image needs the same env vars as the table above supplied by
hand (`docker run -e LOAM_ADMIN_PASSWORD=... -e LOAM_DATABASE_URL=... -e
LOAM_ENCRYPTION_KEY=$(openssl rand -base64 32) ...`) against a Postgres reachable
from wherever the container runs — `deploy/docker-compose.e2e.yml`'s `postgres`
service on its published port is the natural pairing, though nothing wires the two
together automatically today.

## Future Work

- **Backup automation.** Neither store has a scheduled backup job; see Backup &
  Restore above.
- **Sync/ingest liveness observability.** `loam-lae`'s close notes name this
  explicitly: a metric or `/readyz`-adjacent signal for a dead scheduler or ingest
  pool, without repeating the cascade mistake `internal/health`'s design otherwise
  guards against.
- **Packaged schema-rollback tooling.** `internal/db/migrations.Down` exists and is
  tested; nothing ships it as a runnable command against a live DSN.
- **Multi-replica.** Tracked in `docs/server-spec.md`; would also change this
  document's PVC/StatefulSet-vs-Deployment reasoning if it ever happens.
- **vega-infra ArgoCD Application, the `loam-secrets` sealed secret, and the nightly
  Forgejo Actions port** — `loam-ytt2.8`, `loam-ytt2.4`, and `loam-ytt2.10`
  respectively, all still open as of this writing.

## Open Questions

None currently open.
