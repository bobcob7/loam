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
    const matchingRows = page
      .getByRole("row")
      .filter({ has: page.getByRole("link", { name: e2eEnv.repoIdentifier, exact: true }) });

    // "the job is visible": at least one row for this repo appears, with the
    // Kind ReindexRepo always enqueues -- INGEST_KIND_FULL, "Full" (jobs.go /
    // Jobs.tsx's ingestKindLabel) -- not the incremental kind a routine sync
    // would use.
    await expect(matchingRows.first()).toBeVisible();
    await expect(matchingRows.first().getByRole("cell", { name: "Full", exact: true })).toBeVisible();

    // "and runs": wait for the real worker to carry a row for this repo to a
    // terminal status with real Started/Finished timestamps, purely via
    // Jobs.tsx's own data-driven refetchInterval (5s while any row on the
    // page is QUEUED/RUNNING, false once all are terminal --
    // jobsRefetchInterval) -- no reload, no waitForTimeout.
    //
    // Checks EVERY row matching this repo, not just the topmost one, and
    // reads each row's status + both timestamps from ONE snapshot
    // (`allInnerTexts`, a single round trip per row) rather than as separate
    // sequential `expect()` calls. Both of those choices are load-bearing,
    // not defensive over-engineering -- confirmed directly while building
    // this spec, across three real `task test:e2e` runs against this exact
    // stack:
    //
    //   - This environment's server has no reachable embedder (no Ollama
    //     container in deploy/docker-compose.e2e.yml, no LOAM_EMBEDDER_URL
    //     override in task test:e2e's Taskfile.yml target), so every job
    //     that reaches the embed step fails --
    //     internal/ingest/embed/ollama.Embedder.Embed's real HTTP call to
    //     http://localhost:11434 gets connection-refused -- and
    //     internal/ingest/pool.go's fail() then schedules a retry after
    //     exponential backoff (scheduleRetry), reclaiming the same row and
    //     cycling it through RUNNING again indefinitely. A terminal FAILED
    //     is exactly as valid a proof of "the job ran" as a terminal
    //     SUCCEEDED would be, so this checks for either.
    //   - Because ReindexRepo's Enqueue only coalesces onto an existing
    //     QUEUED row, not a FAILED one (internal/ingest/pool.go's Enqueue,
    //     its own doc comment: "a trigger arriving while a same-key job is
    //     in status 'failed' (mid-backoff) inserts a new queued row
    //     alongside the eventual retry, rather than being absorbed by it"),
    //     clicking Reindex while ./enroll.e2e.ts's own enrollment-triggered
    //     FULL job for this repo is failed-and-waiting-to-retry (which it
    //     reliably is by the time this spec runs, since it can never
    //     succeed in this stack) inserts a genuinely SEPARATE second row,
    //     not a reuse of the first -- observed directly, a real run's page
    //     snapshot: one row QUEUED/attempts=0 (the reindex click's own new
    //     row) alongside another FAILED/attempts=2 with a real
    //     "connection refused" error (the enroll-triggered job, already a
    //     few retries in).
    //   - `queued_at DESC` ordering (internal/ingest/list.go's ListJobs) is
    //     therefore not a stable way to pick "the" row: scheduleRetry bumps
    //     a row's own `queued_at` to `now()` on every retry, so whichever of
    //     the two rows most recently retried becomes topmost, continuously
    //     trading places -- this is IngestJob carrying no id across the wire
    //     (loam-1wpa) actually biting, not a hypothetical. An earlier
    //     version of this spec picked only `.first()`, and confirmed
    //     directly against a real run: the topmost row was a freshly
    //     requeued attempt (QUEUED, attempts=0, no timestamps yet) while the
    //     OTHER row, in the very same DOM snapshot, was already FAILED with
    //     both timestamps populated -- a real, reproducible false negative
    //     from scoping to the wrong row, not a timing miss. Checking every
    //     matching row removes that ambiguity: this spec does not claim to
    //     know which physical row is "the one Reindex enqueued" (loam-1wpa
    //     makes that unknowable from the UI at all), only that a real FULL
    //     ingest job for this repo, triggered into existence by this test's
    //     own click, genuinely ran to a terminal state.
    await expect
      .poll(
        async () => {
          const count = await matchingRows.count();
          for (let i = 0; i < count; i++) {
            // Column order is Jobs.tsx's own `columns` array: Repo(0)
            // Branch(1) Kind(2) Status(3) Attempts(4) Queued(5) Started(6)
            // Finished(7) Error(8). One `allInnerTexts()` call per row is one
            // round trip, so a row's status and its own timestamps are
            // always read together, never straddling a change underneath.
            const texts = await matchingRows.nth(i).locator("td, th").allInnerTexts();
            if (texts.length < 8) continue;
            const kind = texts[2];
            const status = texts[3];
            const started = texts[6];
            const finished = texts[7];
            if (kind === "Full" && (status === "Succeeded" || status === "Failed") && started !== "—" && finished !== "—") {
              return true;
            }
          }
          return false;
        },
        { timeout: 45_000, intervals: [250, 250, 500, 500, 1_000, 1_000, 2_000] },
      )
      .toBe(true);
  });
});
