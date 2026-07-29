import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AppProviders } from "../App";
import { ProposalDetail } from "./ProposalDetail";

/**
 * Drives the screen through the real connect-web transport (stubbed at
 * `fetch`, exactly as transport.test.ts and invalidation.test.tsx do) rather
 * than a hand-rolled fake `Transport`, so these tests exercise the same
 * wiring the browser gets: a real connect-query `useQuery`/`useMutation`
 * pair, a real `QueryClient`, and the Connect protocol's actual JSON error
 * shape (`{ code, message }` on a non-200 response -- see
 * @connectrpc/connect/protocol-connect's validateResponse/errorFromJson,
 * which is what turns a stubbed HTTP response back into a typed
 * `ConnectError`).
 */

type RouteBody = Record<string, unknown>;

interface Route {
  readonly status: number;
  readonly body: RouteBody;
}

const ok = (body: RouteBody): Route => ({ status: 200, body });
const failure = (code: string, message: string): Route => ({ status: 400, body: { code, message } });

const workBranchPath = "/loam.v1.WorkBranchService/GetWorkBranch";
const diffPath = "/loam.v1.WorkBranchService/GetWorkBranchDiff";
const commentsPath = "/loam.v1.WorkBranchService/ListComments";
const verdictsPath = "/loam.v1.WorkBranchService/ListVerdicts";
const requestReviewPath = "/loam.v1.WorkBranchService/RequestReview";
const acceptPath = "/loam.admin.v1.ProposalService/AcceptProposal";
const closePath = "/loam.admin.v1.ProposalService/CloseWorkBranch";

const repo = "acme/widgets";
const workBranch = "wb-9c2f1a";

const workBranchFixture = (overrides: RouteBody = {}): RouteBody => ({
  repo,
  name: workBranch,
  target: "main",
  title: "Add search index",
  description: "Adds a search index for the widgets package.",
  state: "WORK_BRANCH_STATE_REVIEWED",
  author: "widget-writer-1-worker",
  ...overrides,
});

/**
 * One non-stale APPROVE (counts toward the bar) and one STALE APPROVE from an
 * earlier round (does not) -- the fixture the whole suite exists to protect:
 * loam/v1/common.proto's VerdictSummary doc comment ("only non-stale verdicts
 * count toward the approval bar") and loam-2xe6's fix
 * (verdictSummaryIntent must force neutral + "(stale)" regardless of
 * outcome).
 */
const verdictsFixture: RouteBody = {
  verdicts: [
    { reviewer: "reviewer-a-agent", outcome: "VERDICT_OUTCOME_APPROVE", stale: true, round: 1 },
    { reviewer: "reviewer-b-agent", outcome: "VERDICT_OUTCOME_APPROVE", stale: false, round: 2 },
  ],
};

const commentsFixture: RouteBody = {
  threads: [
    {
      id: "thread-1",
      resolved: false,
      anchor: { file: "src/index.ts", line: 42 },
      comments: [{ author: "reviewer-a-agent", body: "Consider renaming this.", round: 1 }],
      round: 1,
    },
  ],
  pageInfo: { total: 1 },
};

/** The full set of GET queries this screen fires, with sane defaults. */
const baseRoutes = (): Record<string, Route> => ({
  [workBranchPath]: ok({ workBranch: workBranchFixture() }),
  [diffPath]: ok({ diff: "--- a/src/index.ts\n+++ b/src/index.ts\n@@ -1 +1 @@\n-old\n+new\n" }),
  [commentsPath]: ok(commentsFixture),
  [verdictsPath]: ok(verdictsFixture),
});

/** Tracks every request made, keyed by RPC path, so a test can assert a
 * refetch happened (proof that a mutation's declared invalidation actually
 * re-triggered the active query) without reaching into a QueryClient that
 * AppProviders keeps private. */
const stubFetch = (initial: Record<string, Route>): { routes: Record<string, Route>; calls: string[] } => {
  const routes = { ...initial };
  const calls: string[] = [];
  vi.stubGlobal("fetch", (input: RequestInfo | URL) => {
    const url = String(input);
    calls.push(url);
    const route = routes[url];
    if (route === undefined) {
      return Promise.reject(new Error(`unstubbed request: ${url}`));
    }
    return Promise.resolve(
      new Response(JSON.stringify(route.body), {
        status: route.status,
        headers: { "content-type": "application/json" },
      }),
    );
  });
  return { routes, calls };
};

/**
 * Decodes a captured fetch `init.body` back to JSON. connect-web serializes
 * a unary request to bytes (a `Uint8Array`), not a string, so
 * `String(init.body)` produces a comma-joined byte list rather than JSON
 * text; routing it through `Response` normalises whatever `BodyInit` shape
 * fetch was given.
 */
const readJsonBody = async (init: RequestInit | undefined): Promise<unknown> =>
  init?.body === undefined ? undefined : JSON.parse(await new Response(init.body as BodyInit).text());

const renderScreen = (): ReturnType<typeof render> =>
  render(
    <AppProviders>
      <ProposalDetail repo={repo} workBranch={workBranch} />
    </AppProviders>,
  );

/** Waits for the primary query to settle so a test can start from the loaded screen. */
const renderLoaded = async (): Promise<void> => {
  renderScreen();
  await screen.findByRole("heading", { level: 1, name: "Add search index" });
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("ProposalDetail: loading, not-found, error", () => {
  it("shows a loading state before the work branch query settles", () => {
    stubFetch(baseRoutes());
    renderScreen();
    expect(screen.getByText("Loading proposal…")).toBeInTheDocument();
  });

  it("renders a not-found state via ErrorBanner when GetWorkBranch reports not_found", async () => {
    stubFetch({ ...baseRoutes(), [workBranchPath]: failure("not_found", "wb-missing") });
    renderScreen();
    const banner = await screen.findByRole("alert");
    expect(within(banner).getByText("Proposal not found")).toBeInTheDocument();
  });

  it("renders a generic error state via ErrorBanner for an unmapped failure", async () => {
    stubFetch({ ...baseRoutes(), [workBranchPath]: failure("internal", "index corrupted") });
    renderScreen();
    const banner = await screen.findByRole("alert");
    expect(within(banner).getByText("Could not load proposal")).toBeInTheDocument();
    expect(within(banner).getByText("index corrupted")).toBeInTheDocument();
  });
});

describe("ProposalDetail: loaded screen", () => {
  it("renders the work branch's title, state and target", async () => {
    stubFetch(baseRoutes());
    await renderLoaded();
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Add search index");
    expect(screen.getByText("Reviewed")).toBeInTheDocument();
    expect(screen.getByText("main")).toBeInTheDocument();
  });

  it("renders the diff", async () => {
    stubFetch(baseRoutes());
    await renderLoaded();
    expect(await screen.findByText(/-old/)).toBeInTheDocument();
  });

  it("renders one comment thread with its anchor, author and body", async () => {
    stubFetch(baseRoutes());
    await renderLoaded();
    await screen.findByText("src/index.ts:42");
    // Scoped to the Comments section: "reviewer-a-agent" is also a verdict
    // reviewer, so an unscoped query would be ambiguous.
    const comments = screen.getByRole("heading", { name: "Comments" }).closest("section");
    expect(comments).not.toBeNull();
    if (comments === null) throw new Error("unreachable: asserted above");
    expect(within(comments).getByText("reviewer-a-agent")).toBeInTheDocument();
    expect(within(comments).getByText(/Consider renaming this\./)).toBeInTheDocument();
  });

  it("renders both reviewers as distinct verdict rows, not collapsed to one", async () => {
    stubFetch(baseRoutes());
    await renderLoaded();
    const table = await screen.findByRole("table", { name: "Verdicts" });
    // Two data rows plus the header row.
    expect(within(table).getAllByRole("row")).toHaveLength(3);
    expect(within(table).getByRole("rowheader", { name: "reviewer-a-agent" })).toBeInTheDocument();
    expect(within(table).getByRole("rowheader", { name: "reviewer-b-agent" })).toBeInTheDocument();
  });

  it("carries stale and round as their own columns on the verdicts table", async () => {
    stubFetch(baseRoutes());
    await renderLoaded();
    const table = await screen.findByRole("table", { name: "Verdicts" });
    expect(within(table).getByRole("columnheader", { name: "Stale" })).toBeInTheDocument();
    expect(within(table).getByRole("columnheader", { name: "Round" })).toBeInTheDocument();
    const staleReviewerRow = within(table).getByRole("rowheader", { name: "reviewer-a-agent" }).closest("tr");
    expect(staleReviewerRow).not.toBeNull();
    if (staleReviewerRow === null) throw new Error("unreachable: asserted above");
    expect(within(staleReviewerRow).getByRole("cell", { name: "Yes" })).toBeInTheDocument();
    expect(within(staleReviewerRow).getByRole("cell", { name: "1" })).toBeInTheDocument();
  });

  // THE ONE THING THIS SCREEN EXISTS TO GET RIGHT (loam-2xe6): a stale
  // APPROVE must never render as the counted "Approve" -- it must read
  // neutral and say "(stale)" so an admin scanning verdicts cannot mistake it
  // for a verdict that clears the approval bar. Swapping
  // `verdictSummaryIntent(verdict)` for `verdictOutcomeIntent(verdict.outcome)`
  // in ProposalDetail.tsx must fail exactly this test.
  it("renders a stale APPROVE as neutral 'Approve (stale)', never the counted green Approve", async () => {
    stubFetch(baseRoutes());
    await renderLoaded();
    const table = await screen.findByRole("table", { name: "Verdicts" });
    const staleRow = within(table).getByRole("rowheader", { name: "reviewer-a-agent" }).closest("tr");
    expect(staleRow).not.toBeNull();
    if (staleRow === null) throw new Error("unreachable: asserted above");
    expect(within(staleRow).getByText("Approve (stale)")).toBeInTheDocument();
    expect(within(staleRow).queryByText("Approve")).not.toBeInTheDocument();
  });

  it("renders the non-stale APPROVE as the plain, counted 'Approve'", async () => {
    stubFetch(baseRoutes());
    await renderLoaded();
    const table = await screen.findByRole("table", { name: "Verdicts" });
    const countedRow = within(table).getByRole("rowheader", { name: "reviewer-b-agent" }).closest("tr");
    expect(countedRow).not.toBeNull();
    if (countedRow === null) throw new Error("unreachable: asserted above");
    expect(within(countedRow).getByText("Approve")).toBeInTheDocument();
    expect(within(countedRow).getByRole("cell", { name: "No" })).toBeInTheDocument();
  });
});

describe("ProposalDetail: AcceptProposal", () => {
  it("shows a confirmation dialog before accepting", async () => {
    const user = userEvent.setup();
    stubFetch(baseRoutes());
    await renderLoaded();
    await user.click(screen.getByRole("button", { name: "Accept" }));
    expect(screen.getByRole("dialog", { name: "Accept proposal" })).toBeInTheDocument();
  });

  it("shows pr_url and upstream_branch via CopyField and closes the dialog on success", async () => {
    const user = userEvent.setup();
    const { routes } = stubFetch(baseRoutes());
    routes[acceptPath] = ok({
      prUrl: "https://forge.example/acme/widgets/pull/42",
      upstreamBranch: "loam/wb-9c2f1a",
    });
    await renderLoaded();
    await user.click(screen.getByRole("button", { name: "Accept" }));
    const dialog = screen.getByRole("dialog", { name: "Accept proposal" });
    // The two CopyFields' buttons must be distinguishable by name -- both
    // named bare "Copy" would be two indistinguishable controls.
    await user.click(within(dialog).getByRole("button", { name: "Accept" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.getByLabelText("pull request URL")).toHaveValue(
      "https://forge.example/acme/widgets/pull/42",
    );
    expect(screen.getByLabelText("upstream branch")).toHaveValue("loam/wb-9c2f1a");
    expect(screen.getByRole("button", { name: "Copy pull request URL" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy upstream branch" })).toBeInTheDocument();
    // The action that just opened a PR is no longer offered.
    expect(screen.queryByRole("button", { name: "Accept" })).not.toBeInTheDocument();
  });

  it("invalidates GetWorkBranch on success (its own query refetches)", async () => {
    const user = userEvent.setup();
    const { routes, calls } = stubFetch(baseRoutes());
    routes[acceptPath] = ok({ prUrl: "https://forge.example/pr/1", upstreamBranch: "loam/wb-9c2f1a" });
    await renderLoaded();
    const workBranchCallsBefore = calls.filter((url) => url === workBranchPath).length;
    await user.click(screen.getByRole("button", { name: "Accept" }));
    const dialog = screen.getByRole("dialog", { name: "Accept proposal" });
    await user.click(within(dialog).getByRole("button", { name: "Accept" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() => {
      const after = calls.filter((url) => url === workBranchPath).length;
      expect(after).toBeGreaterThan(workBranchCallsBefore);
    });
  });

  it("maps a failed_precondition to the accept-specific message, not the server's raw one", async () => {
    const user = userEvent.setup();
    const { routes } = stubFetch(baseRoutes());
    routes[acceptPath] = failure("failed_precondition", "SOME RAW SERVER TEXT NEVER SHOWN");
    await renderLoaded();
    await user.click(screen.getByRole("button", { name: "Accept" }));
    const dialog = screen.getByRole("dialog", { name: "Accept proposal" });
    await user.click(within(dialog).getByRole("button", { name: "Accept" }));
    const banner = await within(dialog).findByRole("alert");
    expect(
      within(banner).getByText(
        "This proposal needs at least one non-stale approve verdict before it can be accepted.",
      ),
    ).toBeInTheDocument();
    expect(within(dialog).queryByText("SOME RAW SERVER TEXT NEVER SHOWN")).not.toBeInTheDocument();
    // The dialog stays open on failure so the admin can read the error and retry or cancel.
    expect(screen.getByRole("dialog", { name: "Accept proposal" })).toBeInTheDocument();
  });

  it("does not show the Accept action when the work branch already has an upstream PR", async () => {
    const { routes } = stubFetch(baseRoutes());
    routes[workBranchPath] = ok({
      workBranch: workBranchFixture({ upstreamPrUrl: "https://forge.example/pr/7" }),
    });
    await renderLoaded();
    expect(screen.queryByRole("button", { name: "Accept" })).not.toBeInTheDocument();
    expect(screen.getByLabelText("pull request URL")).toHaveValue("https://forge.example/pr/7");
  });
});

describe("ProposalDetail: CloseWorkBranch", () => {
  it("closes the proposal with the given reason and invalidates GetWorkBranch", async () => {
    const user = userEvent.setup();
    const { routes, calls } = stubFetch(baseRoutes());
    let closeRequestBody: unknown;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push(url);
      if (url === closePath) {
        closeRequestBody = await readJsonBody(init);
        return new Response(JSON.stringify({ workBranch: workBranchFixture({ state: "WORK_BRANCH_STATE_CLOSED" }) }), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }
      const route = routes[url];
      if (route === undefined) return Promise.reject(new Error(`unstubbed request: ${url}`));
      return new Response(JSON.stringify(route.body), {
        status: route.status,
        headers: { "content-type": "application/json" },
      });
    });
    await renderLoaded();
    const workBranchCallsBefore = calls.filter((url) => url === workBranchPath).length;
    await user.click(screen.getByRole("button", { name: "Close proposal" }));
    const dialog = screen.getByRole("dialog", { name: "Close proposal" });
    await user.type(within(dialog).getByRole("textbox", { name: "Reason" }), "Superseded by another change.");
    await user.click(within(dialog).getByRole("button", { name: "Close proposal" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(closeRequestBody).toMatchObject({
      repo,
      workBranch,
      body: "Superseded by another change.",
    });
    await waitFor(() => {
      const after = calls.filter((url) => url === workBranchPath).length;
      expect(after).toBeGreaterThan(workBranchCallsBefore);
    });
  });

  it("marks the reason field as required", async () => {
    const user = userEvent.setup();
    stubFetch(baseRoutes());
    await renderLoaded();
    await user.click(screen.getByRole("button", { name: "Close proposal" }));
    const dialog = screen.getByRole("dialog", { name: "Close proposal" });
    expect(within(dialog).getByRole("textbox", { name: "Reason" })).toBeRequired();
  });
});

describe("ProposalDetail: RequestReview", () => {
  it("confirms with no comment field, per docs/web-spec.md's send-back correction", async () => {
    const user = userEvent.setup();
    stubFetch(baseRoutes());
    await renderLoaded();
    await user.click(screen.getByRole("button", { name: "Request another review round" }));
    const dialog = screen.getByRole("dialog", { name: "Request another review round" });
    expect(within(dialog).queryByRole("textbox")).not.toBeInTheDocument();
  });

  it("sends only repo and work_branch, and invalidates ListVerdicts", async () => {
    const user = userEvent.setup();
    const { routes, calls } = stubFetch(baseRoutes());
    let requestReviewBody: unknown;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push(url);
      if (url === requestReviewPath) {
        requestReviewBody = await readJsonBody(init);
        return new Response(
          JSON.stringify({ workBranch: workBranchFixture({ state: "WORK_BRANCH_STATE_REVIEWABLE" }) }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      const route = routes[url];
      if (route === undefined) return Promise.reject(new Error(`unstubbed request: ${url}`));
      return new Response(JSON.stringify(route.body), {
        status: route.status,
        headers: { "content-type": "application/json" },
      });
    });
    await renderLoaded();
    const verdictsCallsBefore = calls.filter((url) => url === verdictsPath).length;
    await user.click(screen.getByRole("button", { name: "Request another review round" }));
    const dialog = screen.getByRole("dialog", { name: "Request another review round" });
    await user.click(within(dialog).getByRole("button", { name: "Request review" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(requestReviewBody).toEqual({ repo, workBranch });
    await waitFor(() => {
      const after = calls.filter((url) => url === verdictsPath).length;
      expect(after).toBeGreaterThan(verdictsCallsBefore);
    });
  });
});
