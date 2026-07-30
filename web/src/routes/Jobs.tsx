import { create } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import type { ChangeEvent, ReactElement } from "react";
import { useState } from "react";
import { Link } from "wouter";
import { Button } from "../components/Button";
import { ErrorBanner } from "../components/ErrorBanner";
import { Field } from "../components/Field";
import { Form, FormActions } from "../components/Form";
import { Pager } from "../components/Pager";
import { StatusBadge } from "../components/StatusBadge";
import { ingestStatusIntent } from "../components/statusIntent";
import { Table, type TableColumn } from "../components/Table";
import { useMutationInvalidating } from "../data/invalidation";
import { mapConnectError, type ErrorOutcome } from "../data/mapConnectError";
import { toPagerState, useOffsetPagination } from "../data/pagination";
import {
  IngestKind,
  IngestStatus,
  ListIngestJobsRequestSchema,
  ReindexRepoRequestSchema,
  type IngestJob,
} from "../gen/loam/admin/v1/repo_admin_pb";
import {
  listIngestJobs,
  reindexRepo,
} from "../gen/loam/admin/v1/repo_admin-RepoAdminService_connectquery";
import styles from "./Jobs.module.css";
import { repoDetailPath } from "./paths";

/**
 * How often the Jobs screen polls `ListIngestJobs` while at least one
 * returned job is still in flight (QUEUED or RUNNING). Matches the
 * `staleTime` the shared `QueryClient` already commits to for the same
 * reason (src/queryClient.ts: "an ingest job's status changes without any
 * action from this browser"): polling any faster would refetch inside a
 * window the client already considers fresh, and any slower would leave a
 * finished job looking stuck for longer than the client's own staleness
 * budget. This is the one screen loam-nvb.14's data layer left for its
 * owner to decide (queryClient.ts: "loam-nvb.14 owns that").
 *
 * Two things are deliberately NOT handled here, because they are already
 * handled elsewhere:
 *   - Pausing while the tab is hidden: `refetchIntervalInBackground` is left
 *     at its TanStack default of `false`, so the interval simply does not
 *     fire while `document.visibilityState` is "hidden" -- no bespoke
 *     visibility listener needed.
 *   - Refreshing on return to the tab / network reconnect:
 *     `refetchOnWindowFocus`/`refetchOnReconnect` are already `true` on the
 *     shared client (src/queryClient.ts), so switching back to this tab
 *     after watching a reindex run elsewhere shows the finished job even if
 *     the interval had stopped because every job looked terminal before you
 *     left.
 */
export const jobsPollIntervalMs = 5_000;

/** The two `IngestStatus` members that mean "still being worked on". */
const nonTerminalIngestStatuses: ReadonlySet<IngestStatus> = new Set([
  IngestStatus.QUEUED,
  IngestStatus.RUNNING,
]);

/**
 * The polling gate: keep polling only while the page currently on screen
 * contains a job that might still change (QUEUED or RUNNING); stop the
 * moment every visible job is terminal (SUCCEEDED/FAILED) or the list is
 * empty, so an admin who leaves this screen open on an idle repo does not
 * put a permanent request floor under the tab -- exactly what
 * src/queryClient.ts declined to do globally.
 *
 * A wire status this client does not recognise (`UNSPECIFIED`, or a future
 * enum member -- see web/src/gen's trailing `UnknownEnum`) is treated as
 * terminal, not active. That is a deliberate, conservative choice: guessing
 * "still active" for an unrecognised value risks polling forever on a repo
 * whose job is, in fact, done; guessing "terminal" only costs the admin a
 * manual refresh to see an update this client could not have interpreted
 * anyway. It is intentionally NOT a `switch` with a `default` over
 * `IngestStatus` (CLAUDE.md / docs/web-frontend-spec.md: generated enums are
 * open `as const` objects with a trailing `UnknownEnum`, so a switch's
 * `default` silently absorbs a value this client has never seen) -- a `Set`
 * membership test degrades the same way but without hiding a `default`
 * branch that looks like it was handling something it was not.
 */
export function jobsRefetchInterval(jobs: readonly IngestJob[]): number | false {
  return jobs.some((job) => nonTerminalIngestStatuses.has(job.status)) ? jobsPollIntervalMs : false;
}

/** Plain-text label for `IngestKind` -- there is no status-pill intent for
 * it (only IngestStatus/SyncState/WorkBranchState/VerdictOutcome have one in
 * ./statusIntent, which this screen does not own), so it renders as a bare
 * table cell rather than a `StatusBadge`. Exported so Jobs.test.tsx can run
 * the same "every named member besides UNSPECIFIED is covered" guard
 * ./statusIntent.test.ts uses, since a proto regen could add a member here
 * too and this switch is just as unable to be exhaustive over it.
 */
export function ingestKindLabel(kind: IngestKind): string {
  switch (kind) {
    case IngestKind.UNSPECIFIED:
      return "Unspecified";
    case IngestKind.INCREMENTAL:
      return "Incremental";
    case IngestKind.FULL:
      return "Full";
    default:
      return "Unknown";
  }
}

/** RFC 3339 timestamp -> a locale-formatted string, "—" for the empty ones
 * (`started_at`/`finished_at` before they occur, proto/loam/admin/v1/repo_admin.proto). */
export function formatTimestamp(value: string): string {
  if (value === "") return "—";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

/**
 * IngestJob's own `id` (proto/loam/admin/v1/repo_admin.proto field 10,
 * sourced from internal/ingest.JobRecord.ID, a uuid.UUID) is the row's
 * actual stable identity, and every job this screen renders comes from
 * ListIngestJobs, which always populates it (internal/handler/repoadmin
 * jobs.go's toIngestJobProto). Prior to loam-1wpa, IngestJob carried no id
 * across the wire at all, so this screen keyed rows on the tuple
 * (repo, target branch, kind, queued_at) -- two distinct jobs for the same
 * repo/branch/kind enqueued in the same instant collided under that key.
 */
export function jobRowKey(job: IngestJob): string {
  return job.id;
}

type StatusFilterValue = "" | "queued" | "running" | "succeeded" | "failed";

const statusFilterOptions: ReadonlyArray<{
  readonly value: StatusFilterValue;
  readonly label: string;
}> = [
  { value: "", label: "All statuses" },
  { value: "queued", label: "Queued" },
  { value: "running", label: "Running" },
  { value: "succeeded", label: "Succeeded" },
  { value: "failed", label: "Failed" },
];

/** `StatusFilterValue` is a plain closed union this screen owns (not a
 * generated proto enum), so this switch is genuinely exhaustive -- adding a
 * member to the union without a case here is a compile error, unlike a
 * switch over `IngestStatus` itself. */
function statusFromFilterValue(value: StatusFilterValue): IngestStatus | undefined {
  switch (value) {
    case "queued":
      return IngestStatus.QUEUED;
    case "running":
      return IngestStatus.RUNNING;
    case "succeeded":
      return IngestStatus.SUCCEEDED;
    case "failed":
      return IngestStatus.FAILED;
    case "":
      return undefined;
  }
}

/** `mapConnectError`'s "auth-required" outcome carries no `message` (the SPA
 * has no login screen to point at -- docs/web-frontend-spec.md -> Auth), so
 * this is the one place that needs its own copy for it. */
function errorBannerMessage(outcome: ErrorOutcome): string {
  if (outcome.kind === "auth-required") return "Sign in required. Reload the page to sign in again.";
  return outcome.message;
}

/**
 * Jobs (`/jobs`) -- ingest activity across every enrolled repo
 * (docs/web-frontend-spec.md -> Routing & Screens; loam-nvb.14).
 *
 * Polls `ListIngestJobs` every `jobsPollIntervalMs` while the current page
 * holds a non-terminal job (see `jobsRefetchInterval`); a background poll
 * failure keeps showing the last known jobs with an inline `ErrorBanner`
 * above the table rather than discarding them (`query.isRefetchError`) --
 * only a failure with no prior data (`query.isLoadingError`) replaces the
 * screen. Neither error state re-specifies retry: the shared `QueryClient`
 * (src/queryClient.ts) already owns that policy.
 */
export function Jobs(): ReactElement {
  const pagination = useOffsetPagination();
  const [repoDraft, setRepoDraft] = useState("");
  const [appliedRepo, setAppliedRepo] = useState<string | undefined>(undefined);
  const [statusValue, setStatusValue] = useState<StatusFilterValue>("");

  const query = useQuery(
    listIngestJobs,
    create(ListIngestJobsRequestSchema, {
      page: pagination.page,
      repo: appliedRepo,
      status: statusFromFilterValue(statusValue),
    }),
    {
      refetchInterval: (activeQuery) => jobsRefetchInterval(activeQuery.state.data?.jobs ?? []),
    },
  );

  const reindexMutation = useMutationInvalidating(reindexRepo, [{ schema: listIngestJobs }]);

  const handleFilterSubmit = (): void => {
    const trimmed = repoDraft.trim();
    setAppliedRepo(trimmed === "" ? undefined : trimmed);
    pagination.reset();
  };

  const handleStatusChange = (event: ChangeEvent<HTMLSelectElement>): void => {
    setStatusValue(event.target.value as StatusFilterValue);
    pagination.reset();
  };

  const handleClearFilters = (): void => {
    setRepoDraft("");
    setAppliedRepo(undefined);
    setStatusValue("");
    pagination.reset();
  };

  const handleReindex = (): void => {
    const trimmed = repoDraft.trim();
    if (trimmed === "") return;
    setAppliedRepo(trimmed);
    pagination.reset();
    reindexMutation.mutate(create(ReindexRepoRequestSchema, { repo: trimmed }));
  };

  const columns: readonly TableColumn<IngestJob>[] = [
    {
      key: "repo",
      header: "Repo",
      mono: true,
      rowHeader: true,
      cell: (job) => <Link href={repoDetailPath(job.repo)}>{job.repo}</Link>,
    },
    { key: "branch", header: "Branch", mono: true, cell: (job) => job.targetBranch },
    { key: "kind", header: "Kind", cell: (job) => ingestKindLabel(job.kind) },
    {
      key: "status",
      header: "Status",
      cell: (job) => {
        const content = ingestStatusIntent(job.status);
        return <StatusBadge intent={content.intent}>{content.label}</StatusBadge>;
      },
    },
    { key: "attempts", header: "Attempts", align: "end", cell: (job) => String(job.attempts) },
    { key: "queuedAt", header: "Queued", cell: (job) => formatTimestamp(job.queuedAt) },
    { key: "startedAt", header: "Started", cell: (job) => formatTimestamp(job.startedAt) },
    { key: "finishedAt", header: "Finished", cell: (job) => formatTimestamp(job.finishedAt) },
    { key: "error", header: "Error", cell: (job) => (job.error === "" ? "—" : job.error) },
  ];

  const filters = (
    <Form onSubmit={handleFilterSubmit} aria-label="Filter ingest jobs">
      <div className={styles.filterRow}>
        <Field
          label="Repo"
          hint="Also the target for Reindex repo, below."
          value={repoDraft}
          onChange={(event) => setRepoDraft(event.target.value)}
          placeholder="acme/widgets"
        />
        <Field as="select" label="Status" value={statusValue} onChange={handleStatusChange}>
          {statusFilterOptions.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </Field>
      </div>
      <FormActions>
        <Button type="submit" variant="primary">
          Apply filters
        </Button>
        <Button type="button" variant="secondary" onClick={handleClearFilters}>
          Clear filters
        </Button>
        <Button
          type="button"
          variant="secondary"
          disabled={repoDraft.trim() === ""}
          pending={reindexMutation.isPending}
          onClick={handleReindex}
        >
          Reindex repo
        </Button>
      </FormActions>
    </Form>
  );

  if (query.isPending) {
    return (
      <section className={styles.root}>
        <h1>Jobs</h1>
        {filters}
        <p>Loading jobs…</p>
      </section>
    );
  }

  if (query.data === undefined) {
    const outcome = mapConnectError(query.error);
    return (
      <section className={styles.root}>
        <h1>Jobs</h1>
        {filters}
        <ErrorBanner title="Could not load ingest jobs" message={errorBannerMessage(outcome)}>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            Retry
          </Button>
        </ErrorBanner>
      </section>
    );
  }

  const jobs = query.data.jobs;
  const pagerState =
    query.data.pageInfo === undefined ? undefined : toPagerState(pagination.page, query.data.pageInfo);

  return (
    <section className={styles.root}>
      <h1>Jobs</h1>
      {filters}
      {query.isError ? (
        <ErrorBanner title="Could not refresh ingest jobs" message={errorBannerMessage(mapConnectError(query.error))}>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            Retry
          </Button>
        </ErrorBanner>
      ) : null}
      {reindexMutation.isError ? (
        <ErrorBanner
          title="Could not reindex repo"
          message={errorBannerMessage(mapConnectError(reindexMutation.error))}
        >
          <Button variant="secondary" onClick={() => reindexMutation.reset()}>
            Dismiss
          </Button>
        </ErrorBanner>
      ) : null}
      <Table
        caption="Ingest jobs"
        columns={columns}
        rows={jobs}
        rowKey={jobRowKey}
        stickyHeader
        emptyMessage="No ingest jobs match these filters."
      />
      {pagerState === undefined ? null : (
        <Pager
          total={pagerState.total}
          limit={pagerState.limit}
          offset={pagerState.offset}
          onOffsetChange={pagination.setOffset}
          itemNoun="jobs"
        />
      )}
    </section>
  );
}
