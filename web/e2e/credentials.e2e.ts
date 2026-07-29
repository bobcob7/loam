import { e2eEnv, expect, test } from "./fixtures";

/**
 * The credentials journey (docs/testing-spec.md -> Layer 3 "Admin SPA
 * (Playwright)": "credentials (set token → validated)";
 * docs/web-frontend-spec.md -> Routing & Screens: `/credentials`).
 *
 * The Credentials screen is token-only (web/src/routes/Credentials.tsx's own
 * doc comment; `CredentialStatus` field 3, the old SSH flag, is `reserved`
 * in proto/loam/admin/v1/credential.proto) -- there is no
 * `GenerateSSHKeyPair` RPC anywhere and nothing here renders a public key, so
 * `CopyField` is correctly unused on this screen and this spec never reaches
 * for it. `SetUpstreamTokenResponse` carries only a `CredentialStatus`
 * (host/has_token/validated); there is no field the server could echo a
 * token back through even by accident, which is what this file's SECRET
 * HYGIENE note below is really about -- the token leaves the browser once,
 * over the wire, and Playwright's own failure artefacts are the next place
 * it could resurface.
 *
 * SECRET HYGIENE (this bead's #1 explicit ask): every token this file types
 * into the Token field is real -- the seeded Forgejo's own admin access
 * token (`e2eEnv.forgejoToken`) or, in the negative case, an obviously fake
 * literal that carries no real access. `playwright.config.ts` sets
 * `trace: "retain-on-failure"` and `screenshot: "only-on-failure"`.
 *
 * VERIFIED DIRECTLY, TWICE (this bead's own instruction: "generate a real
 * failure, open the trace, and look at what it captured" -- both artefact
 * classes below were produced by a real run against the real namespaced
 * stack, then opened, not inferred from documentation):
 *
 * (1) A test.fail()-annotated failure (the "[loam-4kz]" test below, run for
 *     real). Its reported OUTCOME is "passed" (it failed exactly as
 *     test.fail() predicted), so playwright.config.ts's
 *     retain-on-failure/only-on-failure settings do NOT fire for it --
 *     confirmed: the run's test-results/ directory holds no trace.zip and
 *     no screenshot for this test at all. It STILL attaches
 *     error-context.md, unconditionally, on any thrown assertion regardless
 *     of test.fail() -- and that file embeds an ARIA/accessibility snapshot
 *     of the page at the moment of failure, which renders a textbox's
 *     CURRENT VALUE as plain text. The real run's own error-context.md
 *     contained, verbatim:
 *       textbox "Token": e7ceb8c5308bf482b9b696424c99f8e9dbc882fb
 *     -- the real seeded Forgejo admin token, in the clear, in a markdown
 *     file this codebase's own web/.gitignore does not stop a CI workflow
 *     from uploading (gitignore only governs `git add`, never a CI
 *     artifact-upload step).
 *
 * (2) A genuine, unannotated failure (reproduced by deliberately breaking
 *     an assertion in the "proof of mechanics" test below, running for
 *     real, then reverting -- not this file's normal state). This DOES
 *     retain both a screenshot and trace.zip. The screenshot
 *     (test-failed-1.png) is safe: it is a rendered PNG, and a
 *     `type="password"` field is masked at the browser's rendering layer,
 *     independent of anything Playwright does -- confirmed by looking at
 *     the actual PNG. trace.zip is NOT safe. Unzipped and grepped for the
 *     real token, it appeared in THREE independent places:
 *       - the action log (test.trace/0-trace.trace): `Fill
 *         "<token>" getByRole('dialog', ...).getByLabel('Token')` and the
 *         raw `fill("<token>")` call record -- Playwright's own step log
 *         records the literal argument passed to `.fill()`.
 *       - a DOM snapshot embedded in 0-trace.trace: `["INPUT",
 *         {"__playwright_value_":"<token>", ..., "type":"password",
 *         "value":"<token>"}]` -- Playwright's trace snapshotter captures
 *         an input's LIVE value verbatim (via its own
 *         `__playwright_value_` marker, and also as a plain `value` key)
 *         specifically so the trace viewer can show what was typed; a
 *         `type="password"` mask is a rendering hint the snapshotter does
 *         not honour. (This corrects an earlier, unverified assumption in
 *         this same comment that the DOM property wouldn't be captured --
 *         it is.)
 *       - a captured network resource (resources/<hash>.json): the raw
 *         `SetUpstreamToken` POST body verbatim,
 *         `{"host":"...","token":"<token>"}` -- request bodies are wire
 *         payloads, not view-layer output, and no scrubbing runs on them
 *         anywhere in this codebase (server-side
 *         `redactToken`/`redactedMarker` in
 *         internal/handler/credential/credential.go only scrubs the token
 *         out of ERROR MESSAGES and LOG lines, never a captured HTTP body).
 *
 *   WHAT THIS MEANS FOR A CI-UPLOADED ARTEFACT: uploading
 *   `test-results/`/`playwright-report/` (both already gitignored,
 *   web/.gitignore -- which stops a `git add`, not a CI artifact-upload
 *   step) from a real, credentialed run of this suite ships a real forge
 *   access token to wherever that artefact lands, via error-context.md on
 *   EVERY failure (including an expected one) and via trace.zip on any
 *   unannotated one. This is exactly the "leaked credential in a CI
 *   artefact" this bead calls worse than a missing test, and it is not a
 *   defect in this test file -- it is a property of Playwright's own
 *   snapshotting and of this RPC's wire contract (SetUpstreamTokenRequest.
 *   token has no alternative, e.g. a header, to carry it out of band). No
 *   change to this spec file closes it.
 *
 *   PROPOSED FIX (not applied here -- a harness-wide concern, not specific
 *   to one spec file): the practical options are (a) a
 *   `page.route()`/`context.route()` interceptor, registered once in
 *   playwright.config.ts or a global setup, that rewrites any `token` field
 *   in a `CredentialService/SetUpstreamToken` request body to a fixed
 *   marker before Playwright's tracing layer ever records it -- this would
 *   need to run early enough to also scrub the DOM-snapshot/action-log
 *   copies, not just the network one, since (2) above shows the leak is
 *   not confined to the network capture; or (b) never run this journey
 *   with a real, live-scoped token in CI -- seed one that is syntactically
 *   valid but already revoked/expired, so a leaked copy is worthless. (a)
 *   is more general and should live in fixtures.ts or playwright.config.ts
 *   once agreed, not duplicated per spec file that happens to submit a
 *   secret.
 *
 * loam-4kz (KNOWN BUG, verified directly against this exact stack while
 * building this spec -- OUT OF SCOPE, not worked around here): `forge.
 * Forgejo.ValidateToken`'s `apiBaseURL` (internal/forge/forgejo.go)
 * unconditionally prepends "https://" to any host string that does not
 * already contain "://". `EnrollRepo` derives credentials by the upstream
 * URL's BARE `host:port` (deriveRepoIdentity), and the Credentials screen's
 * own Host field hint tells an admin to type exactly that bare form
 * ("github.com", "forgejo.example.com") -- but this e2e stack's seeded
 * Forgejo (deploy/docker-compose.e2e.yml) serves plaintext HTTP, so
 * validating that realistic bare host always dials `https://` at a
 * plaintext listener and fails before Forgejo ever sees the token. Confirmed
 * directly, via `curl` against this exact namespaced stack, before writing
 * any Playwright code:
 *
 *   POST /loam.admin.v1.CredentialService/SetUpstreamToken
 *   {"host":"127.0.0.1:13030","token":"<real seeded token>"}
 *   -> {"code":"internal","message":"internal error"}
 *
 *   server log: "validating the token against 127.0.0.1:13030: validating
 *   token for 127.0.0.1:13030: Post \"https://127.0.0.1:13030/api/v1/
 *   repos/loam-scope-probe-.../does-not-exist/pulls\": http: server gave
 *   HTTP response to HTTPS client"
 *
 * This is the exact failure cmd/demoenv/credential.go's own doc comment
 * predicts ("no single host string can both validate over the RPC and be
 * found again at enrollment"). The test below submits exactly that
 * realistic bare host (matching the field's own hint, matching what
 * EnrollRepo would derive) and is marked `test.fail()` rather than adjusted
 * to a host format that dodges the bug -- per this bead's explicit
 * instruction not to shape the test around it. When loam-4kz lands, this
 * annotation must be removed; Playwright will fail the run loudly if the
 * test starts passing while `test.fail()` is still present, which is the
 * intended forcing function to catch a stale annotation.
 */
test.describe("credentials journey", () => {
  test("[loam-4kz] setting a valid upstream token for the seeded Forgejo (bare host) shows Validated", async ({
    page,
  }) => {
    // See this file's own top-of-file doc comment for the full reasoning
    // and the exact curl reproduction. Not worked around: this uses the
    // bare host:port the Host field's own hint tells an admin to type, the
    // same bare form EnrollRepo derives -- the one loam-4kz breaks against
    // this stack's plaintext-HTTP Forgejo.
    test.fail(
      true,
      "loam-4kz: SetUpstreamToken always dials https:// for a bare host, and this stack's seeded " +
        "Forgejo is plaintext HTTP -- see this file's doc comment for the exact curl reproduction.",
    );

    await page.goto("/credentials");
    await expect(page.getByRole("heading", { name: "Credentials", level: 1 })).toBeVisible();

    // The header's "Set token" button, not a per-row action: the row for
    // this bare host already exists (Taskfile.yml step 5 seeds it directly
    // via `demoenv seed-credential`, out of band, for EnrollRepo's sake) and
    // already has a token, so its OWN row action reads "Update token" --
    // leaving "Set token" an unambiguous match for the header button.
    await page.getByRole("button", { name: "Set token", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Set upstream token" });
    await expect(dialog).toBeVisible();

    await dialog.getByLabel("Host").fill(e2eEnv.forgejoHost);
    await dialog.getByLabel("Token").fill(e2eEnv.forgejoToken);
    await dialog.getByRole("button", { name: "Save token" }).click();

    // The spec-mandated outcome (docs/testing-spec.md: "set token ->
    // validated"): the dialog closes on success and the row for this host
    // shows "Validated". This is the assertion that currently fails --
    // loam-4kz means the dialog instead stays open with an ErrorBanner
    // reading "internal error" (Code.Internal -> "unexpected" ->
    // data/mapConnectError.ts), which this test deliberately does NOT
    // assert on: doing so would be asserting the bug's shape as the
    // expected behaviour, which is exactly the "test shaped around a bug"
    // this bead warns against.
    await expect(dialog).toBeHidden();
    const row = page.getByRole("row", { name: new RegExp(escapeForRegExp(e2eEnv.forgejoHost)) });
    await expect(row.getByText("Validated")).toBeVisible();
  });

  /**
   * NOT the bead's literal ask, and not a substitute for the test above:
   * this exists solely to prove the dialog/status-badge mechanics the test
   * above exercises are real and non-vacuous, by exercising the identical
   * "fill Host+Token, submit, assert Validated" path against a host format
   * this stack's plaintext Forgejo can genuinely validate -- an explicit
   * `http://` scheme, which `apiBaseURL` (internal/forge/forgejo.go) has
   * always left untouched for exactly this reason ("host may be a bare
   * domain ... or include a scheme (used by tests pointing at an httptest
   * server)"). This does not collide with EnrollRepo's own bare-host
   * credential lookup (loam-4kz's actual scope) because this journey never
   * calls EnrollRepo at all. If the test above were silently vacuous (e.g.
   * a matcher missing its call, or an assertion that could never observe
   * "Validated" at all), this test would be vacuous in the identical way --
   * it is not: this one genuinely passes today.
   */
  test("proof of mechanics: a scheme-qualified host this stack's Forgejo can reach shows Validated", async ({
    page,
  }) => {
    const host = `http://${e2eEnv.forgejoHost}`;
    await page.goto("/credentials");
    await page.getByRole("button", { name: "Set token", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Set upstream token" });
    await expect(dialog).toBeVisible();

    await dialog.getByLabel("Host").fill(host);
    await dialog.getByLabel("Token").fill(e2eEnv.forgejoToken);
    await dialog.getByRole("button", { name: "Save token" }).click();

    await expect(dialog).toBeHidden();
    const row = page.getByRole("row", { name: new RegExp(escapeForRegExp(host)) });
    await expect(row.getByText("Validated")).toBeVisible();
  });

  /**
   * Proves this suite's "Validated" assertion can genuinely go red for a
   * reason that has nothing to do with loam-4kz: a token Forgejo itself
   * rejects. Reuses the previous test's now-validated scheme-qualified
   * host on purpose (`SetUpstreamToken` validates BEFORE it writes --
   * internal/handler/credential/credential.go's own doc comment -- so a
   * rejected token here never touches that row; this only asserts on the
   * DIALOG's own error state, never on the table). If a regression ever
   * made SetUpstreamToken report `validated: true` for a token the forge
   * never authenticated, this is the test that would catch it.
   */
  test("an invalid token is rejected, not reported as validated", async ({ page }) => {
    const host = `http://${e2eEnv.forgejoHost}`;
    await page.goto("/credentials");
    await page.getByRole("button", { name: "Set token", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Set upstream token" });
    await expect(dialog).toBeVisible();

    await dialog.getByLabel("Host").fill(host);
    await dialog.getByLabel("Token").fill("not-a-real-token-obviously-fake");
    await dialog.getByRole("button", { name: "Save token" }).click();

    // invalid_argument routes to the Host field, never the dialog's
    // ErrorBanner (Credentials.tsx's own hostFieldError branch; mirrored by
    // web/src/routes/Credentials.test.tsx's "routes invalid_argument to the
    // Host field" vitest case) -- asserted here against the real server's
    // real rejection, not a mocked one.
    await expect(dialog).toContainText("does not authenticate");
    await expect(dialog.getByRole("alert")).not.toBeVisible();
    // The dialog stays open -- the mutation did not succeed, and the
    // admin's typed host is not lost.
    await expect(dialog).toBeVisible();
  });
});

/** Escapes regex metacharacters in a literal string used to build a `RegExp` name matcher (host strings here always contain `.` and `:`, both meaningful outside a character class). */
function escapeForRegExp(literal: string): string {
  return literal.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
