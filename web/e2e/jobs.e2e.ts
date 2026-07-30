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
  test("reindexing an enrolled repo enqueues a job that is visible and runs to SUCCEEDED", async ({
    page,
  }) => {
    // Well above the default 30s (playwright.config.ts): the real
    // ingest.Pool worker wakes up immediately on enqueue (Pool.wakeUp,
    // internal/ingest/pool.go), but Jobs.tsx only ever learns the result via
    // its own 5s poll, and a real embed step (see loam-1dmg: task test:e2e
    // now boots cmd/demoenv embed, an Ollama-wire-shaped
    // internal/fakeembed server, and points LOAM_EMBEDDER_URL at it) takes
    // real wall-clock time on top of that poll interval.
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
    // sequential `expect()` calls. Both choices remain load-bearing even
    // now that the stack has a real embedder (loam-1dmg): ReindexRepo's
    // Enqueue only coalesces onto an existing QUEUED row
    // (internal/ingest/pool.go's own doc comment), so clicking Reindex
    // while ./enroll.e2e.ts's own enrollment-triggered FULL job for this
    // repo is anywhere past QUEUED -- RUNNING, or already terminal --
    // inserts a genuinely SEPARATE second row, not a reuse of the first.
    // `queued_at DESC` ordering (internal/ingest/list.go's ListJobs) is
    // therefore not a stable way to pick "the" row up front. IngestJob now
    // carries an id (loam-1wpa), but ReindexRepo's own response never gets
    // one back (Pool.Enqueue coalesces onto an existing queued row without
    // reporting which row it touched), so this spec still cannot name which
    // physical row is "the one Reindex enqueued" from the click alone --
    // only that a real FULL ingest job for this repo, triggered into
    // existence by this test's own click, genuinely reaches SUCCEEDED.
    // Requiring SUCCEEDED (not "SUCCEEDED or FAILED")
    // is loam-1dmg's whole point: task test:e2e now boots a real fake
    // embedder (cmd/demoenv embed) and points LOAM_EMBEDDER_URL at it, so
    // a FAILED terminal state here would mean the fix regressed, not that
    // the job merely "ran".
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
            // A terminal FAILED here (loam-1dmg) means the embedder fix
            // regressed, not that the job merely "ran" -- fail fast with
            // the row's own error text rather than let the poll time out
            // silently after 45s.
            if (kind === "Full" && status === "Failed") {
              const errorText = texts[8] ?? "(no error column)";
              throw new Error(`a Full ingest job for ${e2eEnv.repoIdentifier} reached FAILED, not SUCCEEDED: ${errorText}`);
            }
            if (kind === "Full" && status === "Succeeded" && started !== "—" && finished !== "—") {
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
