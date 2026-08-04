# Compose Quickstart

Loam on one machine with Docker and nothing else — no Kubernetes, no Helm, no
build toolchain. `deploy/docker-compose.yml` and a `.env` you fill in.

**Where this sits.** `docs/deployment-spec.md` is the Kubernetes story: the Helm
chart, the cluster it targets, backup/restore and rollback procedures for a
deployment run by an operator who already has a cluster. This document is the
other rung — one machine, one command — and it is a **sibling** to that one
rather than a section inside it, because the two share almost no operational
surface: no Argo, no sealed secrets, no ingress controller, no PVC storage
classes. `README.md` carries a five-line pointer here rather than the whole
walkthrough, so the README stays an explanation of what loam *is*. Everything
about the container image itself — how it is built, why it runs as uid 10001,
why `loamhook` ships beside the server — is in `docs/deployment-spec.md` and the
`Dockerfile`'s header, and is not repeated here.

This walkthrough was executed end to end against a clean machine while it was
written — from an unedited `.env` through an enrolled repo, a verified agent
identity, and a plain `git push` that the server accepted and attributed to
`ada-lovelace-7-author`. Every command and every quoted output below is real.
Steps that are known to differ between platforms say so.

---

## What you need

- Docker (or a Docker-API-compatible engine — this was verified against both).
- **Compose v2.20 or newer**, either as the `docker compose` plugin or the
  standalone `docker-compose` binary. `deploy/docker-compose.yml` uses
  `depends_on.<service>.required`, which older versions do not understand.
  If `docker compose version` fails on your machine, you have the standalone
  binary instead — use `docker-compose` in place of `docker compose` in every
  command below.
- Somewhere to get an embedder. See step 2.
- About 1.5 GB of disk for the two images, plus whatever your mirrors need.

You do **not** need Go, Node, or a C compiler. The compose file pulls a
published, pinned image.

---

## 1. Configure

```sh
git clone <this repo> && cd loam
cp deploy/.env.example deploy/.env
```

Compose reads `deploy/.env` automatically — the project directory is the
directory holding the compose file, so this works from the repo root with no
`--env-file` flag. `deploy/.env` is gitignored.

Now fill in the four values that have no default. `deploy/.env.example`
explains each one where it is defined; the short version:

```sh
openssl rand -base64 32   # -> LOAM_ENCRYPTION_KEY
openssl rand -base64 24   # -> LOAM_ADMIN_PASSWORD
openssl rand -base64 24   # -> LOAM_DB_PASSWORD
```

`openssl rand` works as-is on macOS and Linux.

> ### Back up `LOAM_ENCRYPTION_KEY` before you go any further
>
> This key encrypts every stored forge credential, and **it cannot be rotated
> in place**. On every boot the server decrypts each stored credential with it
> *before* the HTTP listener binds; a key that does not match refuses to start
> — the container crashloops and `/healthz` never comes up.
>
> **A database backup does not cover it.** The key is nowhere in Postgres.
> `pg_dump` carries the ciphertext and not the thing that opens it, so a
> restore under a different key gives you an instance that cannot start.
>
> Put it wherever you keep secrets you cannot regenerate. Recovery from a lost
> key means deleting every `credentials` row by hand in SQL and re-entering
> every forge token — `docs/deployment-spec.md` → "The coupling:
> `LOAM_ENCRYPTION_KEY`" has the exact procedure.
>
> Never regenerate this key to "reset" something. There is no reset.

The compose file will refuse to render at all if any of the four is unset —
that is deliberate, and the reasoning is recorded in `deploy/.env.example`.
Nothing starts, and the error is the whole of it (this is the actual output of
`up -d` against a freshly copied, unedited `.env`):

```
error while interpolating services.postgres.environment.POSTGRES_PASSWORD:
required variable LOAM_DB_PASSWORD is missing a value:
set LOAM_DB_PASSWORD in deploy/.env -- copy deploy/.env.example and read its comments
```

It reports one variable at a time — the first it reaches, which is not
necessarily the first one in the file — so expect to run `up -d` once per
missing value if you fill them in one at a time.

## 2. Point at an embedder

`LOAM_EMBEDDER_URL` is the fourth must-set value, and no default is correct:
loam's own default (`http://localhost:11434`) means *inside the loam container*
and never works here. Three options, all documented in `deploy/.env.example`:

| You have | Set |
| --- | --- |
| ollama already running on this host, Docker Desktop / Podman | `http://host.docker.internal:11434` |
| ollama already running on this host, plain Linux Docker Engine | `http://172.17.0.1:11434` (check `ip -4 addr show docker0`), or add `extra_hosts: ["host.docker.internal:host-gateway"]` to the loam service |
| ollama on another machine | that machine's URL |
| nothing, and you want the batteries included | `http://ollama:11434`, then add `--profile ollama` to every compose command below |

A host-native ollama must be listening on more than loopback for a container to
reach it: `OLLAMA_HOST=0.0.0.0:11434 ollama serve`.

For the first two options you pull the model yourself:
`ollama pull nomic-embed-text`. The `--profile ollama` path does it for you, in
a one-shot `ollama-init` service that loam waits on, so there is no window in
which loam is ingesting against an ollama that has no model. That profile costs
roughly 5 GB of disk (~2–3 GB image plus a ~274 MB model) and ~1 GB of RAM, and
it is CPU-only — nothing here configures GPU passthrough.

**Changing `LOAM_EMBEDDER_MODEL` is not a pure configuration change.**
`loam-c94.16` fixed an ingest failure where a fixed bytes-per-token ratio could
not bound `nomic-embed-text`'s 2048-token context; that bound is specific to
this model. A different model needs that arithmetic revisited, and vectors
already in the database were produced by the old one.

## 3. Start it

```sh
docker compose -f deploy/docker-compose.yml up -d
```

Compose starts Postgres, waits for `pg_isready` to pass, and only then starts
loam — which matters, because loam runs its database migrations during boot and
exits against a database that is still initialising. A healthy first run looks
like this:

```
 Container loam-postgres-1  Started
 Container loam-postgres-1  Waiting
 Container loam-postgres-1  Healthy
 Container loam-loam-1      Started
```

## 4. Check it is actually working

Not "the page loaded". These three, in order:

```sh
curl -s http://127.0.0.1:8080/healthz     # -> live
curl -s http://127.0.0.1:8080/readyz      # -> ready
docker compose -f deploy/docker-compose.yml ps
```

`/healthz` is unconditional liveness. `/readyz` is the one that means
something: it pings the connection pool *and* checks that the schema matches
the binary, so it stays 503 until migrations have finished. The `ps` output
should show `loam` as `healthy`, which is the container's own healthcheck
calling both endpoints.

Reading the log:

```sh
docker compose -f deploy/docker-compose.yml logs -f loam
```

It is JSON, one object per line. A healthy start ends with the server
announcing its listener; the lines worth knowing on sight are
`"msg":"loading configuration"` at `ERROR` level (a bad or missing `LOAM_*`
variable — the message names which) and anything mentioning
`LOAM_ENCRYPTION_KEY does not match` (the non-rotatable key, above).

**What `/readyz` does not tell you.** It deliberately excludes the sync
scheduler, the ingest pool, the embedder and the forge — see
`docs/deployment-spec.md` → "The Readiness Caveat", which explains how to check
those two subsystems, since a dead one is invisible from outside the process.

## 5. Log in

Open <http://127.0.0.1:8080/> and authenticate with `LOAM_ADMIN_USER` (default
`admin`) and the `LOAM_ADMIN_PASSWORD` you set. The console is HTTP basic auth;
an unauthenticated request gets `401`.

> **The port is bound to `127.0.0.1` on purpose.** Loam's MVP has **no agent
> authentication** — identity and role are trusted exactly as asserted in the
> client's `LOAM_AGENT_*` environment (`README.md` → Agent Identity & Roles).
> Anyone who can reach this port can act as any agent in any role; the admin
> console is behind basic auth, the agent RPC surface is not. Set
> `LOAM_HTTP_BIND=0.0.0.0` only behind something that authenticates, or on a
> network where you trust every host.

## 6. Enrol a repo

This is where you first meet the credential model, and the order matters:
a credential is **per forge host**, not per repo, and enrolment needs it to
already exist.

1. On your forge, create an access token:
   - **Forgejo**: `write:repository` and `write:user` scope.
   - **GitHub**: a **classic** personal access token with `repo` scope.
     Fine-grained PATs and GitHub App installation tokens are not supported
     (`docs/sync-spec.md` → Provider Interface, Limits). GitHub Enterprise
     Server is not supported either — only `github.com` itself.
2. In the console, add it under the forge host — for GitHub, enter `github.com`
   (or `api.github.com`; both resolve to the same credential).
   `CredentialService` validates the token against the host as you save it, so
   a bad token fails here rather than at the first sync.
3. Enrol the repo by its `<group>/<name>` identifier (for GitHub, `<owner>/<repo>`)
   and target branch. Loam probes the upstream, creates a bare mirror under
   `LOAM_DATA_DIR`, and starts fetching it every `LOAM_SYNC_INTERVAL`.

One credential covers every repo on that host, for both the REST API and git
transport. Which forge a repo uses is resolved automatically from its host —
nothing to configure beyond the host and token above
(`docs/sync-spec.md` → "Selecting a provider").

## 7. Point an agent at it

Do not hand-write the agent's environment. `scripts/init-workspace` exists to
do exactly this — it writes the identity block into
`.claude/settings.local.json` and a `CLAUDE.local.md` briefing on the CLI's
sharp edges, excludes both from git, and then *verifies* the profile with a
real server round-trip instead of assuming it works:

```sh
scripts/init-workspace \
  --server http://127.0.0.1:8080 \
  --name ada-lovelace --id 7 --role author \
  --repo yourgroup/yourrepo --branch main
```

Run `scripts/init-workspace --help` for the rest of its options.

### The identity format, because this one is a trap

The agent identifier is **`<name>-<id>-<role>`**, assembled by the CLI from
three separate variables. You set the three parts; you never set the whole
string.

```sh
LOAM_AGENT_NAME=ada-lovelace    # the bare name
LOAM_AGENT_ID=7                 # the bare number
LOAM_AGENT_ROLE=author          # the bare role
# the CLI builds:  ada-lovelace-7-author
```

Putting the assembled identifier into `LOAM_AGENT_NAME` produces identifiers
like `ada-lovelace-7-author-ada-lovelace-7-author-author`, and those go into
permanent review records where nothing rewrites them. Check with
`loam whoami --verify`, which prints the composed identifier and confirms the
server accepts the role:

```
{"name":"ada-lovelace","id":"7","role":"author","identifier":"ada-lovelace-7-author","verified":true}
```

`scripts/init-workspace` writes those three variables into
`.claude/settings.local.json`, which is read by Claude Code and by nothing else.
If you are driving the CLI from an ordinary shell, export them yourself:

```sh
export LOAM_SERVER_URL=http://127.0.0.1:8080
export LOAM_AGENT_NAME=ada-lovelace LOAM_AGENT_ID=7 LOAM_AGENT_ROLE=author
```

### After `loam clone`, source control is plain git

There is no `loam commit` and no `loam push`. `loam clone` bootstraps a working
clone with the server as its only remote and the right refspecs and author
identity; from there you use `git` exactly as you always do. The server
authorises each push at receive time against the identity baked into that
clone — there is no client-side guard to satisfy. (A plain `git clone` against
the server does *not* work: there are no ambient credentials and it fails with
`could not read Username`. Always bootstrap with `loam clone`.)

---

## Operating it

### Backups — two things, and one of them is not in the database

**Postgres** holds repos, credentials, roles, work-branch metadata, verdicts,
threads and comments:

```sh
docker compose -f deploy/docker-compose.yml exec -T postgres \
  pg_dump -U loam loam > loam-$(date +%F).sql
```

Dump *through the container* rather than copying files off the volume — a
file-level copy of a running Postgres is not a backup.

**The `loam-data` volume** holds the bare git mirrors. It is **not a cache**:
a work branch that was never accepted upstream exists only there, and nothing
else has a copy (`docs/deployment-spec.md` → The Stateful Surface). Mirrors of
upstream branches are re-fetchable; unaccepted work branches are not.

```sh
docker run --rm -v loam_loam-data:/data -v "$PWD:/backup" alpine \
  tar czf /backup/loam-data-$(date +%F).tar.gz -C /data .
```

**`LOAM_ENCRYPTION_KEY`** is in neither of those. Back up `deploy/.env`
separately, to somewhere that is not this machine and not this repository. A
Postgres dump restored under a different key gives you a server that will not
start.

### Upgrading

Migrations run on boot, so an upgrade is an image tag and a restart:

1. Edit `image:` in `deploy/docker-compose.yml` (or set `LOAM_IMAGE` in
   `deploy/.env`) to the new commit-sha tag.
2. `docker compose -f deploy/docker-compose.yml up -d`
3. Watch `/readyz` go 503 → 200. It stays 503 until the schema matches the new
   binary, which is exactly the signal you want.

Take the Postgres dump first. Rolling *back* is the same edit in reverse, but
only if the newer version's migrations were backward-compatible — the dump is
the thing that makes a rollback safe, not the tag.

### Stopping, restarting, and destroying

```sh
docker compose -f deploy/docker-compose.yml down      # keeps both volumes
docker compose -f deploy/docker-compose.yml up -d     # picks them back up
docker compose -f deploy/docker-compose.yml down -v   # DESTROYS both volumes
```

`down -v` destroys every mirror, which includes every work branch that was
never accepted upstream.

---

## Troubleshooting

**`required variable LOAM_ADMIN_PASSWORD is missing a value`** — `deploy/.env`
does not exist or does not set it. Nothing started; fix it and re-run.

**loam restarts forever, log says `LOAM_DATA_DIR: data directory not
writable`** — you swapped the named volume for a bind mount. The image runs as
uid/gid 10001; Docker seeds a *named* volume from the image including its
ownership, but creates a *bind mount* owned by root and chowns nothing.
Kubernetes solves this with `fsGroup: 10001`; compose has no equivalent, so the
ownership has to already be right:

```sh
mkdir -p ./data && sudo chown -R 10001:10001 ./data
```

before the first `up`.

**What the log actually says on the currently pinned image** — verified against
`registry.bobcob7.com/loam/server:bdea9ab7c5b0…`, which is what
`deploy/docker-compose.yml` runs today:

```
{"level":"ERROR","msg":"loading configuration","error":"LOAM_DATA_DIR: data
 directory not writable: open /var/lib/loam/.loam-write-check-1977453151:
 permission denied"}
```

It names the path and **not** the uid that was denied, which is the fact you
need — so if you are reading this because of that message, the answer is above:
the process is uid/gid **10001**, and the mount has to let that uid write.
`docker compose exec loam id` confirms it on a running container, and
`ls -ldn ./data` shows who owns the directory now.

`internal/config` was fixed as part of this work to name the uid, the gid, the
directory and the exact `chown` in that error. That fix is in the tree but
**not in the pinned image**, which predates it; it reaches you the next time
the image tag here is bumped past that commit. Until then, the message is the
one quoted above.

**loam restarts forever, log mentions `LOAM_ENCRYPTION_KEY does not match`** —
the key in `deploy/.env` is not the one that encrypted the stored credentials.
Restore the right key. If it is gone, `docs/deployment-spec.md` → "The
coupling: `LOAM_ENCRYPTION_KEY`" has the recovery procedure, and it involves
re-entering every forge token.

**`/readyz` stays 503** — check `docker compose ... logs loam` and
`... logs postgres`. Most often the database is still migrating (wait), or
`LOAM_DB_PASSWORD` was changed after the first `up` (the Postgres image only
consumes it when it initialises an empty data directory, so loam is now sending
a password the database never adopted).

**Ingest jobs never leave `queued`** — that is the embedder, not loam. Check
`LOAM_EMBEDDER_URL` is reachable *from inside the container*
(`docker compose ... exec loam wget -qO- $LOAM_EMBEDDER_URL`) and that
`nomic-embed-text` is actually pulled there.

**Anything involving `type "vector" does not exist`** — you are not running
pgvector. `deploy/docker-compose.yml` pins `pgvector/pgvector:pg16`; plain
`postgres` cannot run loam's migrations and is not supported.

---

## Checking the stack still works

`task test:compose` brings this exact file up from clean, asserts the startup
ordering, readiness, and that the admin console serves the real SPA, then tears
it down including volumes. It is nightly-tier because it pulls an image and
boots a database; the cheap half of the same question — did a variable get
renamed, did the Postgres image drift from the e2e stack or the Helm chart —
runs per-PR as an ordinary `go test ./internal/deploycheck/...`. See
`docs/testing-spec.md` → CI Stages.
