import { IngestStatus, SyncState } from "../gen/loam/admin/v1/repo_admin_pb";
import { VerdictOutcome, WorkBranchState } from "../gen/loam/v1/common_pb";
import type { StatusIntent } from "./StatusBadge";

/**
 * The result of mapping a schema enum value onto a {@link StatusIntent}: the
 * intent to tint the pill with, and the label to carry as its text.
 */
export interface StatusBadgeContent {
  readonly intent: StatusIntent;
  readonly label: string;
}

/**
 * Every mapping helper below shares one policy for the two cases that are
 * not one of the enum's *named* members, and the two are deliberately
 * different:
 *
 *  - `*_UNSPECIFIED` (0) is a real, documented member -- proto3's zero value,
 *    meaning "never set". It gets its own `case` and reads as `neutral`,
 *    the same intent as the schema's other "nothing to see here" states
 *    (SYNC_STATE_IDLE, WORK_BRANCH_STATE_DRAFT/CLOSED); tokens.css does not
 *    name it explicitly, but "unset" fits that bucket rather than any other.
 *  - the `default` branch is the one this bead exists for: every generated
 *    enum here is an open `as const` object with a trailing `UnknownEnum`
 *    member (see web/src/gen), so TypeScript will not force -- or even let --
 *    a `switch` be exhaustive over it. `default` is the only thing standing
 *    between the frontend and a runtime value it has never seen (a server
 *    that shipped a new enum member this client has not been regenerated
 *    against). It maps to `warning`, not `neutral` or `danger`: tokens.css
 *    defines `warning` as "any needs-attention state that is not an error",
 *    which is exactly this -- a value the UI does not understand deserves
 *    attention, but calling it `danger` would assert a failure the frontend
 *    has no basis to claim. Every helper is tested against a value outside
 *    its generated union to keep this branch honest.
 */
const UNKNOWN_STATUS: StatusBadgeContent = { intent: "warning", label: "Unknown" };

/** Maps `loam.admin.v1.SyncState` to a status pill (RepoAdminService.ListRepos). */
export function syncStateIntent(state: SyncState): StatusBadgeContent {
  switch (state) {
    case SyncState.UNSPECIFIED:
      return { intent: "neutral", label: "Unspecified" };
    case SyncState.IDLE:
      return { intent: "neutral", label: "Idle" };
    case SyncState.SYNCING:
      return { intent: "info", label: "Syncing" };
    case SyncState.ERROR:
      return { intent: "danger", label: "Error" };
    default:
      return UNKNOWN_STATUS;
  }
}

/** Maps `loam.admin.v1.IngestStatus` to a status pill (RepoAdminService.ListIngestJobs). */
export function ingestStatusIntent(status: IngestStatus): StatusBadgeContent {
  switch (status) {
    case IngestStatus.UNSPECIFIED:
      return { intent: "neutral", label: "Unspecified" };
    case IngestStatus.QUEUED:
      return { intent: "neutral", label: "Queued" };
    case IngestStatus.RUNNING:
      return { intent: "info", label: "Running" };
    case IngestStatus.SUCCEEDED:
      return { intent: "success", label: "Succeeded" };
    case IngestStatus.FAILED:
      return { intent: "danger", label: "Failed" };
    default:
      return UNKNOWN_STATUS;
  }
}

/** Maps `loam.v1.WorkBranchState` to a status pill (proposals queue and detail). */
export function workBranchStateIntent(state: WorkBranchState): StatusBadgeContent {
  switch (state) {
    case WorkBranchState.UNSPECIFIED:
      return { intent: "neutral", label: "Unspecified" };
    case WorkBranchState.DRAFT:
      return { intent: "neutral", label: "Draft" };
    case WorkBranchState.REVIEWABLE:
      return { intent: "info", label: "Reviewable" };
    case WorkBranchState.REVIEWED:
      return { intent: "warning", label: "Reviewed" };
    case WorkBranchState.COMPLETE:
      return { intent: "success", label: "Complete" };
    case WorkBranchState.CLOSED:
      return { intent: "neutral", label: "Closed" };
    default:
      return UNKNOWN_STATUS;
  }
}

/**
 * Maps `loam.v1.VerdictOutcome` to a status pill (a proposal's verdicts).
 *
 * This maps the outcome alone. tokens.css also assigns `neutral` to a
 * *stale* verdict regardless of its outcome (`VerdictSummary.stale`,
 * loam/v1/common.proto) -- staleness is a separate boolean field, not an
 * enum member, so it is out of scope for an enum-to-intent helper and is not
 * folded in here. A screen rendering a `VerdictSummary` decides that for
 * itself, e.g.:
 *
 * ```ts
 * const content = summary.stale
 *   ? { intent: "neutral" as const, label: verdictOutcomeIntent(summary.outcome).label }
 *   : verdictOutcomeIntent(summary.outcome);
 * ```
 */
export function verdictOutcomeIntent(outcome: VerdictOutcome): StatusBadgeContent {
  switch (outcome) {
    case VerdictOutcome.UNSPECIFIED:
      return { intent: "neutral", label: "Unspecified" };
    case VerdictOutcome.APPROVE:
      return { intent: "success", label: "Approve" };
    case VerdictOutcome.DISAPPROVE:
      return { intent: "danger", label: "Disapprove" };
    case VerdictOutcome.NEUTRAL:
      return { intent: "neutral", label: "Neutral" };
    default:
      return UNKNOWN_STATUS;
  }
}
