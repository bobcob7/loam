import { IngestStatus, SyncState } from "../gen/loam/admin/v1/repo_admin_pb";
import {
  UpstreamDrift,
  VerdictOutcome,
  WorkBranchConflict,
  WorkBranchState,
  type VerdictSummary,
} from "../gen/loam/v1/common_pb";
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
 * Maps `loam.v1.WorkBranchConflict` to a status pill, or `undefined` when
 * there is nothing to say.
 *
 * `undefined` covers NONE and UNSPECIFIED together, and that pairing is
 * deliberate rather than a shortcut. NONE means the branch merges cleanly, so
 * a badge would be noise on every healthy proposal. UNSPECIFIED means the
 * field never arrived -- which is what a server older than this field looks
 * like -- and badging every proposal "Unspecified" against such a server would
 * be worse than silence about a state it cannot report. An UNKNOWN value still
 * badges: that is a server NEWER than this client, which is a real
 * needs-attention signal rather than an absence.
 */
export function conflictIntent(conflict: WorkBranchConflict): StatusBadgeContent | undefined {
  switch (conflict) {
    case WorkBranchConflict.UNSPECIFIED:
    case WorkBranchConflict.NONE:
      return undefined;
    case WorkBranchConflict.FLAGGED:
      return { intent: "warning", label: "Conflicted" };
    case WorkBranchConflict.RESET:
      return { intent: "warning", label: "Conflict reset" };
    default:
      return UNKNOWN_STATUS;
  }
}

/**
 * Maps `loam.v1.UpstreamDrift` to a status pill, or `undefined` when there is
 * nothing to say -- the same absence rule as {@link conflictIntent}.
 *
 * DIVERGED is `danger`, not `warning`, and it is a separate badge from the
 * conflict one on purpose. The two describe independent facts that can hold at
 * once, and they call for different operator actions: a conflict is fixed by
 * catching the branch up, while divergence means somebody rewrote the branch
 * Loam pushed and only reconciling it on the forge will clear it
 * (`docs/sync-spec.md` -> Upstream Drift). Collapsing them into one
 * "conflicted" pill would send the admin to fix the wrong thing.
 */
export function upstreamDriftIntent(drift: UpstreamDrift): StatusBadgeContent | undefined {
  switch (drift) {
    case UpstreamDrift.UNSPECIFIED:
    case UpstreamDrift.NONE:
      return undefined;
    case UpstreamDrift.DIVERGED:
      return { intent: "danger", label: "Upstream diverged" };
    default:
      return UNKNOWN_STATUS;
  }
}

/**
 * Maps `loam.v1.VerdictOutcome` to a status pill (a proposal's verdicts).
 *
 * This maps the outcome alone and DELIBERATELY has no knowledge of staleness:
 * tokens.css also assigns `neutral` to a *stale* verdict regardless of its
 * outcome (`VerdictSummary.stale`, loam/v1/common.proto), but staleness is a
 * separate boolean field, not an enum member, so an enum-to-intent helper
 * structurally cannot see it.
 *
 * Do not call this directly on a `VerdictSummary`'s outcome -- a stale
 * APPROVE would render as `success`/"Approve", telling an admin the approval
 * bar is met when the verdict does not count toward it
 * (loam/v1/common.proto: "only non-stale verdicts count toward the approval
 * bar"). Call {@link verdictSummaryIntent} instead, which takes the whole
 * summary and applies the staleness override. This function stays exported
 * because {@link verdictSummaryIntent} is built on it, and because a caller
 * that only has a bare `VerdictOutcome` with no staleness to consider (none
 * exists in this codebase today) would still have a legitimate use for it.
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

/**
 * Maps a whole `loam.v1.VerdictSummary` to a status pill (a proposal's
 * verdicts) -- the function every screen rendering a `VerdictSummary` MUST
 * call instead of `verdictOutcomeIntent(summary.outcome)`.
 *
 * loam/v1/common.proto's doc comment on `VerdictSummary` is the source of
 * truth this enforces: "requesting review marks the prior round's verdicts
 * `stale`, and only non-stale verdicts count toward the approval bar." A
 * stale verdict's `outcome` is unchanged by going stale, but it no longer
 * counts, so it must never keep the outcome's own intent -- most importantly
 * a stale APPROVE must never render as `success`, which would read as "the
 * approval bar is met" when it is not. This function forces `neutral` for
 * any stale verdict, matching tokens.css's own listing of "stale verdicts"
 * under the neutral intent, regardless of `outcome` (including an outcome
 * this build does not recognise -- staleness wins even over the `warning`
 * fallback `verdictOutcomeIntent` gives an unknown outcome).
 *
 * The label also names the staleness explicitly, e.g. "Approve (stale)"
 * rather than a bare "Approve" recoloured neutral: StatusBadge's own contract
 * (see StatusBadge.tsx) is that colour is never the sole carrier of meaning,
 * so a neutral pill that still reads "Approve" is exactly the ambiguous
 * rendering this bead exists to remove -- a admin scanning a dense table by
 * word rather than hue must not be able to mistake it for a counted verdict.
 *
 * `summary.round` is deliberately left out of the label. It is a plain
 * `number` already on the summary, independent of intent, that a screen can
 * render as its own column or alongside the badge (e.g. "Round 2") if the
 * screen judges it useful context; folding it into this pill's text would
 * overload one label with two unrelated facts and every screen would have to
 * parse it back out to lay them out separately.
 */
export function verdictSummaryIntent(summary: VerdictSummary): StatusBadgeContent {
  const content = verdictOutcomeIntent(summary.outcome);
  if (!summary.stale) {
    return content;
  }
  return { intent: "neutral", label: `${content.label} (stale)` };
}
