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

    // "and runs": wait for the real worker to carry SOME row for this repo
    // to a terminal status with real Started/Finished timestamps, purely
    // via Jobs.tsx's own data-driven refetchInterval (5s while any row on
    // the page is QUEUED/RUNNING, false once all are terminal --
    // jobsRefetchInterval) -- no reload, no waitForTimeout.
    //
    // This environment's server (task test:e2e, Taskfile.yml) has no
    // reachable embedder: no Ollama container in
    // deploy/docker-compose.e2e.yml, no LOAM_EMBEDDER_URL override in that
    // task. So the job's real path here is QUEUED -> RUNNING -> FAILED once
    // ingestion reaches the embed step and
    // internal/ingest/embed/ollama.Embedder.Embed's real HTTP call to
    // http://localhost:11434 refuses the connection -- and internal/ingest/
    // pool.go's fail() then schedules a retry after ~1s exponential backoff
    // (scheduleRetry), reclaiming the SAME row and cycling it back through
    // RUNNING indefinitely, since nothing in this stack ever makes the
    // embedder reachable. A terminal FAILED is exactly as valid a proof of
    // "the job ran" as a terminal SUCCEEDED would be, so this checks for
    // either -- but it must read status and both timestamps from ONE
    // snapshot of the row (`allInnerTexts`, a single round trip), not as
    // separate sequential `expect()` calls: internal/ingest/pool.go's
    // fail()/succeed() always write a job's terminal status and its
    // finished_at in the same transaction, so any single ListIngestJobs
    // response that shows one is terminal always shows the other too, but
    // an earlier version of this spec split that across three independent
    // `expect()` calls, each re-resolving the row on its own -- between
    // them, this stack's own retry had already reclaimed the row (resetting
    // started_at, clearing the terminal-ness the first `expect()` had just
    // caught), so the later checks observed a fresh, still-running attempt
    // instead of the one just proven terminal. That was a reproducible
    // flake in this spec's own design (confirmed directly: 3 consecutive
    // `task test:e2e` runs, this test failed on the 3rd with exactly that
    // shape), fixed here structurally by making the whole check atomic,
    // not by widening a timeout.
    await expect
      .poll(
        async () => {
          const candidate = page
            .getByRole("row")
            .filter({ has: page.getByRole("link", { name: e2eEnv.repoIdentifier, exact: true }) })
            .first();
          if ((await candidate.count()) === 0) return false;
          // Column order is Jobs.tsx's own `columns` array: Repo(0)
          // Branch(1) Kind(2) Status(3) Attempts(4) Queued(5) Started(6)
          // Finished(7) Error(8).
          const texts = await candidate.locator("td, th").allInnerTexts();
          if (texts.length < 8) return false;
          const kind = texts[2];
          const status = texts[3];
          const started = texts[6];
          const finished = texts[7];
          return (
            kind === "Full" &&
            (status === "Succeeded" || status === "Failed") &&
            started !== "—" &&
            finished !== "—"
          );
        },
        { timeout: 45_000, intervals: [250, 250, 500, 500, 1_000, 1_000, 2_000] },
      )
      .toBe(true);
  });
});
