import { defineConfig, devices } from "@playwright/test";

/**
 * Layer 3 harness config (docs/testing-spec.md -> "Layer 3 — End-to-End";
 * docs/web-frontend-spec.md; loam-li0.11.1). Runs the admin-journey suite
 * under web/e2e against the REAL server binary (embedded SPA) + Postgres +
 * a seeded real Forgejo, brought up by deploy/docker-compose.e2e.yml and
 * driven end to end by `task test:e2e` (Taskfile.yml) -- never against
 * `vite dev` and never with a mocked backend. This is the nightly stage
 * per docs/testing-spec.md's CI Stages table, not the per-PR gate.
 *
 * Chromium only, per this bead's scope ("chromium only, to start"); adding
 * a browser project later is additive here, not a rewrite.
 *
 * Test files are named `*.e2e.ts` (see testMatch) rather than the
 * `*.spec.ts`/`*.test.ts` vitest's default `include` glob already matches
 * project-wide (vite.config.ts) -- this keeps the two runners' file sets
 * disjoint without editing vite.config.ts's test config at all: vitest
 * would otherwise try to import files that call @playwright/test's `test`
 * outside its own runner and fail with a confusing "test() cannot be
 * called here" error.
 */
const baseURL = process.env["LOAM_E2E_BASE_URL"] ?? "http://127.0.0.1:8099";
const adminUser = process.env["LOAM_E2E_ADMIN_USER"] ?? "admin";
const adminPassword = process.env["LOAM_E2E_ADMIN_PASSWORD"];

if (adminPassword === undefined || adminPassword === "") {
  // Fails at config-load time, before any test runs, with a specific
  // reason -- not a generic connection-refused once the first test tries
  // to load a page. The SPA is behind HTTP Basic auth
  // (docs/web-spec.md -> Hosting & Routing) and a browser's native
  // credential prompt cannot be typed into by Playwright (it is not a page
  // element -- see web/e2e/admin-auth.e2e.ts), so `use.httpCredentials`
  // below is the only way in, and it has to be handed the exact password
  // the running server was booted with. `task test:e2e` sets both from one
  // generated value; running `npx playwright test` directly without it is
  // a misuse this message is meant to catch immediately.
  throw new Error(
    "web/playwright.config.ts: LOAM_E2E_ADMIN_PASSWORD is required. Run this suite via `task test:e2e` " +
      "(Taskfile.yml), which brings up deploy/docker-compose.e2e.yml, boots the real server binary against it, " +
      "and sets LOAM_E2E_* from that boot.",
  );
}

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.e2e.ts",
  // One shared server and one shared seeded Forgejo repo back every test in
  // this suite (Layer 3 is intentionally not per-test-isolated the way
  // Layers 1-2 are -- docs/testing-spec.md), so tests run serially rather
  // than risking two specs racing the same enrolled-repo state.
  fullyParallel: false,
  workers: 1,
  forbidOnly: process.env["CI"] !== undefined,
  retries: process.env["CI"] !== undefined ? 1 : 0,
  timeout: 30_000,
  expect: {
    // Every assertion in this suite is web-first (auto-retrying) with this
    // explicit timeout; nothing calls waitForTimeout or sleeps a guessed
    // duration (this bead's "no arbitrary sleeps" rule).
    timeout: 10_000,
  },
  reporter: process.env["CI"] !== undefined ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL,
    // The document request itself draws a native Basic-auth prompt
    // (RegisterSPA mounts the SPA behind Auth.AdminOnly, docs/web-spec.md),
    // before any JS runs -- Playwright cannot type into that dialog, so
    // credentials must be supplied here rather than filled into a page.
    httpCredentials: { username: adminUser, password: adminPassword },
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "off",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
