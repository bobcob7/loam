import { useQuery } from "@connectrpc/connect-query";
import type { ReactElement } from "react";
import { useState } from "react";
import { Button } from "../components/Button";
import { CopyField } from "../components/CopyField";
import { Dialog } from "../components/Dialog";
import { ErrorBanner } from "../components/ErrorBanner";
import { Field } from "../components/Field";
import { Form, FormActions } from "../components/Form";
import { Markdown } from "../components/Markdown";
import { Pager } from "../components/Pager";
import { StatusBadge } from "../components/StatusBadge";
import { Table, type TableColumn } from "../components/Table";
import { verdictSummaryIntent, workBranchStateIntent } from "../components/statusIntent";
import { useMutationInvalidating } from "../data/invalidation";
import { mapConnectError, type ErrorOutcome } from "../data/mapConnectError";
import { toPagerState, useOffsetPagination } from "../data/pagination";
import {
  acceptProposal,
  closeWorkBranch,
  listProposals,
} from "../gen/loam/admin/v1/proposal-ProposalService_connectquery";
import type { Thread, VerdictSummary } from "../gen/loam/v1/common_pb";
import {
  getWorkBranch,
  getWorkBranchDiff,
  listComments,
  listVerdicts,
  requestReview,
} from "../gen/loam/v1/workbranch-WorkBranchService_connectquery";
import styles from "./ProposalDetail.module.css";

export interface ProposalDetailProps {
  /** The enrolled repo identifier in its wire form, `<group>/<repo_name>`. */
  readonly repo: string;
  /** The work branch name, `wb-<hex>` — unique within the repo. */
  readonly workBranch: string;
}

/**
 * Renders a mapped error's message. `auth-required` carries no `message` of
 * its own (docs/web-frontend-spec.md -> Auth: there is no login page, only a
 * browser refresh re-triggers basic auth), so this is the one place that
 * spells out what the admin should do instead of reading an undefined value.
 */
function errorMessage(outcome: ErrorOutcome): string {
  if (outcome.kind === "auth-required") {
    return "Authentication required. Refresh the page to sign in again.";
  }
  return outcome.message;
}

/** A thread's anchor rendered as a location, or "General comment" when unanchored. */
function anchorLabel(thread: Thread): string {
  if (thread.anchor === undefined) return "General comment";
  return thread.anchor.line === undefined
    ? thread.anchor.file
    : `${thread.anchor.file}:${thread.anchor.line}`;
}

const verdictRowKey = (verdict: VerdictSummary): string => `${verdict.reviewer}-${verdict.round}`;

const verdictColumns: readonly TableColumn<VerdictSummary>[] = [
  { key: "reviewer", header: "Reviewer", cell: (verdict) => verdict.reviewer, rowHeader: true, mono: true },
  {
    key: "outcome",
    header: "Outcome",
    // MUST call verdictSummaryIntent, never verdictOutcomeIntent(verdict.outcome):
    // only that helper applies the staleness override so a stale APPROVE
    // renders neutral rather than the green "Approve" that would tell an
    // admin the approval bar is met when this verdict no longer counts
    // (proto/loam/v1/common.proto: VerdictSummary -- loam-2xe6).
    cell: (verdict) => {
      const content = verdictSummaryIntent(verdict);
      return <StatusBadge intent={content.intent}>{content.label}</StatusBadge>;
    },
  },
  // Carried as its own column rather than folded into the outcome label
  // (nvb.6's guidance for this screen, and statusIntent.ts's own doc comment
  // on verdictSummaryIntent): an admin scanning the table needs stale/round
  // as facts distinct from the coloured outcome pill.
  { key: "stale", header: "Stale", cell: (verdict) => (verdict.stale ? "Yes" : "No") },
  { key: "round", header: "Round", cell: (verdict) => String(verdict.round), align: "end" },
];

/**
 * Proposal detail (`/proposals/:group/:name/:workBranch`) — the review, its
 * diff, threads and verdicts, plus the admin decision.
 *
 * Queries `loam.v1.WorkBranchService` directly (GetWorkBranch,
 * GetWorkBranchDiff, ListComments, ListVerdicts) as a superuser, and
 * `loam.admin.v1.ProposalService` for the two terminal decisions
 * (AcceptProposal, CloseWorkBranch); RequestReview -- sending a reviewed
 * branch back for another round -- is the shared WorkBranchService
 * operation reached the same way an author reaches it. Per this bead's own
 * NOTES and docs/web-spec.md ("There is no send-back comment; the admin's
 * feedback ... lives in the work branch's threads"), RequestReview here is a
 * plain confirmation with no comment field, even though the generated
 * request type still carries one; feedback goes through ReplyToThread on the
 * relevant thread instead.
 */
export function ProposalDetail({ repo, workBranch }: ProposalDetailProps): ReactElement {
  const workBranchQuery = useQuery(getWorkBranch, { repo, workBranch });
  const diffQuery = useQuery(getWorkBranchDiff, { repo, workBranch });
  const comments = useOffsetPagination();
  const commentsQuery = useQuery(listComments, { repo, workBranch, page: comments.page });
  const verdictsQuery = useQuery(listVerdicts, { repo, workBranch });

  const [acceptOpen, setAcceptOpen] = useState(false);
  const [closeOpen, setCloseOpen] = useState(false);
  const [requestReviewOpen, setRequestReviewOpen] = useState(false);
  const [closeBody, setCloseBody] = useState("");

  const acceptMutation = useMutationInvalidating(
    acceptProposal,
    [{ schema: listProposals }, { schema: getWorkBranch }],
    { onSuccess: () => setAcceptOpen(false) },
  );
  const closeMutation = useMutationInvalidating(
    closeWorkBranch,
    [{ schema: listProposals }, { schema: getWorkBranch }],
    {
      onSuccess: () => {
        setCloseOpen(false);
        setCloseBody("");
      },
    },
  );
  const requestReviewMutation = useMutationInvalidating(
    requestReview,
    [{ schema: getWorkBranch }, { schema: listVerdicts }, { schema: listProposals }],
    { onSuccess: () => setRequestReviewOpen(false) },
  );

  if (workBranchQuery.isPending) {
    // The identifiers come from the route, not from the fetch, so they are
    // renderable before anything settles -- matching RepoDetail, which keeps
    // its `<h1>` while pending. Two reasons this is not cosmetic: a user who
    // followed a link to a slow proposal otherwise sees a bare "Loading" with
    // no confirmation of WHICH proposal, and AppRoutes.test.tsx proves the
    // route table round-trips `<group>/<name>/<workBranch>` into props by
    // reading exactly these values back out of the rendered screen.
    return (
      <>
        <h1>{workBranch}</h1>
        <p>
          <span>{repo}</span> — loading proposal…
        </p>
      </>
    );
  }
  if (workBranchQuery.isError) {
    const outcome = mapConnectError(workBranchQuery.error);
    if (outcome.kind === "not-found") {
      return (
        <ErrorBanner
          title="Proposal not found"
          message="This work branch does not exist, or its repo is not enrolled."
        />
      );
    }
    return <ErrorBanner title="Could not load proposal" message={errorMessage(outcome)} />;
  }
  const wb = workBranchQuery.data.workBranch;
  if (wb === undefined) {
    return (
      <ErrorBanner
        title="Proposal not found"
        message="This work branch does not exist, or its repo is not enrolled."
      />
    );
  }

  const stateBadge = workBranchStateIntent(wb.state);
  const verdicts = verdictsQuery.data?.verdicts ?? [];
  const threads = commentsQuery.data?.threads ?? [];
  const alreadyAccepted = wb.upstreamPrUrl !== undefined || acceptMutation.isSuccess;

  return (
    <>
      <h1>{wb.title}</h1>
      <p className={styles.meta}>
        <StatusBadge intent={stateBadge.intent}>{stateBadge.label}</StatusBadge>
        <span className={styles.metaItem}>
          {repo} / <span className={styles.mono}>{workBranch}</span>
        </span>
        <span className={styles.metaItem}>
          Target: <span className={styles.mono}>{wb.target}</span>
        </span>
        <span className={styles.metaItem}>Author: {wb.author}</span>
      </p>
      {/* Agent-authored and untrusted: it goes through the shared renderer,
          never into markup of its own (components/Markdown.tsx). */}
      <Markdown source={wb.description} />

      <section className={styles.section}>
        <h2>Diff</h2>
        {diffQuery.isPending && <p>Loading diff…</p>}
        {diffQuery.isError && (
          <ErrorBanner title="Could not load diff" message={errorMessage(mapConnectError(diffQuery.error))} />
        )}
        {diffQuery.data !== undefined && <pre className={styles.diff}>{diffQuery.data.diff}</pre>}
      </section>

      <section className={styles.section}>
        <h2>Verdicts</h2>
        {verdictsQuery.isError ? (
          <ErrorBanner
            title="Could not load verdicts"
            message={errorMessage(mapConnectError(verdictsQuery.error))}
          />
        ) : (
          <Table
            caption="Verdicts"
            columns={verdictColumns}
            rows={verdicts}
            rowKey={verdictRowKey}
            emptyMessage="No verdicts yet."
          />
        )}
      </section>

      <section className={styles.section}>
        <h2>Comments</h2>
        {commentsQuery.isError && (
          <ErrorBanner
            title="Could not load comments"
            message={errorMessage(mapConnectError(commentsQuery.error))}
          />
        )}
        {commentsQuery.data !== undefined && (
          <>
            {threads.length === 0 ? (
              <p>No comment threads yet.</p>
            ) : (
              <ul className={styles.threadList}>
                {threads.map((thread) => (
                  <li key={thread.id} className={styles.thread}>
                    <p className={styles.threadHeading}>
                      <span className={styles.mono}>{anchorLabel(thread)}</span>
                      {thread.resolved && <StatusBadge intent="success">Resolved</StatusBadge>}
                    </p>
                    <ul className={styles.commentList}>
                      {thread.comments.map((comment, index) => (
                        <li key={`${thread.id}-${index}`}>
                          <span className={styles.commentAuthor}>{comment.author}</span>
                          {` (round ${comment.round}): `}
                          {comment.body}
                        </li>
                      ))}
                    </ul>
                  </li>
                ))}
              </ul>
            )}
            {commentsQuery.data.pageInfo !== undefined && (
              <Pager
                {...toPagerState(comments.page, commentsQuery.data.pageInfo)}
                onOffsetChange={comments.setOffset}
                itemNoun="threads"
              />
            )}
          </>
        )}
      </section>

      <section className={styles.section}>
        <h2>Decision</h2>
        {alreadyAccepted && (
          <div className={styles.acceptedPanel}>
            <p>This proposal has an upstream pull request.</p>
            {acceptMutation.isSuccess ? (
              <>
                <CopyField label="pull request URL" value={acceptMutation.data.prUrl} />
                <CopyField label="upstream branch" value={acceptMutation.data.upstreamBranch} />
              </>
            ) : (
              wb.upstreamPrUrl !== undefined && (
                <CopyField label="pull request URL" value={wb.upstreamPrUrl} />
              )
            )}
          </div>
        )}
        <div className={styles.actions}>
          {!alreadyAccepted && (
            <Button variant="primary" onClick={() => setAcceptOpen(true)}>
              Accept
            </Button>
          )}
          <Button variant="secondary" onClick={() => setRequestReviewOpen(true)}>
            Request another review round
          </Button>
          <Button variant="danger" onClick={() => setCloseOpen(true)}>
            Close proposal
          </Button>
        </div>
      </section>

      <Dialog
        open={acceptOpen}
        title="Accept proposal"
        description="Opens a pull request on the upstream forge with this work branch's title and description."
        onClose={() => setAcceptOpen(false)}
        footer={
          <>
            <Button variant="secondary" onClick={() => setAcceptOpen(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              pending={acceptMutation.isPending}
              onClick={() => acceptMutation.mutate({ repo, workBranch })}
            >
              Accept
            </Button>
          </>
        }
      >
        {acceptMutation.isError && (
          <ErrorBanner
            title="Could not accept proposal"
            message={
              mapConnectError(acceptMutation.error).kind === "failed-precondition"
                ? "This proposal needs at least one non-stale approve verdict before it can be accepted."
                : errorMessage(mapConnectError(acceptMutation.error))
            }
          />
        )}
        <p>
          Open an upstream pull request for <strong>{wb.title}</strong>?
        </p>
      </Dialog>

      <Dialog
        open={requestReviewOpen}
        title="Request another review round"
        description="Sends this work branch back to REVIEWABLE and marks the current round's verdicts stale."
        onClose={() => setRequestReviewOpen(false)}
        footer={
          <>
            <Button variant="secondary" onClick={() => setRequestReviewOpen(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              pending={requestReviewMutation.isPending}
              onClick={() => requestReviewMutation.mutate({ repo, workBranch })}
            >
              Request review
            </Button>
          </>
        }
      >
        {requestReviewMutation.isError && (
          <ErrorBanner
            title="Could not request review"
            message={errorMessage(mapConnectError(requestReviewMutation.error))}
          />
        )}
        <p>
          This does not take a comment here — leave feedback on a thread first with Reply, then send
          the branch back.
        </p>
      </Dialog>

      <Dialog
        open={closeOpen}
        title="Close proposal"
        description="Closes the work branch without opening a pull request."
        onClose={() => setCloseOpen(false)}
      >
        {closeMutation.isError && (
          <ErrorBanner
            title="Could not close proposal"
            message={errorMessage(mapConnectError(closeMutation.error))}
          />
        )}
        <Form onSubmit={() => closeMutation.mutate({ repo, workBranch, body: closeBody })}>
          <Field
            as="textarea"
            label="Reason"
            required
            value={closeBody}
            onChange={(event) => setCloseBody(event.target.value)}
          />
          <FormActions>
            <Button type="button" variant="secondary" onClick={() => setCloseOpen(false)}>
              Cancel
            </Button>
            <Button type="submit" variant="danger" pending={closeMutation.isPending}>
              Close proposal
            </Button>
          </FormActions>
        </Form>
      </Dialog>
    </>
  );
}
