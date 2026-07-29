import { expect, test } from "./fixtures";

/**
 * Admin basic auth (docs/web-spec.md -> Hosting & Routing, Auth):
 * `RegisterSPA` mounts the embedded SPA behind `Auth.AdminOnly`
 * (internal/server/spa.go), so the DOCUMENT request itself draws a native
 * basic-auth challenge before any JS runs. Playwright's `page` fixture
 * cannot answer that dialog directly (it is not a page element), which is
 * exactly why playwright.config.ts supplies `httpCredentials` at the
 * context level instead -- proven by the third test below, which loads the
 * page with no per-test auth code at all.
 *
 * The first two tests deliberately bypass Playwright's own `page`/
 * `request` fixtures (which would send the configured httpCredentials
 * automatically) and use plain `fetch` instead, so they can assert the
 * *unauthenticated* and *wrong-credential* cases the config's
 * httpCredentials would otherwise mask.
 */

test.describe("admin basic auth", () => {
  test("an unauthenticated request is rejected 401 with the documented challenge", async ({ baseURL }) => {
    const res = await fetch(new URL("/", baseURL));
    expect(res.status).toBe(401);
    // docs/web-spec.md: "the admin username and password are provided on
    // server startup"; internal/httpauth's wwwAuthenticateRealm is
    // `Basic realm="loam"`, sent on every 401 so a browser prompts.
    expect(res.headers.get("www-authenticate")).toBe('Basic realm="loam"');
  });

  test("a wrong admin credential is rejected the same way, not silently downgraded", async ({ baseURL }) => {
    const wrongCredential = Buffer.from("admin:definitely-not-the-configured-password").toString("base64");
    const res = await fetch(new URL("/", baseURL), {
      headers: { Authorization: `Basic ${wrongCredential}` },
    });
    expect(res.status).toBe(401);
    expect(res.headers.get("www-authenticate")).toBe('Basic realm="loam"');
  });

  test("with valid credentials, the real admin console is served -- not the placeholder", async ({ page }) => {
    await page.goto("/");
    // web/index.html's real title, distinct from the placeholder's bare
    // "Loam" (web/dist/index.html's committed stand-in, web/embed.go).
    await expect(page).toHaveTitle("Loam Admin");
    await expect(page.getByRole("heading", { name: "Repos", level: 1 })).toBeVisible();
    // The five top-level screens (docs/web-spec.md -> Screens) that only
    // the real AppLayout nav renders; the placeholder has no navigation at
    // all, only a single paragraph.
    for (const label of ["Repos", "Credentials", "Roles", "Proposals", "Jobs"]) {
      await expect(page.getByRole("link", { name: label, exact: true })).toBeVisible();
    }
    // Belt and suspenders against the exact regression this bead calls out
    // (loam-m6hg): the placeholder's one line of body text, verbatim.
    await expect(page.locator("body")).not.toContainText("Loam admin interface — coming soon");
  });
});
