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
  VerdictOutcome,
  VerdictSummarySchema,
  WorkBranchSchema,
  WorkBranchState,
} from "../gen/loam/v1/common_pb";
import styles from "../components/StatusBadge.module.css";
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

const workBranch = () =>
  create(WorkBranchSchema, {
    repo: "acme/widgets",
    name: "wb-9c2f1a",
    target: "main",
    title: "Add retry to the sync loop",
    description: "",
    state: WorkBranchState.REVIEWED,
    author: "agent-3-implementer",
  });

const oneProposalResponse = (): ListProposalsResponse =>
  create(ListProposalsResponseSchema, {
    proposals: [
      create(ProposalSchema, {
        workBranch: workBranch(),
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

    // Paginates via Pager: pageInfo.total (40) exceeds the default limit
    // (25), so the pager must render its landmark and the correct summary.
    const pager = screen.getByRole("navigation", { name: "Pagination" });
    expect(within(pager).getByText(/page 1 of 2/i)).toBeInTheDocument();
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
