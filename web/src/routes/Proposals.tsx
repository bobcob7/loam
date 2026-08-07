import { useQuery } from "@connectrpc/connect-query";
import type { ReactElement } from "react";
import { Link } from "wouter";
import { ErrorBanner } from "../components/ErrorBanner";
import { Pager } from "../components/Pager";
import { StatusBadge } from "../components/StatusBadge";
import { Table, type TableColumn } from "../components/Table";
import {
  conflictIntent,
  upstreamDriftIntent,
  verdictSummaryIntent,
  workBranchStateIntent,
} from "../components/statusIntent";
import { mapConnectError } from "../data/mapConnectError";
import { toPagerState, useOffsetPagination } from "../data/pagination";
import type { Proposal } from "../gen/loam/admin/v1/proposal_pb";
import { listProposals } from "../gen/loam/admin/v1/proposal-ProposalService_connectquery";
import type { VerdictSummary, WorkBranch } from "../gen/loam/v1/common_pb";
import { proposalDetailPath } from "./paths";
import styles from "./Proposals.module.css";

/**
 * One table row: a proposal's work branch, this round's verdicts, and
 * whether the server would accept it right now.
 *
 * `workBranch` is optional on the wire `Proposal` message (every embedded
 * message field is, in protobuf-es's TS output) but the server never sends
 * one without it -- `ProposalService.ListProposals` only ever returns a work
 * branch with >= 1 non-stale approve verdict
 * (loam/admin/v1/proposal.proto). {@link toRow} drops the (unreachable in
 * practice) case rather than let every column below re-guard against
 * `undefined`.
 */
interface ProposalRow {
  readonly key: string;
  readonly workBranch: WorkBranch;
  readonly verdicts: readonly VerdictSummary[];
  readonly acceptable: boolean;
}

/** Converts one `Proposal` to a row, or `undefined` if it carries no work branch. */
function toRow(proposal: Proposal): ProposalRow | undefined {
  const workBranch = proposal.workBranch;
  if (workBranch === undefined) return undefined;
  return {
    key: `${workBranch.repo}/${workBranch.name}`,
    workBranch,
    verdicts: proposal.verdicts,
    acceptable: proposal.acceptable,
  };
}

const rowKey = (row: ProposalRow): string => row.key;

/**
 * Columns: work-branch title (linked, row header), repo, target branch,
 * state, a verdicts summary -- the five things docs/web-frontend-spec.md's
 * `/proposals` entry calls for -- plus a "Blocked by" column (loam-u84g). A
 * module-level constant rather than built inside the component: no column
 * reads render state, so there is nothing gained by recreating the array
 * (and its cell closures) on every render.
 */
const columns: readonly TableColumn<ProposalRow>[] = [
  {
    key: "title",
    header: "Work branch",
    rowHeader: true,
    cell: (row) => (
      <Link href={proposalDetailPath(row.workBranch.repo, row.workBranch.name)}>
        {row.workBranch.title}
      </Link>
    ),
  },
  {
    key: "repo",
    header: "Repo",
    mono: true,
    cell: (row) => row.workBranch.repo,
  },
  {
    key: "target",
    header: "Target branch",
    mono: true,
    cell: (row) => row.workBranch.target,
  },
  {
    key: "state",
    header: "State",
    cell: (row) => {
      const badge = workBranchStateIntent(row.workBranch.state);
      return <StatusBadge intent={badge.intent}>{badge.label}</StatusBadge>;
    },
  },
  {
    key: "verdicts",
    header: "Verdicts",
    cell: (row) => (
      <ul className={styles.verdictList}>
        {row.verdicts.map((verdict) => {
          // MUST be verdictSummaryIntent(verdict), never
          // verdictOutcomeIntent(verdict.outcome) directly: `stale` is a bool
          // FIELD on VerdictSummary, not a VerdictOutcome member, so the
          // outcome-only helper cannot see it and a stale APPROVE would
          // render success/"Approve" -- telling the admin the approval bar is
          // met when it is not (loam-2xe6, proto/loam/v1/common.proto:103).
          const badge = verdictSummaryIntent(verdict);
          return (
            <li key={`${verdict.reviewer}-${verdict.round}`} className={styles.verdictItem}>
              <span className={styles.reviewer}>{verdict.reviewer}</span>
              <StatusBadge intent={badge.intent}>{badge.label}</StatusBadge>
              {/* `round` is a separate plain field, deliberately never folded
                  into the badge's own label (see statusIntent.ts). */}
              <span className={styles.round}>Round {verdict.round}</span>
            </li>
          );
        })}
      </ul>
    ),
  },
  {
    key: "blocked",
    header: "Blocked by",
    // The queue used to OMIT a branch that could not be accepted -- which is
    // how an approved P1 fix missed the v0.0.8 release: it was absent, not
    // shown blocked, so the operator merged everything on offer and never
    // saw it (loam-u84g). The server now lists it with `acceptable = false`
    // and this column says why.
    //
    // The reason comes from the work branch's own `conflict` and
    // `upstream_drift` fields, which can BOTH be set at once and are
    // deliberately never collapsed into one pill (statusIntent.ts). The
    // "Not acceptable" fallback covers the third blocker, which is neither
    // field: a conflicting advance demoted the branch out of REVIEWED, so
    // its state pill (one column left) already names it.
    cell: (row) => {
      if (row.acceptable) return <span className={styles.clear}>—</span>;
      const badges = [conflictIntent(row.workBranch.conflict), upstreamDriftIntent(row.workBranch.upstreamDrift)].filter(
        (badge) => badge !== undefined,
      );
      const shown = badges.length > 0 ? badges : [{ intent: "warning" as const, label: "Not acceptable" }];
      return (
        <ul className={styles.blockedList}>
          {shown.map((badge) => (
            <li key={badge.label}>
              <StatusBadge intent={badge.intent}>{badge.label}</StatusBadge>
            </li>
          ))}
        </ul>
      );
    },
  },
];

/**
 * Proposals (`/proposals`) -- the review queue: reviewed work branches
 * awaiting an admin decision, each already carrying its verdicts so the
 * admin sees who approved without a second call
 * (docs/web-frontend-spec.md -> Routing & Screens). The route takes no
 * parameters, so -- like every screen in this directory -- this takes plain
 * props (none, here) rather than calling `useParams`.
 */
export function Proposals(): ReactElement {
  const { page, setOffset } = useOffsetPagination();
  const query = useQuery(listProposals, { page });

  if (query.isPending) {
    return (
      <>
        <h1>Proposals</h1>
        <p role="status">Loading proposals…</p>
      </>
    );
  }

  if (query.isError) {
    const outcome = mapConnectError(query.error);
    // auth-required carries no message of its own (docs/web-frontend-spec.md
    // -> Auth: there is no login page, so a refresh is the recovery, not a
    // server-sent string) -- every other outcome kind's message came from
    // mapConnectError, which already falls back to a generic one.
    const message =
      outcome.kind === "auth-required"
        ? "Sign in required. Refresh the page and enter your admin credentials."
        : outcome.message;
    return (
      <>
        <h1>Proposals</h1>
        <ErrorBanner title="Could not load proposals" message={message} />
      </>
    );
  }

  const rows = query.data.proposals.map(toRow).filter((row): row is ProposalRow => row !== undefined);
  const pagerState =
    query.data.pageInfo === undefined ? undefined : toPagerState(page, query.data.pageInfo);

  return (
    <>
      <h1>Proposals</h1>
      <Table
        caption="Proposals awaiting a decision"
        columns={columns}
        rows={rows}
        rowKey={rowKey}
        emptyMessage="No proposals awaiting decision."
      />
      {pagerState !== undefined && (
        <div className={styles.pager}>
          <Pager
            total={pagerState.total}
            limit={pagerState.limit}
            offset={pagerState.offset}
            onOffsetChange={setOffset}
            itemNoun="proposals"
          />
        </div>
      )}
    </>
  );
}
