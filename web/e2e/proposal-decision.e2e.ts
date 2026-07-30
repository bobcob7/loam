import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { e2eEnv, expect, test } from "./fixtures";

/**
 * The proposal-decision journey (docs/testing-spec.md -> Layer 3 "Admin SPA
 * (Playwright)": "proposal decision (view diff/verdicts -> accept -> PR URL
 * shown)"; loam-li0.11.3). THE HEAVIEST of the four admin journeys: unlike
 * ./enroll.e2e.ts, which only needs the seeded Forgejo repo itself, this one
 * needs a REVIEWED work branch carrying a real diff and a recorded approve
 * verdict before the SPA journey can even start.
 *
 * FIXTURE SETUP DRIVES THE REAL BINARIES, NOT ROWS. `test.beforeAll` below
 * shells out to the real, already-built `bin/loam` (task test:e2e's own
 * `task build:bin` step puts it at the repo root) as three separate agent
 * identities, and to plain `git`, exactly the way cmd/demoenv and
 * Taskfile.yml's demo:m4/demo:m5 targets already do: clone the enrolled
 * repo, start a work branch, commit and push a real file, request review,
 * and submit a verdict. Nothing here writes a database row directly --
 * Layer 3 exists to prove the shipped artifacts compose (docs/testing-spec.md),
 * and a fixture that bypassed the CLI/RPC surface would defeat that.
 *
 * WHY THIS FILE NEVER CALLS EnrollRepo ITSELF. playwright.config.ts's own
 * doc comment records that this suite is NOT per-test isolated: one shared
 * server and one shared seeded Forgejo repo back every spec, and
 * ./fixtures.ts's header names ./enroll.e2e.ts as the ONE journey in ITS
 * bead's scope, implying every sibling journey (credentials, this one,
 * jobs) inherits the repo enroll.e2e.ts's own browser journey enrolls.
 * Enrolling a second time here -- even defensively, only when absent --
 * would race that spec file: with `workers: 1` there is no concurrent
 * execution to race, but if some future file ordering ever ran this file
 * BEFORE enroll.e2e.ts, a self-heal enroll here would make enroll.e2e.ts's
 * own EnrollRepo call fail with "already enrolled" and break a spec this
 * bead does not own. So this file only ASSERTS the repo is enrolled
 * (bounded poll, failing loudly with a specific reason if it never is) and
 * never creates the enrollment itself.
 *
 * TWO WORK BRANCHES, NOT ONE, FOR THE SAME REASON demo:m5's TWO WORK
 * BRANCHES EXIST: one property cannot show two shapes. `approveBranch`
 * carries exactly one current-round, non-stale approve and is the happy
 * path: view its diff and verdicts, click Accept, see the PR URL.
 * `staleBranch` is the CORRECTNESS CONSTRAINT this bead calls out
 * (proto/loam/v1/common.proto:103, loam-2xe6): its round-1 approve is
 * marked stale by a second request-review, and round 2 is closed out by a
 * SECOND reviewer's NEUTRAL verdict (not another approve) -- reviewer A's
 * row renders "Approve (stale)" in the Verdicts table, and Accept must
 * still refuse. A journey that seeded a stale approve and expected accept
 * to succeed would be asserting the loam-2xe6 bug, not covering it. Two
 * DIFFERENT reviewer identities matter here: WorkBranchService.ListVerdicts
 * keeps only each reviewer's LATEST round (dedupeLatestPerReviewer,
 * internal/handler/workbranch/review.go), so if the SAME reviewer voted in
 * both rounds their round-1 approve would never appear in the response at
 * all, and the misleading "(stale)" pill this bead exists to guard would
 * have nothing to render.
 *
 * `staleBranch` never appears in the `/proposals` queue -- ListProposals
 * only returns a reviewed branch with >= 1 NON-stale approve
 * (web/src/routes/Proposals.tsx's own ProposalRow doc comment) -- so its
 * test navigates straight to the proposal-detail URL rather than clicking a
 * row, which is itself realistic: an admin who followed a stale link or
 * bookmark reaches exactly this page.
 *
 * THE ROUTE HAS THREE SEGMENTS. An enrolled repo identifier is
 * "<group>/<name>" (proto/loam/v1/common.proto) and spans two path segments
 * (web/src/routes/paths.ts), so every URL built here is
 * `/proposals/${e2eEnv.repoIdentifier}/${workBranch}` -- never a
 * hand-assembled two-segment guess.
 *
 * NO fixtures.ts OR Taskfile.yml CHANGE. Everything this journey needs
 * beyond ./fixtures.ts's existing exports (e2eEnv, expect, test) is local to
 * this file: the loam/git driving, the admin-RPC enrollment check, the two
 * work-branch identities. Sibling beads (credentials, jobs) edit web/e2e/
 * concurrently, and the harness files are shared -- this file adds nothing
 * to either.
 */

/** This file's own directory, resolved without relying on process.cwd(). */
const here = path.dirname(fileURLToPath(import.meta.url));

/** The repo root -- web/e2e -> web -> repo root -- and the real CLI binary `task build:bin` places there. */
const repoRoot = path.resolve(here, "..", "..");
const loamBin = path.join(repoRoot, "bin", "loam");

/** One `loam` CLI identity: the three LOAM_AGENT_* values (docs/cli-spec.md -> whoami). */
interface Identity {
  readonly name: string;
  readonly id: string;
  readonly role: string;
}

/**
 * The three identities this fixture drives, matching demo:m4/demo:m5's
 * "<name>-<id>-<role>" shape (internal/httpauth.Identity) and their two
 * builtin, non-overlapping roles (migration 0001_init): `author` holds
 * work.start/work.set/work.request_review; `reviewer` holds work.verdict.
 * Two DISTINCT reviewer identities are required for the stale scenario --
 * see this file's header comment on why one reviewer voting twice would
 * hide the very verdict the stale test exists to show.
 */
const author: Identity = { name: "loam-e2e-p-author", id: "1", role: "author" };
const reviewerA: Identity = { name: "loam-e2e-p-reviewer-a", id: "2", role: "reviewer" };
const reviewerB: Identity = { name: "loam-e2e-p-reviewer-b", id: "3", role: "reviewer" };

/** Titles distinct enough to assert on and to tell apart in a transcript. */
const approveTitle = "E2E: proposal decision journey (approve path)";
const staleTitle = "E2E: proposal decision journey (stale-verdict path)";

/** Options {@link run} accepts; `env` defaults to the current process's own. */
interface RunOptions {
  readonly cwd?: string;
  readonly input?: string;
  readonly env?: NodeJS.ProcessEnv;
}

/** Runs `cmd args...`, throwing an Error carrying combined stdout+stderr on a non-zero exit. */
function run(cmd: string, args: readonly string[], options: RunOptions = {}): string {
  try {
    return execFileSync(cmd, args, {
      cwd: options.cwd,
      input: options.input,
      env: options.env ?? process.env,
      encoding: "utf8",
    });
  } catch (error) {
    const failure = error as { stdout?: string; stderr?: string; message: string };
    const output = [failure.stdout, failure.stderr].filter((chunk) => chunk !== undefined).join("\n");
    throw new Error(`${cmd} ${args.join(" ")} failed: ${output.length > 0 ? output : failure.message}`);
  }
}

/** Runs the real `bin/loam` binary as one identity against the running server. */
function loam(identity: Identity, serverURL: string, args: readonly string[], options: { cwd?: string; input?: string } = {}): string {
  if (!fs.existsSync(loamBin)) {
    throw new Error(
      `web/e2e/proposal-decision.e2e.ts: ${loamBin} does not exist. Run this suite via \`task test:e2e\` ` +
        "(Taskfile.yml), whose `task build:bin` step builds it before the Playwright suite runs.",
    );
  }
  return run(loamBin, args, {
    cwd: options.cwd,
    input: options.input,
    env: {
      ...process.env,
      LOAM_SERVER_URL: serverURL,
      LOAM_AGENT_NAME: identity.name,
      LOAM_AGENT_ID: identity.id,
      LOAM_AGENT_ROLE: identity.role,
    },
  });
}

/** The last "/"-separated segment of a repo identifier -- the directory name `loam clone` creates it under. */
function repoLastSegment(repo: string): string {
  const idx = repo.lastIndexOf("/");
  if (idx < 0 || idx === repo.length - 1) {
    throw new Error(`repo identifier ${JSON.stringify(repo)} is not shaped like "<group>/<name>"`);
  }
  return repo.slice(idx + 1);
}

/** `loam work start`'s JSON success shape (docs/cli-spec.md -> start) -- only `name` is needed here. */
interface WorkStartOutput {
  readonly name: string;
}

/**
 * Asserts the seeded repo is already enrolled, without ever enrolling it
 * itself -- see this file's header comment for why. Polls
 * RepoAdminService.ListRepos with the same shape ./fixtures.ts's
 * observeSyncState uses, over plain `fetch` with a manual Basic-auth header
 * (rather than Playwright's `request` fixture, whose scoping in
 * `beforeAll` this file does not want to depend on).
 */
async function requireRepoEnrolled(baseURL: string): Promise<void> {
  const adminUser = process.env["LOAM_E2E_ADMIN_USER"] ?? "admin";
  const adminPassword = process.env["LOAM_E2E_ADMIN_PASSWORD"];
  if (adminPassword === undefined || adminPassword === "") {
    throw new Error("web/e2e/proposal-decision.e2e.ts: LOAM_E2E_ADMIN_PASSWORD is required (see playwright.config.ts)");
  }
  const authHeader = `Basic ${Buffer.from(`${adminUser}:${adminPassword}`).toString("base64")}`;
  const deadline = Date.now() + 30_000;
  let lastStatus = "never queried";
  while (Date.now() < deadline) {
    const res = await fetch(new URL("/loam.admin.v1.RepoAdminService/ListRepos", baseURL), {
      method: "POST",
      headers: { "content-type": "application/json", authorization: authHeader },
      body: "{}",
    });
    if (res.ok) {
      const body = (await res.json()) as { repos?: readonly { repo?: string }[] };
      if ((body.repos ?? []).some((candidate) => candidate.repo === e2eEnv.repoIdentifier)) return;
      lastStatus = `200 OK but ${e2eEnv.repoIdentifier} not (yet) listed`;
    } else {
      lastStatus = `HTTP ${res.status}`;
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error(
    `web/e2e/proposal-decision.e2e.ts: repo ${e2eEnv.repoIdentifier} was never observed enrolled (last: ${lastStatus}). ` +
      "This journey assumes web/e2e/enroll.e2e.ts's browser journey has already enrolled the shared seeded repo -- " +
      "it deliberately does not enroll the repo itself (see this file's header comment on why). Running this file " +
      "before enroll.e2e.ts, or alone, is the misuse this check exists to catch.",
  );
}

/**
 * Drives the real CLI end to end for one proposal: clone (once, reused
 * across both work branches), start, commit a real file and push it,
 * title/describe, and request review -- everything demo:m4/demo:m5 drive
 * through `loam`/`git` rather than a fixture row. Returns the server-
 * generated work-branch name.
 */
function pushProposal(
  serverURL: string,
  cloneDir: string,
  noteFile: string,
  noteBody: string,
  title: string,
  description: string,
): string {
  const startOut = JSON.parse(loam(author, serverURL, ["work", "start", e2eEnv.repoIdentifier, e2eEnv.targetBranch])) as WorkStartOutput;
  const workBranch = startOut.name;
  run("git", ["-C", cloneDir, "checkout", "-q", e2eEnv.targetBranch]);
  run("git", ["-C", cloneDir, "checkout", "-q", "-b", workBranch]);
  const notePath = path.join(cloneDir, noteFile);
  fs.mkdirSync(path.dirname(notePath), { recursive: true });
  fs.writeFileSync(notePath, noteBody);
  run("git", ["-C", cloneDir, "add", noteFile]);
  run("git", ["-C", cloneDir, "commit", "-q", "-m", `docs: ${title}`]);
  run("git", ["-C", cloneDir, "push", "-q", "origin", workBranch]);
  loam(author, serverURL, ["work", "set", e2eEnv.repoIdentifier, workBranch, "--title", title], { input: description });
  loam(author, serverURL, ["work", "request-review", e2eEnv.repoIdentifier, workBranch]);
  return workBranch;
}

let workspace: string;
let approveBranch: string;
let staleBranch: string;
const approveNoteFile = "e2e-notes/approve-path.md";
const staleNoteFile = "e2e-notes/stale-path.md";

test.beforeAll(async ({ baseURL }) => {
  if (baseURL === undefined || baseURL === "") {
    throw new Error("web/e2e/proposal-decision.e2e.ts: no baseURL configured (see playwright.config.ts)");
  }
  await requireRepoEnrolled(baseURL);

  workspace = fs.mkdtempSync(path.join(os.tmpdir(), "loam-e2e-proposal-"));
  loam(author, baseURL, ["clone", e2eEnv.repoIdentifier, e2eEnv.targetBranch], { cwd: workspace });
  const cloneDir = path.join(workspace, repoLastSegment(e2eEnv.repoIdentifier));

  // approveBranch: one round, one non-stale approve -- the happy path.
  approveBranch = pushProposal(
    baseURL,
    cloneDir,
    approveNoteFile,
    "# E2E proposal note\n\nPushed by web/e2e/proposal-decision.e2e.ts's happy-path journey.\n",
    approveTitle,
    "Adds a note so this proposal carries a real, reviewable diff.",
  );
  loam(reviewerA, baseURL, ["work", "verdict", e2eEnv.repoIdentifier, approveBranch, "--outcome", "approve"]);

  // staleBranch: round 1 approve (by reviewerA), a second request-review
  // (opens round 2, marks round 1's approve stale), then round 2 closed out
  // by reviewerB's NEUTRAL verdict -- state is REVIEWED again, but
  // CurrentRoundApproveCount is 0 (proto/loam/v1/common.proto:103,
  // loam-2xe6). See this file's header comment for why reviewerB must be a
  // SECOND identity rather than reviewerA voting again.
  staleBranch = pushProposal(
    baseURL,
    cloneDir,
    staleNoteFile,
    "# E2E proposal note (stale path)\n\nPushed by web/e2e/proposal-decision.e2e.ts's stale-verdict journey.\n",
    staleTitle,
    "Adds a note; this proposal's only approve verdict will be made stale by a second review round.",
  );
  loam(reviewerA, baseURL, ["work", "verdict", e2eEnv.repoIdentifier, staleBranch, "--outcome", "approve"]);
  loam(author, baseURL, ["work", "request-review", e2eEnv.repoIdentifier, staleBranch]);
  loam(reviewerB, baseURL, ["work", "verdict", e2eEnv.repoIdentifier, staleBranch, "--outcome", "neutral"]);
});

test.afterAll(() => {
  if (workspace !== undefined) fs.rmSync(workspace, { recursive: true, force: true });
});

test.describe("proposal decision journey", () => {
  // loam-4kz (FIXED; this test previously carried a `test.fail()`
  // annotation documenting it here, scoped inside the test body on
  // purpose -- at describe scope it would have silently flagged the
  // stale-verdict test below as expected-to-fail too).
  //
  // AcceptProposal used to reach `POST https://<host>/.../pulls` and die
  // with "http: server gave HTTP response to HTTPS client": EnrollRepo
  // stored a BARE repos.forge_host, and internal/forge/forgejo.go's
  // apiBaseURL honours an explicit scheme but otherwise prepends
  // "https://" unconditionally -- so no repo enrolled through the real
  // EnrollRepo RPC could have a PR opened against a plaintext-HTTP forge.
  // Reproduced independently in a second flow by loam-li0.11.2
  // (SetUpstreamToken, same error).
  //
  // Fixed by internal/handler/repoadmin/handler.go's forgeHostOf:
  // deriveRepoIdentity now derives a scheme-qualified repos.forge_host
  // for a plain-HTTP upstream (this e2e stack's seeded Forgejo), which
  // apiBaseURL has always addressed correctly. The assertions below are
  // unweakened from before the fix.
  test("opening a reviewed proposal from the queue: diff, verdicts, accept, PR URL shown", async ({ page }) => {
    await page.goto("/proposals");
    await expect(page.getByRole("heading", { name: "Proposals", level: 1 })).toBeVisible();

    const row = page.getByRole("row").filter({ has: page.getByRole("link", { name: approveTitle, exact: true }) });
    await expect(row).toBeVisible();
    await row.getByRole("link", { name: approveTitle, exact: true }).click();

    await expect(page).toHaveURL(new RegExp(`/proposals/${e2eEnv.repoIdentifier}/${approveBranch}$`));
    await expect(page.getByRole("heading", { name: approveTitle, level: 1 })).toBeVisible();

    // The diff is real, from the actual pushed commit -- not a placeholder.
    await expect(page.locator("pre")).toContainText(approveNoteFile);
    await expect(page.locator("pre")).toContainText("E2E proposal note");

    // The verdict is current (round 1) and non-stale -- reviewerA's row
    // reads a plain "Approve", never "(stale)".
    const verdictsTable = page.getByRole("table", { name: "Verdicts" });
    const reviewerRow = verdictsTable.getByRole("row").filter({ hasText: reviewerA.name });
    await expect(reviewerRow).toContainText("Approve");
    await expect(reviewerRow).not.toContainText("(stale)");
    await expect(reviewerRow).toContainText("No"); // Stale column

    await page.getByRole("button", { name: "Accept", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Accept proposal" });
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: "Accept", exact: true }).click();

    // Each of pr_url/upstream_branch is its OWN CopyField -- an unqualified
    // "Copy" would match both (bead note), so both locators are qualified
    // by their field's own label.
    await expect(page.getByRole("button", { name: "Copy pull request URL" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Copy upstream branch" })).toBeVisible();

    const prUrlField = page.getByLabel("pull request URL");
    await expect(prUrlField).not.toHaveValue("");
    const prUrl = await prUrlField.inputValue();
    expect(prUrl, `expected a Forgejo pull-request URL, got ${JSON.stringify(prUrl)}`).toContain("/pulls/");

    // upstream_branch is deterministic (docs/sync-spec.md): loam/<work branch>.
    await expect(page.getByLabel("upstream branch")).toHaveValue(`loam/${approveBranch}`);

    // Accept succeeded: the dialog closed and the Accept button itself is
    // gone now that the work branch carries an upstream_pr_url (bead note).
    await expect(dialog).toBeHidden();
    await expect(page.getByRole("button", { name: "Accept", exact: true })).toHaveCount(0);
  });

  test("a stale approve from an earlier round does not let accept through (loam-2xe6)", async ({ page }) => {
    await page.goto(`/proposals/${e2eEnv.repoIdentifier}/${staleBranch}`);
    await expect(page.getByRole("heading", { name: staleTitle, level: 1 })).toBeVisible();

    const verdictsTable = page.getByRole("table", { name: "Verdicts" });
    // reviewerA's round-1 approve is stale: the SPA renders it neutral and
    // suffixes the label, never a plain green "Approve" (verdictSummaryIntent,
    // web/src/components/statusIntent.ts -- this is the exact misreading
    // loam-2xe6 exists to prevent).
    const staleRow = verdictsTable.getByRole("row").filter({ hasText: reviewerA.name });
    await expect(staleRow).toContainText("Approve (stale)");
    await expect(staleRow).toContainText("Yes"); // Stale column

    // reviewerB's round-2 neutral verdict is current and is NOT an approve,
    // so it does not satisfy AcceptProposal's ">= 1 non-stale approve" rule
    // either -- it is what brought the branch back to REVIEWED at all.
    const currentRow = verdictsTable.getByRole("row").filter({ hasText: reviewerB.name });
    await expect(currentRow).toContainText("Neutral");
    await expect(currentRow).not.toContainText("(stale)");

    await page.getByRole("button", { name: "Accept", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Accept proposal" });
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: "Accept", exact: true }).click();

    // FailedPrecondition maps to the accept-specific message, never raw
    // server text (bead note; web/src/routes/ProposalDetail.tsx).
    await expect(dialog.getByRole("alert")).toContainText(
      "This proposal needs at least one non-stale approve verdict before it can be accepted.",
    );

    // Refused, not silently accepted: the dialog is still open (no onSuccess
    // ran) and neither CopyField exists anywhere on the page.
    await expect(dialog).toBeVisible();
    await expect(page.getByRole("button", { name: "Copy pull request URL" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Accept", exact: true }).last()).toBeVisible();
  });
});
