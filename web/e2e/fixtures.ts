import { expect, test as base, type APIRequestContext } from "@playwright/test";

/**
 * Shared fixtures and helpers for the Layer 3 Playwright suite
 * (docs/testing-spec.md). This file, plus playwright.config.ts and the
 * compose stack + Taskfile target that boot the real server, is what the
 * sibling journey beads (loam-li0.11.2 credentials, .11.3 proposal
 * decision, .11.4 jobs) inherit: env-driven fixture data, the auth model,
 * and {@link observeSyncState}'s pattern for asserting a real, transient
 * backend state without a sleep.
 *
 * No custom Playwright fixtures are added here (`test`/`expect` are
 * re-exported as-is) -- the suite is small enough that plain env lookups
 * cover everything today. Add a fixture here, not per-spec, the day two
 * spec files need the same derived state.
 */

/** Reads a required `LOAM_E2E_*` variable, or fails with a specific, actionable reason. */
function requireEnv(name: string): string {
  const value = process.env[name];
  if (value === undefined || value === "") {
    throw new Error(
      `web/e2e: ${name} is required but not set. It is set by \`task test:e2e\` (Taskfile.yml) from the ` +
        "Forgejo repo it just seeded -- running a spec file directly (e.g. `npx playwright test`) without that " +
        "setup is a misuse this check exists to catch immediately, rather than as a confusing failure deep " +
        "inside a test.",
    );
  }
  return value;
}

/**
 * The env-driven fixture data every spec in this suite reads, all supplied
 * by `task test:e2e`'s seeding step (Taskfile.yml): a real Forgejo repo,
 * auto-initialized so it has a `HEAD`, created fresh for this run.
 */
export const e2eEnv = {
  /** The upstream URL the enroll form's "Upstream URL" field is filled with. */
  get upstreamUrl(): string {
    return requireEnv("LOAM_E2E_UPSTREAM_URL");
  },
  /**
   * The `<group>/<repo_name>` identifier `RepoAdminService.EnrollRepo`
   * derives from {@link upstreamUrl} (internal/handler/repoadmin/handler.go
   * -> deriveRepoIdentity: the URL's bare path, minus a leading slash and a
   * trailing `.git`) -- e.g. `e2eadmin/e2e-repo`. Two segments, matching
   * docs/web-frontend-spec.md's `<group>/<name>` routing note.
   */
  get repoIdentifier(): string {
    return requireEnv("LOAM_E2E_REPO_IDENTIFIER");
  },
  /** The seeded repo's default branch (Forgejo's `auto_init` default: `main`). */
  get targetBranch(): string {
    return requireEnv("LOAM_E2E_TARGET_BRANCH");
  },
  /**
   * The seeded Forgejo's bare `host:port` (e.g. "127.0.0.1:13000") -- the
   * same format the Credentials screen's own Host field hints an admin
   * should type for the default, https case ("github.com"). Deliberately
   * NOT the scheme-qualified form Taskfile.yml's step 5 hands to `demoenv
   * seed-credential` for enrollment (`http://127.0.0.1:13000`, matching
   * what `RepoAdminService.EnrollRepo` derives from the http:// upstream
   * URL, `internal/handler/repoadmin/handler.go`'s `forgeHostOf`) -- this
   * fixture exists precisely so credentials.e2e.ts can submit the
   * REALISTIC bare form to `SetUpstreamToken` and prove it still
   * validates against this stack's plaintext Forgejo (loam-4kz's
   * scheme-mismatch retry, internal/forge/forgejo.go's `ValidateToken`),
   * rather than relying on the scheme-qualified row step 5 seeds out of
   * band for enrollment's own sake. Added for loam-li0.11.2
   * (credentials.e2e.ts).
   */
  get forgejoHost(): string {
    return requireEnv("LOAM_E2E_FORGEJO_HOST");
  },
  /**
   * A real Forgejo access token for the seeded admin user, scope `all` --
   * the same token Taskfile.yml's step 5 already generates and uses to
   * seed both the credential row and the repo content.
   */
  get forgejoToken(): string {
    return requireEnv("LOAM_E2E_FORGEJO_TOKEN");
  },
};

export const test = base;
export { expect };

/**
 * Sets a host's upstream token by calling
 * `CredentialService.SetUpstreamToken` directly over a plain, bare `fetch()`
 * -- deliberately NOT through Playwright's `page`, `context`, or `request`
 * fixtures (loam-s6mh).
 *
 * Those three are all wired into the running test's trace recording, and
 * a real credential that passes through any of them shows up in a failure
 * artefact in triplicate, all confirmed directly by forcing a real
 * failure and grepping the result (see credentials.e2e.ts's own top-of-file
 * doc comment for the full repro): the action log
 * (`fill("<token>")`'s own argument), a DOM snapshot of the input's live
 * value (a `type="password"` mask is a rendering hint the snapshotter does
 * not honour -- the value is captured verbatim regardless), and the raw
 * body of any traced network request. A bare `fetch()` call has no
 * relationship to Playwright's Chromium instance or any of its
 * instrumented request contexts, so none of the three ever see it: this
 * function is how a spec proves a real token's REAL server-side effect
 * without ever putting the token where a trace/error-context capture could
 * find it.
 *
 * Uses the same admin Basic-auth pair playwright.config.ts hands the
 * browser via `use.httpCredentials` (env-sourced; see that file for why
 * the SPA requires it before any JS runs), reconstructed here because this
 * call bypasses the browser -- and its native credential prompt -- entirely.
 */
export async function setUpstreamTokenOutOfBand(host: string, token: string): Promise<{ validated: boolean }> {
  const baseURL = process.env["LOAM_E2E_BASE_URL"] ?? "http://127.0.0.1:8099";
  const adminUser = process.env["LOAM_E2E_ADMIN_USER"] ?? "admin";
  const adminPassword = requireEnv("LOAM_E2E_ADMIN_PASSWORD");
  const basicAuth = Buffer.from(`${adminUser}:${adminPassword}`).toString("base64");
  const res = await fetch(`${baseURL}/loam.admin.v1.CredentialService/SetUpstreamToken`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Basic ${basicAuth}` },
    body: JSON.stringify({ host, token }),
  });
  if (!res.ok) {
    throw new Error(
      `setUpstreamTokenOutOfBand: SetUpstreamToken for ${host} returned ${res.status} ${await res.text()}`,
    );
  }
  const body = (await res.json()) as { status?: { validated?: boolean } };
  return { validated: body.status?.validated ?? false };
}

/**
 * Polls `RepoAdminService.ListRepos` (over Connect's JSON protocol via a
 * plain POST + `Content-Type: application/json` -- connect-go registers
 * the JSON codec by default and cmd/server passes no HandlerOptions, the
 * same "no generated client needed" fact Taskfile.yml's own `admin_rpc`
 * curl helpers rely on) until `repo`'s `sync.state` equals `want`, or
 * `timeoutMs` elapses. Returns whether it was observed.
 *
 * This is the ONLY way to see `SYNC_STATE_SYNCING` for a newly enrolled
 * repo. `EnrollRepo` runs the initial clone synchronously inside one unary
 * RPC and itself advances `sync_state` back to `idle` before that RPC
 * returns (internal/handler/repoadmin/enroll.go's own doc comment: "sync_
 * state is Syncing for the clone+reconcile duration ... EnrollRepo only
 * returns success after ... sync_state has been advanced to Idle"). So by
 * the time the SPA's `EnrollRepo` mutation resolves and its `onSuccess`
 * closes the dialog, the state visible in any *subsequent* read is already
 * `idle` -- "syncing" is real but exists only for the clone's wall-clock
 * duration, concurrently with the in-flight request, never after it. The
 * enroll journey spec starts this poll before clicking submit for exactly
 * that reason.
 *
 * Uses Playwright's own `expect.poll` (a bounded, retrying assertion, not
 * a fixed delay) to satisfy this bead's "no arbitrary sleeps" rule -- the
 * same bounded-poll shape Taskfile.yml's own `wait_for_http_ok` helpers
 * already use for the equivalent "wait for a real, in-flight operation to
 * reach an observable state" problem.
 */
export async function observeSyncState(
  request: APIRequestContext,
  repo: string,
  want: string,
  timeoutMs = 15_000,
): Promise<boolean> {
  // Empirically measured while building this test (a raw curl loop against
  // the real running server, polling ListRepos every ~10ms across a real
  // EnrollRepo call against this bead's own seed fixture): the syncing
  // window this observes is genuinely real but only around 100-150ms
  // wide -- most of it consumed by EnrollRepo's preceding steps (the
  // authenticated CheckRepo read+write probe, the repos-row insert) that
  // run BEFORE sync_state is ever set to syncing at all. Callers should
  // issue one throwaway request over `request` before racing (see
  // ./enroll.e2e.ts) so Node/undici's one-time connection-establishment
  // cost lands outside the window this function is timing, not inside it.
  try {
    await expect
      .poll(
        async () => {
          const res = await request.post("/loam.admin.v1.RepoAdminService/ListRepos", { data: {} });
          if (!res.ok()) return "";
          const body = (await res.json()) as {
            repos?: readonly { repo?: string; sync?: { state?: string } }[];
          };
          const match = (body.repos ?? []).find((candidate) => candidate.repo === repo);
          return match?.sync?.state ?? "";
        },
        // Densely spaced at the start, on purpose: the window above is
        // ~100-150ms wide, so anything coarser than ~10-20ms between the
        // first several attempts risks straddling it entirely (observed
        // directly: intervals starting at 50ms missed the window outright
        // in one run). Widens after that once the window has certainly
        // either been caught or closed, so a slow CI host is not
        // hammered for the full timeout.
        { timeout: timeoutMs, intervals: [10, 10, 15, 15, 20, 25, 35, 50, 75, 100, 150, 250, 400] },
      )
      .toBe(want);
    return true;
  } catch {
    return false;
  }
}
