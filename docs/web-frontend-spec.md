# Web Front-End Spec

Design for the Loam admin SPA — the single-page app served by the server and used by the
admin. It is the front end of the web interface specified in `docs/web-spec.md` (which
covers hosting, auth, and the admin API); this document covers the SPA itself. Never used by
agents.

Status: **draft.** Architecture and stack are settled; component-level detail is filled in
as we build.

## Stack

- **React + TypeScript**, bundled with **Vite**.
- **Connect** client: `@connectrpc/connect-web` (transport) + `@connectrpc/connect-query`
  (TanStack Query hooks) + `@tanstack/react-query`. Typed query/mutation hooks are generated
  from the same protos that back the server.
- **Router**: **`wouter`** — a tiny (~2 KB) hooks-based client-side router. No SSR — this is
  a static SPA; data comes from connect-query, not route loaders.
- **Styling**: minimal hand-written CSS via **CSS Modules**, a small global reset, and CSS
  custom properties for theme tokens. No CSS framework or component library; components are
  hand-rolled (§Conventions).
- **Server state** lives entirely in TanStack Query (via connect-query); local UI state is
  plain React state. No global state library.

The SPA speaks the **Connect protocol** to `connect-go` over the same origin/port, so no
CORS and no separate API host.

## Project Layout

The SPA lives in `web/`, separate from the Go module:

```
web/
  package.json
  vite.config.ts
  buf.gen.yaml          # TypeScript codegen template (§Codegen)
  index.html
  src/
    main.tsx            # entry: QueryClient + transport + router
    transport.ts        # connect-web transport (same-origin)
    routes/             # one module per screen (§Routing & Screens)
    components/         # hand-rolled Table, Dialog, Form, Button, ErrorBanner, …
    gen/                # generated TS (protoc-gen-es + connect-query) — see Codegen
    styles/             # reset.css, tokens.css
  dist/                 # Vite build output, embedded by Go (§Build & Embed)
```

Go embeds `web/dist` from a small package (e.g. `internal/web`) via `//go:embed`.

## Codegen (TypeScript)

The SPA consumes the **same `proto/` schema** as the server, generated to TypeScript:

- Plugins: **`@bufbuild/protoc-gen-es`** (messages + clients) and
  **`@connectrpc/protoc-gen-connect-query`** (TanStack Query hooks), pinned as `web/`
  devDependencies.
- A `web/buf.gen.yaml` template outputs to `web/src/gen/` (both `loam.v1` and
  `loam.admin.v1`). Run through the repo's pinned buf: `go tool buf generate --template
  web/buf.gen.yaml`, wrapped by a Taskfile target (`web:generate`).
- Generated TS is committed, mirroring the Go side (so `web` builds without a codegen step);
  see §Open Questions on whether to commit the bundled `dist`.

This gives the admin the same typed surface the CLI has — e.g. `useQuery(listProposals)`,
`useMutation(acceptProposal)` — straight from the protos.

## Auth

The interface is behind **HTTP Basic auth** (see `docs/web-spec.md`). The SPA does **not**
manage credentials:

- The browser shows its native basic-auth prompt on the first `401` (from a static asset or
  an admin RPC) and thereafter attaches the cached `Authorization` header to same-origin
  requests — including the connect-web `fetch` calls. No login page, no token storage.
- The SPA handles Connect `unauthenticated` / `permission_denied` errors gracefully (an
  "authentication required" state); a refresh re-triggers the browser prompt.
- Basic auth has no clean programmatic logout; treated as out of scope (browser-level).

## Routing & Screens

Client-side routes map to the screens in `docs/web-spec.md`. Each screen lists its data
(queries), actions (mutations), and states. The admin is a **superuser**, so screens freely
mix `loam.admin.v1` and `loam.v1` (`WorkBranchService`) calls.

- **`/` → Repos** (default). List enrolled repos with sync status.
  - Queries: `RepoAdminService.ListRepos`.
  - Actions: `EnrollRepo` (form: upstream URL + target branches).
  - States: loading / empty ("no repos enrolled") / error.
- **`/repos/:repo` → Repo detail.** Target branches, description schema, and the repo's
  credential status.
  - Queries: `RepoAdminService.GetRepo`, `CredentialService.GetCredentialStatus` (repo's host).
  - Actions: `SetTargetBranches`, `SetDescriptionSchema`, `RemoveRepo`.
- **`/credentials` → Credentials.** Per-forge-host tokens and SSH keys.
  - Queries: `CredentialService.ListCredentials`.
  - Actions: `SetUpstreamToken`, `GenerateSSHKeyPair` (shows the public key to copy).
- **`/roles` → Roles.** Agent role editor.
  - Queries: `RoleService.ListRoles`.
  - Actions: `CreateRole` / `UpdateRole` (operation checkboxes + instructions text),
    `DeleteRole` (disabled for `builtin`).
- **`/proposals` → Proposals queue.** Reviewed work branches awaiting a decision.
  - Queries: `ProposalService.ListProposals` (paginated; each carries its verdicts).
- **`/proposals/:repo/:workBranch` → Proposal detail.** The review, its diff, threads, and
  verdicts, with the admin decision.
  - Queries: `WorkBranchService.GetWorkBranch` + `GetWorkBranchDiff` + `ListComments` +
    `ListVerdicts` (admin superuser on `loam.v1`).
  - Actions: `ProposalService.AcceptProposal`, `ProposalService.CloseWorkBranch`,
    `WorkBranchService.RequestReview` (send back for re-review, with a comment).

## Data Layer

- One shared connect-web **transport** (`baseUrl: "/"`, Connect protocol), and one
  `QueryClient`, provided at the root.
- Reads use connect-query `useQuery`; writes use `useMutation`, invalidating the affected
  queries on success (e.g. `EnrollRepo` → invalidate `ListRepos`; `AcceptProposal` →
  invalidate `ListProposals`).
- **Pagination** uses the schema's `Page`/`PageInfo` (`ListProposals`, `ListRepos`) — offset
  paging with a "load more"/pager driven by `PageInfo.total`.
- **Error mapping** from Connect codes to UI: `unauthenticated` → auth state;
  `permission_denied` → "not allowed"; `invalid_argument` → inline form errors;
  `failed_precondition` → action-specific message (e.g. accepting without an approve
  verdict); `not_found` → empty/404 state. A shared `ErrorBanner` renders the rest.

## Build & Embed

- **Build**: `vite build` → `web/dist/` (hashed assets + `index.html`). `web/dist` is
  **gitignored**; `task web:build` produces it before the Go embed compiles.
- **Embed**: a Go package embeds `web/dist` (`//go:embed all:dist`) and serves it with **SPA
  fallback** — any path that is not an RPC route or a real asset returns `index.html`
  (client router takes over). Static + fallback are behind admin basic auth (see web-spec
  routing table).
- **Task integration**: `task web:install` (`npm ci`), `task web:generate`, `task web:build`;
  the top-level build depends on `web:build` so `dist` exists before the Go embed compiles.

## Dev Workflow

- `vite dev` serves the SPA on a dev port with HMR; a Vite **proxy** forwards
  `/loam.v1.*` and `/loam.admin.v1.*` to a locally running Go server, so the dev SPA talks to
  the real backend. Basic auth is entered once in the browser.
- `task web:generate` after any `proto/` change refreshes `web/src/gen/`.

## Conventions

- **TypeScript strict**; no `any` in app code (generated code excepted).
- **Hand-rolled components** on native elements with basic accessibility (labeled inputs,
  focus-trapped dialogs, keyboard-dismiss): `Button`, `Table`, `Dialog`, `Field`/`Form`,
  `ErrorBanner`, `Pager`, `CopyField` (for the SSH public key).
- **CSS Modules** per component; global `reset.css` + `tokens.css` (spacing, color, radius as
  CSS variables). **Dark theme only** — a single theme, no light mode or toggle.
- No client-side secrets or config — the SPA is same-origin and unconfigured; everything
  comes from the API.

## Open Questions

None currently open.
