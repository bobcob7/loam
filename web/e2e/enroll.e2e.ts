import { e2eEnv, expect, observeSyncState, test } from "./fixtures";

/**
 * The enroll journey (docs/testing-spec.md -> Layer 3 "Admin SPA
 * (Playwright)": "enroll (form -> repo listed, syncing)";
 * docs/web-frontend-spec.md -> Routing & Screens: `/` Repos). This is the
 * one journey in this bead's scope; it exists to prove the harness end to
 * end, not just that it is present.
 *
 * Against the real seeded Forgejo (deploy/docker-compose.e2e.yml), the
 * upstream is reachable and has a real `HEAD`, so `ProbeRepo` succeeds and
 * pre-fills the indexed-branch picker exactly as it would for a live
 * admin -- nothing here works around the probe.
 */
test.describe("enroll journey", () => {
  test("filling the enroll form lists the repo and (transiently) shows it syncing", async ({ page, request }) => {
    // Warms the `request` fixture's keep-alive connection well before the
    // race below starts, so its one-time TCP/TLS-handshake cost (paid on
    // whichever request happens to go first) lands here, not inside the
    // ~100-150ms syncing window ./fixtures.ts's observeSyncState times.
    await request.post("/loam.admin.v1.RepoAdminService/ListRepos", { data: {} });

    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Repos", level: 1 })).toBeVisible();

    await page.getByRole("button", { name: "Enroll repo" }).click();
    const dialog = page.getByRole("dialog", { name: "Enroll a repo" });
    await expect(dialog).toBeVisible();

    const upstreamField = dialog.getByLabel("Upstream URL");
    await upstreamField.fill(e2eEnv.upstreamUrl);
    // EnrollDialog wires ProbeRepo to the field's onBlur, not onChange
    // (web/src/routes/Repos.tsx); Tab is a real blur, not a synthetic
    // event dispatch.
    await upstreamField.press("Tab");

    // ProbeRepo's response pre-fills indexedBranch from the upstream's
    // real HEAD (the seeded repo's auto_init default branch) and adds it
    // to targetBranches, so no manual branch entry is needed here -- this
    // is exercising the form's designed pre-fill path, not working around
    // it. A web-first assertion (auto-retrying up to the configured
    // timeout), not a sleep: ProbeRepo is a real network round trip to the
    // real Forgejo container.
    const indexedBranch = dialog.getByLabel("Indexed branch");
    await expect(indexedBranch).toHaveValue(e2eEnv.targetBranch);

    const submit = dialog.getByRole("button", { name: "Enroll", exact: true });
    await expect(submit).toBeEnabled();

    // Start the race BEFORE clicking submit: EnrollRepo runs the initial
    // clone synchronously inside one RPC and only returns after sync_state
    // is already back to idle (see ./fixtures.ts's observeSyncState doc
    // comment for the full reasoning), so "syncing" must be caught while
    // the click's request is still in flight, never after. This poll and
    // the click below run concurrently against the real server.
    const syncingObserved = observeSyncState(request, e2eEnv.repoIdentifier, "SYNC_STATE_SYNCING");
    await submit.click();
    expect(
      await syncingObserved,
      "never observed SYNC_STATE_SYNCING while EnrollRepo's clone was in flight -- either the clone completed " +
        "faster than this suite could observe it, or enrollment never reached the syncing state at all",
    ).toBe(true);

    await expect(dialog).toBeHidden();

    const row = page
      .getByRole("row")
      .filter({ has: page.getByRole("link", { name: e2eEnv.repoIdentifier, exact: true }) });
    await expect(row).toBeVisible();
    // The final, settled state once EnrollRepo has returned successfully
    // (SyncState.IDLE -> "Idle", web/src/components/statusIntent.ts).
    await expect(row.getByText("Idle")).toBeVisible();
  });
});
