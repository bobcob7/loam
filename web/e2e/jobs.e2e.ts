import { e2eEnv, expect, test } from "./fixtures";

/**
 * The jobs journey (docs/testing-spec.md -> Layer 3 "Admin SPA (Playwright)":
 * "jobs (reindex -> job runs visible)"; docs/web-frontend-spec.md -> Routing
 * & Screens: `/jobs` Jobs). Depends on ./enroll.e2e.ts having already
 * enrolled `e2eEnv.repoIdentifier` (both this suite's `fullyParallel: false,
 * workers: 1` config and the two files' alphabetical run order guarantee
 * that): `ReindexRepo` 404s an unenrolled repo
 * (internal/handler/repoadmin/jobs.go), and enrolling it is that spec's own
 * job, not this one's.
 *
 * The central constraint (this bead's own text, docs/web-frontend-spec.md's
 * "jobsRefetchInterval"): Jobs.tsx polls ListIngestJobs every 5s while the
 * current page holds a QUEUED/RUNNING job, and stops once every job on it is
 * terminal. This spec proves that gate end to end -- reaching a terminal
 * state by watching the real background worker (internal/ingest.Pool),
 * never by reloading or sleeping. Every assertion below is Playwright
 * web-first (auto-retrying up to an explicit timeout).
 */
test.describe("jobs journey", () => {
  test("reindexing an enrolled repo enqueues a job that is visible and runs to a terminal state", async ({
    page,
  }) => {
    // Well above the default 30s (playwright.config.ts): the real
    // ingest.Pool worker wakes up immediately on enqueue (Pool.wakeUp,
    // internal/ingest/pool.go), but Jobs.tsx only ever learns the result via
    // its own 5s poll, and this environment's server (task test:e2e,
    // Taskfile.yml) is never given a reachable embedder -- no Ollama
    // container in deploy/docker-compose.e2e.yml, and no LOAM_EMBEDDER_URL
    // override in that task -- so the job's real, observed path here is
    // QUEUED -> RUNNING -> FAILED once ingestion reaches the embed step and
    // internal/ingest/embed/ollama.Embedder.Embed's real HTTP call to
    // http://localhost:11434 refuses the connection. That failure is a
    // genuine, deliberate consequence of this stack's fixture choices, not
    // a bug this spec works around: see this bead's final report. A
    // terminal FAILED is exactly as valid a proof of "the job ran" as a
    // terminal SUCCEEDED would be, so this spec asserts on either.
    test.setTimeout(60_000);

    await page.goto("/jobs");
    await expect(page.getByRole("heading", { name: "Jobs", level: 1 })).toBeVisible();

    const repoField = page.getByLabel("Repo");
    await repoField.fill(e2eEnv.repoIdentifier);

    // Disabled until a repo is typed (Jobs.tsx: `disabled={repoDraft.trim() === ""}`).
    const reindexButton = page.getByRole("button", { name: "Reindex repo" });
    await expect(reindexButton).toBeEnabled();
    await reindexButton.click();

    // handleReindex (Jobs.tsx) sets appliedRepo to this exact identifier as
    // part of the same click, so once the mutation's invalidation refetch
    // lands, the table is already filtered to this repo alone -- no
    // separate "apply filters" step needed for the reindex path.
    //
    // internal/ingest/list.go's ListJobs orders `queued_at DESC`, and this
    // click's Enqueue call always commits (or, if ./enroll.e2e.ts's own
    // enrollment-triggered FULL job for this repo/branch is still QUEUED,
    // coalesces onto the same still-latest row -- internal/ingest/pool.go's
    // Enqueue) no earlier than that prior job, so the topmost row matching
    // this repo is always THIS job -- never a guess keyed on the job id
    // IngestJob does not carry across the wire (loam-1wpa).
    const row = page
      .getByRole("row")
      .filter({ has: page.getByRole("link", { name: e2eEnv.repoIdentifier, exact: true }) })
      .first();

    // "the job is visible": it appears in the table at all, with the Kind
    // ReindexRepo always enqueues -- INGEST_KIND_FULL, "Full" (jobs.go /
    // Jobs.tsx's ingestKindLabel) -- not the incremental kind a routine sync
    // would use.
    await expect(row).toBeVisible();
    await expect(row.getByRole("cell", { name: "Full", exact: true })).toBeVisible();

    // "and runs": wait for the real worker to carry it to a terminal status
    // purely via Jobs.tsx's own data-driven refetchInterval -- no reload, no
    // waitForTimeout. The 45s budget here is CI-host slack, not a guess at
    // ingest's own wall-clock cost (a couple hundred small, unparsed files):
    // comfortably more than one poll cycle (5s) plus this suite's own
    // per-assertion default (10s) would allow, so a slow poll response
    // doesn't read as a false red.
    await expect(row.getByRole("cell", { name: /^(Succeeded|Failed)$/ })).toBeVisible({
      timeout: 45_000,
    });

    // Proof the job actually RAN rather than a UI artifact rendering a
    // terminal label over stale/default data: Started/Finished only ever
    // stop being formatTimestamp's "—" placeholder once ingest.Pool's real
    // claim (sets started_at) and terminal UPDATE (sets finished_at) have
    // each committed (internal/ingest/pool.go) -- both are set in the same
    // transaction as the very status this assertion already waited for, so
    // this is confirming the UI reflects that, not re-timing it. Column
    // order is Jobs.tsx's own `columns` array: Repo(0) Branch(1) Kind(2)
    // Status(3) Attempts(4) Queued(5) Started(6) Finished(7) Error(8).
    const cells = row.locator("td, th");
    await expect(cells.nth(6)).not.toHaveText("—");
    await expect(cells.nth(7)).not.toHaveText("—");
  });
});
