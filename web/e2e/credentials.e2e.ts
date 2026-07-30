import { e2eEnv, expect, setUpstreamTokenOutOfBand, test } from "./fixtures";

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
 *   FIX APPLIED (loam-s6mh): the real token this file uses is now never
 *   typed into the browser at all.
 *     - The "proof of mechanics" test below sets the real token via
 *       {@link setUpstreamTokenOutOfBand} (./fixtures.ts) -- a bare
 *       `fetch()` call with no relationship to any Playwright-instrumented
 *       request context, so none of the three capture points above ever
 *       see it -- then only loads the page and asserts the rendered
 *       "Validated" text. This still proves the assertion is real (a real
 *       server verdict for a real token, rendered by the real UI) without
 *       putting the token where a trace/error-context capture could find
 *       it.
 *     - The "[loam-4kz]" test below never depended on the token's VALUE in
 *       the first place -- the bug it demonstrates is a host-format defect
 *       (`apiBaseURL` dialing `https://` at a plaintext listener) that
 *       fails at the network/dial layer before Forgejo ever inspects the
 *       Authorization header (see this file's own curl repro above: the
 *       server log shows a TLS handshake failure, not an auth rejection).
 *       So it now fills the Token field with a non-secret placeholder
 *       (`nonSecretProbeToken` below) instead of the real one -- identical
 *       failure, identical assertions, zero credential value entering the
 *       DOM.
 *   Broader-than-this-file options considered and set aside: (a) a
 *   `page.route()`/`context.route()` interceptor would need to scrub the
 *   DOM-snapshot/action-log copies too, not just the network one, since (2)
 *   above shows the leak is not confined to the network capture -- more
 *   general, but more fragile, and unnecessary once nothing traced ever
 *   carries the real value; (c) scoping the seeded token to a narrower,
 *   throwaway-per-run grant and revoking it in `task test:e2e`'s teardown
 *   is applied at the harness level (Taskfile.yml's `test:e2e`), not here --
 *   defence-in-depth regardless of this file's own fix, since it bounds
 *   what a leaked artefact from ANY spec could do, not just this journey's.
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
/**
 * Filled into the "[loam-4kz]" test's Token field below in place of a real
 * credential (loam-s6mh). That test's bug is a host-format defect --
 * `apiBaseURL` dials `https://` at a plaintext listener before Forgejo
 * ever reads the Authorization header -- so no token value, real or fake,
 * changes its outcome; see this file's own top-of-file "FIX APPLIED" note
 * for the full reasoning.
 */
const nonSecretProbeToken = "loam-s6mh-non-secret-probe-token";

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
    // loam-s6mh: a non-secret placeholder, not e2eEnv.forgejoToken -- see
    // this file's own top-of-file "FIX APPLIED" note. The bug this test
    // demonstrates fails at the dial layer over the HOST format alone
    // (apiBaseURL forces https:// at a plaintext listener), before Forgejo
    // ever reads the Authorization header, so the token's value cannot
    // change this test's outcome either way; there is no reason for a real
    // credential to be the one typed into this field, and every reason not
    // to (test.fail() means this test's failure is EXPECTED, i.e. its own
    // artefacts, including error-context.md, are produced on every run).
    await dialog.getByLabel("Token").fill(nonSecretProbeToken);
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
   * this exists solely to prove the status-badge mechanics the test above
   * exercises are real and non-vacuous, against a host format this stack's
   * plaintext Forgejo can genuinely validate -- an explicit `http://`
   * scheme, which `apiBaseURL` (internal/forge/forgejo.go) has always left
   * untouched for exactly this reason ("host may be a bare domain ... or
   * include a scheme (used by tests pointing at an httptest server)").
   * This does not collide with EnrollRepo's own bare-host credential
   * lookup (loam-4kz's actual scope) because this journey never calls
   * EnrollRepo at all. If the "Validated" render were silently vacuous
   * (e.g. a matcher that could never observe it at all), this test would
   * be vacuous in the identical way -- it is not: this one genuinely
   * passes today.
   *
   * loam-s6mh: sets the token via {@link setUpstreamTokenOutOfBand}
   * rather than through the dialog's Token field (see this file's own
   * top-of-file "FIX APPLIED" note) -- this proves the RPC-to-render path
   * end to end (real backend verdict -> real UI render) without the
   * dialog's own submit affordance being part of what's under test here;
   * the dialog's fill-and-submit mechanics for a REJECTED token are still
   * covered, with a real browser interaction, by the next test below.
   */
  test("proof of mechanics: a scheme-qualified host this stack's Forgejo can reach shows Validated", async ({
    page,
  }) => {
    const host = `http://${e2eEnv.forgejoHost}`;
    // loam-s6mh: the real token is set directly against the real server
    // (setUpstreamTokenOutOfBand, ./fixtures.ts) rather than typed into
    // the dialog's Token field -- see this file's own top-of-file "FIX
    // APPLIED" note. The RPC's own response is asserted first, so this
    // still proves the backend genuinely validated a real token before
    // the UI is asked to do anything at all.
    const result = await setUpstreamTokenOutOfBand(host, e2eEnv.forgejoToken);
    expect(result.validated).toBe(true);

    // The remainder is exactly the assertion this test exists for
    // (docs comment above): a fresh page load renders that real,
    // server-verified state as "Validated" -- proving the row/badge this
    // suite's other assertions rely on reflects reality rather than
    // being vacuously true. Nothing here is stubbed, mocked, or asserted
    // against anything this test itself set up in the DOM.
    await page.goto("/credentials");
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
