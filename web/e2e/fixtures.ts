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
};

export const test = base;
export { expect };

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
