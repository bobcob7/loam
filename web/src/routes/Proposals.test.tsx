import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { describe, expect, it } from "vitest";
import { createQueryClient } from "../queryClient";
import {
  ListProposalsResponseSchema,
  ProposalSchema,
  ProposalService,
  type ListProposalsResponse,
} from "../gen/loam/admin/v1/proposal_pb";
import {
  PageInfoSchema,
  UpstreamDrift,
  VerdictOutcome,
  VerdictSummarySchema,
  WorkBranchConflict,
  WorkBranchState,
} from "../gen/loam/v1/common_pb";
import styles from "../components/StatusBadge.module.css";
import { workBranchFixture } from "../test/fixtures";
import { Proposals } from "./Proposals";

/**
 * The bead's own DESIGN section prescribes this exact harness: connect-query's
 * `TransportProvider` over a `createRouterTransport` stub (so no real network
 * call happens), a real `QueryClient` from the shared factory (so the
 * screen's retry/staleTime policy is the real one, not re-specified here),
 * and a wouter `Router` pinned to an in-memory location via `memoryLocation`
 * (so this never depends on jsdom's own URL). `AppProviders` (src/App.tsx)
 * is not reusable here because it hard-wires the singleton `transport`,
 * which talks to a real fetch -- there is no seam to inject a stub through
 * it.
 */
function renderProposals(transport: ReturnType<typeof createRouterTransport>): void {
  const client = createQueryClient();
  const { hook } = memoryLocation({ path: "/proposals" });
  const Wrapper = (): ReactNode => (
    <TransportProvider transport={transport}>
      <QueryClientProvider client={client}>
        <Router hook={hook}>
          <Proposals />
        </Router>
      </QueryClientProvider>
    </TransportProvider>
  );
  render(<Wrapper />);
}

/** Looks up a StatusBadge intent's real CSS Module class, so the "does this
    badge carry the success tint" assertions below check the actual export
    rather than a hard-coded guess at its hashed name. */
const badgeClass = (intent: "success" | "neutral" | "warning"): string => {
  const name = styles[intent];
  if (name === undefined) throw new Error(`StatusBadge.module.css has no .${intent} class`);
  return name;
};

/**
 * The healthy branch every case below starts from (loam-mvso).
 *
 * `acceptable` is now TRUE, matching what the server's `acceptableNow` would
 * compute from the accompanying branch; it was left at its `false` zero
 * before, so this response rendered a "Not acceptable" pill on a reviewed,
 * approved, cleanly-merging branch. The `conflict`/`upstream_drift` values
 * come from the shared builder -- see src/test/fixtures.ts for why omitting
 * them is not cosmetic.
 */
const oneProposalResponse = (): ListProposalsResponse =>
  create(ListProposalsResponseSchema, {
    proposals: [
      create(ProposalSchema, {
        acceptable: true,
        workBranch: workBranchFixture(),
        verdicts: [
          create(VerdictSummarySchema, {
            reviewer: "agent-1-reviewer",
            outcome: VerdictOutcome.APPROVE,
            stale: true,
            round: 1,
          }),
          create(VerdictSummarySchema, {
            reviewer: "agent-2-reviewer",
            outcome: VerdictOutcome.APPROVE,
            stale: false,
            round: 2,
          }),
        ],
      }),
    ],
    pageInfo: create(PageInfoSchema, { total: 40 }),
  });

describe("Proposals", () => {
  it("renders a loading state before the query resolves", async () => {
    let resolve: (value: ListProposalsResponse) => void = () => undefined;
    const pending = new Promise<ListProposalsResponse>((res) => {
      resolve = res;
    });
    const transport = createRouterTransport(({ service }) => {
      service(ProposalService, { listProposals: () => pending });
    });
    renderProposals(transport);
    expect(screen.getByRole("status")).toHaveTextContent(/loading/i);
    resolve(oneProposalResponse());
    await waitFor(() => expect(screen.getByRole("table")).toBeInTheDocument());
  });

  it("lists a proposal's work branch, repo, target, state, and per-reviewer verdicts, and links the title to the detail screen", async () => {
    const transport = createRouterTransport(({ service }) => {
      service(ProposalService, { listProposals: () => oneProposalResponse() });
    });
    renderProposals(transport);

    const link = await screen.findByRole("link", { name: "Add retry to the sync loop" });
    // proposalDetailPath, never a hand-assembled string -- the repo half of
    // the identifier contains its own slash (routes/paths.ts).
    expect(link).toHaveAttribute("href", "/proposals/acme/widgets/wb-9c2f1a");

    const row = link.closest("tr");
    if (row === null) throw new Error("unreachable: the link renders inside a table row");
    const withinRow = within(row);
    expect(withinRow.getByText("acme/widgets")).toBeInTheDocument();
    expect(withinRow.getByText("main")).toBeInTheDocument();
    expect(withinRow.getByText("Reviewed")).toBeInTheDocument();

    // The stale APPROVE: THE assertion this screen exists for. It must read
    // "Approve (stale)" and must NOT carry the success tint -- a plain
    // `getByText` on the label alone would still pass if the intent
    // regressed to success, so the class is checked directly.
    const staleBadge = withinRow.getByText("Approve (stale)");
    expect(staleBadge.className).not.toContain(badgeClass("success"));
    expect(staleBadge.className).toContain(badgeClass("neutral"));

    // The non-stale APPROVE in the same row must still read as success --
    // otherwise "never render success" could be satisfied by never rendering
    // success at all.
    const liveBadge = withinRow.getByText("Approve");
    expect(liveBadge.className).toContain(badgeClass("success"));

    // Round is surfaced, but as its own text, never folded into either badge.
    expect(withinRow.getByText("Round 1")).toBeInTheDocument();
    expect(withinRow.getByText("Round 2")).toBeInTheDocument();

    // loam-mvso. The "Blocked by" cell short-circuits on `row.acceptable`
    // (Proposals.tsx), so on an acceptable row the branch's `conflict` and
    // `upstream_drift` never reach the DOM at all. This pins the clear row
    // saying so out loud: "no blocker pill" and "empty cell" are the same DOM,
    // so the marker is asserted by its own text rather than inferred from the
    // absences below. It does NOT pin the fixture's enum fidelity --
    // src/test/fixtures.test.ts does.
    expect(withinRow.getByText("—")).toBeInTheDocument();
    expect(withinRow.queryByText("Not acceptable")).not.toBeInTheDocument();
    expect(withinRow.queryByText("Conflicted")).not.toBeInTheDocument();
    expect(withinRow.queryByText("Upstream diverged")).not.toBeInTheDocument();
    expect(withinRow.queryByText("Conflict unknown")).not.toBeInTheDocument();
    expect(withinRow.queryByText("Upstream drift unknown")).not.toBeInTheDocument();

    // Paginates via Pager: pageInfo.total (40) exceeds the default limit
    // (25), so the pager must render its landmark and the correct summary.
    const pager = screen.getByRole("navigation", { name: "Pagination" });
    expect(within(pager).getByText(/page 1 of 2/i)).toBeInTheDocument();
  });

  // loam-u84g. A conflicting target advance demotes an approved branch out of
  // REVIEWED and back to DRAFT, leaving its approve non-stale
  // (docs/git-spec.md -> Target Advances & Catch-Up). Such a branch used to be
  // omitted from this response entirely, so the operator merged everything the
  // queue offered and never learned it existed; an approved P1 fix missed
  // v0.0.8 that way. It is now listed with `acceptable = false`, and the row
  // must SAY SO -- rendering it as an ordinary proposal would be worse than
  // omitting it, because the admin would click Accept and the server would
  // refuse.
  it("marks a blocked proposal, naming the conflict, while leaving an acceptable one unmarked", async () => {
    const transport = createRouterTransport(({ service }) => {
      service(ProposalService, {
        listProposals: () =>
          create(ListProposalsResponseSchema, {
            proposals: [
              create(ProposalSchema, {
                acceptable: true,
                workBranch: workBranchFixture(),
                verdicts: [
                  create(VerdictSummarySchema, {
                    reviewer: "agent-2-reviewer",
                    outcome: VerdictOutcome.APPROVE,
                    stale: false,
                    round: 2,
                  }),
                ],
              }),
              create(ProposalSchema, {
                acceptable: false,
                workBranch: workBranchFixture({
                  name: "wb-88c455",
                  title: "Strip pool params from the migration DSN",
                  state: WorkBranchState.DRAFT,
                  conflict: WorkBranchConflict.RESET,
                  upstreamDrift: UpstreamDrift.DIVERGED,
                }),
                verdicts: [
                  create(VerdictSummarySchema, {
                    reviewer: "agent-1-reviewer",
                    outcome: VerdictOutcome.APPROVE,
                    stale: false,
                    round: 3,
                  }),
                ],
              }),
            ],
            pageInfo: create(PageInfoSchema, { total: 2 }),
          }),
      });
    });
    renderProposals(transport);

    const blockedLink = await screen.findByRole("link", {
      name: "Strip pool params from the migration DSN",
    });
    const blockedRow = blockedLink.closest("tr");
    if (blockedRow === null) throw new Error("unreachable: the link renders inside a table row");
    const withinBlocked = within(blockedRow);
    // Both reasons, as two separate badges. Collapsing them would send the
    // admin to fix the wrong thing (statusIntent.ts) -- a conflict is a
    // catch-up push, divergence is reconciled on the forge.
    expect(withinBlocked.getByText("Conflict reset")).toBeInTheDocument();
    expect(withinBlocked.getByText("Upstream diverged")).toBeInTheDocument();
    // Its approve is genuinely non-stale, so the success tint is correct and
    // is exactly why "blocked" has to be said out loud somewhere else.
    expect(withinBlocked.getByText("Approve").className).toContain(badgeClass("success"));

    const okLink = screen.getByRole("link", { name: "Add retry to the sync loop" });
    const okRow = okLink.closest("tr");
    if (okRow === null) throw new Error("unreachable: the link renders inside a table row");
    // The acceptable row must carry NO blocker badge -- otherwise "the blocked
    // row is badged" would be satisfied by badging every row.
    expect(within(okRow).queryByText("Conflict reset")).not.toBeInTheDocument();
    expect(within(okRow).queryByText("Upstream diverged")).not.toBeInTheDocument();
    expect(within(okRow).queryByText("Not acceptable")).not.toBeInTheDocument();
  });

  // The third blocker is neither `conflict` nor `upstream_drift`: a branch can
  // be unacceptable purely because it is no longer REVIEWED. The row must
  // still say something rather than render an empty cell that reads as clear.
  it("falls back to a generic blocked badge when neither conflict nor drift explains it", async () => {
    const transport = createRouterTransport(({ service }) => {
      service(ProposalService, {
        listProposals: () =>
          create(ListProposalsResponseSchema, {
            proposals: [
              create(ProposalSchema, {
                acceptable: false,
                workBranch: workBranchFixture({
                  name: "wb-1a2b3c",
                  title: "Sent back for another round",
                  state: WorkBranchState.REVIEWABLE,
                }),
                verdicts: [],
              }),
            ],
            pageInfo: create(PageInfoSchema, { total: 1 }),
          }),
      });
    });
    renderProposals(transport);
    expect(await screen.findByText("Not acceptable")).toBeInTheDocument();
  });

  // loam-mvso. UNSPECIFIED is set here DELIBERATELY -- spelled out, not
  // omitted -- because that is the only way to say "the server did not report
  // this" as a claim rather than as an oversight. docs/web-spec.md:
  // UNSPECIFIED never means "fine", so a column headed "Blocked by" must not
  // leave the cell empty, and it must name both fields separately rather than
  // emit two identical "Unknown" pills that collide as React keys and tell the
  // admin nothing about which is which. `acceptable` is false because a server
  // old enough not to send these two is old enough not to send it either.
  it("names both fields when the server reported neither, rather than reading as clear", async () => {
    const transport = createRouterTransport(({ service }) => {
      service(ProposalService, {
        listProposals: () =>
          create(ListProposalsResponseSchema, {
            proposals: [
              create(ProposalSchema, {
                acceptable: false,
                workBranch: workBranchFixture({
                  name: "wb-7d3e9f",
                  title: "Reported by a server that never set the fields",
                  conflict: WorkBranchConflict.UNSPECIFIED,
                  upstreamDrift: UpstreamDrift.UNSPECIFIED,
                }),
                verdicts: [],
              }),
            ],
            pageInfo: create(PageInfoSchema, { total: 1 }),
          }),
      });
    });
    renderProposals(transport);

    const link = await screen.findByRole("link", {
      name: "Reported by a server that never set the fields",
    });
    const row = link.closest("tr");
    if (row === null) throw new Error("unreachable: the link renders inside a table row");
    const withinRow = within(row);
    expect(withinRow.getByText("Conflict unknown")).toBeInTheDocument();
    expect(withinRow.getByText("Upstream drift unknown")).toBeInTheDocument();
    // Not the generic fallback: the two fields DID say something -- that they
    // cannot be vouched for -- which is more than "not acceptable" conveys.
    expect(withinRow.queryByText("Not acceptable")).not.toBeInTheDocument();
  });

  it("renders the empty state when no proposals are awaiting a decision", async () => {
    const transport = createRouterTransport(({ service }) => {
      service(ProposalService, {
        listProposals: () =>
          create(ListProposalsResponseSchema, {
            proposals: [],
            pageInfo: create(PageInfoSchema, { total: 0 }),
          }),
      });
    });
    renderProposals(transport);
    expect(await screen.findByText("No proposals awaiting decision.")).toBeInTheDocument();
    // Nothing to page through -- Pager renders nothing when the whole result
    // set fits on one page (src/components/Pager.tsx).
    expect(screen.queryByRole("navigation", { name: "Pagination" })).not.toBeInTheDocument();
  });

  it("renders an ErrorBanner with the server's message on a generic failure", async () => {
    const transport = createRouterTransport(({ service }) => {
      service(ProposalService, {
        listProposals: () => {
          throw new ConnectError("graph index unavailable", Code.Internal);
        },
      });
    });
    renderProposals(transport);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("graph index unavailable");
  });

  it("renders the auth-required message, not a raw error, when the query is unauthenticated", async () => {
    const transport = createRouterTransport(({ service }) => {
      service(ProposalService, {
        listProposals: () => {
          throw new ConnectError("invalid credentials", Code.Unauthenticated);
        },
      });
    });
    renderProposals(transport);
    const alert = await screen.findByRole("alert");
    // mapConnectError's auth-required outcome carries no server message
    // (docs/web-frontend-spec.md -> Auth), so this must be the screen's own
    // recovery text, not "invalid credentials" leaking through.
    expect(alert).toHaveTextContent(/sign in required/i);
    expect(alert).not.toHaveTextContent("invalid credentials");
  });
});
