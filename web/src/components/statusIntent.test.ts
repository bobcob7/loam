import { create } from "@bufbuild/protobuf";
import { IngestStatus, SyncState } from "../gen/loam/admin/v1/repo_admin_pb";
import { VerdictOutcome, VerdictSummarySchema, WorkBranchState, type VerdictSummary } from "../gen/loam/v1/common_pb";
import {
  ingestStatusIntent,
  syncStateIntent,
  verdictOutcomeIntent,
  verdictSummaryIntent,
  workBranchStateIntent,
  type StatusBadgeContent,
} from "./statusIntent";

// Every one of these four generated enums is an open `as const` object with
// a trailing UnknownEnum member (web/src/gen), so TypeScript cannot make a
// `switch` over one exhaustive, let alone catch a member this test forgot.
// Each table below is therefore checked against every *named* member the
// generated file currently declares (`Object.entries(Enum)`, skipping
// UNSPECIFIED which each helper documents separately) rather than a
// hand-picked subset -- if a proto regen adds a member, this test starts
// failing (missing table row) instead of silently passing over it.
//
// The one value no table can contain by construction is the unrecognised
// one: a wire value outside the generated union, reached only through
// `default`. `(999 as unknown as Enum)` is a double assertion, not `!`
// (banned by CLAUDE.md) -- 999 is a `number`, but the enum type is
// `0 | 1 | ... | (number & { brand })`, and no literal number type is
// assignable to that branded intersection, so a single `as` is rejected by
// `tsc` and the double assertion is the documented way through.
describe("syncStateIntent", () => {
  const known: ReadonlyArray<readonly [SyncState, StatusBadgeContent]> = [
    [SyncState.IDLE, { intent: "neutral", label: "Idle" }],
    [SyncState.SYNCING, { intent: "info", label: "Syncing" }],
    [SyncState.ERROR, { intent: "danger", label: "Error" }],
  ];

  it("covers every named SyncState member besides UNSPECIFIED", () => {
    const namedValues = Object.entries(SyncState)
      .filter(([name]) => name !== "UNSPECIFIED")
      .map(([, value]) => value);
    expect(known.map(([value]) => value).sort()).toEqual(namedValues.sort());
  });

  it.each(known)("maps %s to %o", (value, expected) => {
    expect(syncStateIntent(value)).toEqual(expected);
  });

  it("maps the unset zero value to neutral, distinctly from an unrecognised one", () => {
    expect(syncStateIntent(SyncState.UNSPECIFIED)).toEqual({ intent: "neutral", label: "Unspecified" });
  });

  it("falls back to a deliberate warning, not neutral or danger, for a value outside the generated union", () => {
    expect(syncStateIntent(999 as unknown as SyncState)).toEqual({ intent: "warning", label: "Unknown" });
  });
});

describe("ingestStatusIntent", () => {
  const known: ReadonlyArray<readonly [IngestStatus, StatusBadgeContent]> = [
    [IngestStatus.QUEUED, { intent: "neutral", label: "Queued" }],
    [IngestStatus.RUNNING, { intent: "info", label: "Running" }],
    [IngestStatus.SUCCEEDED, { intent: "success", label: "Succeeded" }],
    [IngestStatus.FAILED, { intent: "danger", label: "Failed" }],
  ];

  it("covers every named IngestStatus member besides UNSPECIFIED", () => {
    const namedValues = Object.entries(IngestStatus)
      .filter(([name]) => name !== "UNSPECIFIED")
      .map(([, value]) => value);
    expect(known.map(([value]) => value).sort()).toEqual(namedValues.sort());
  });

  it.each(known)("maps %s to %o", (value, expected) => {
    expect(ingestStatusIntent(value)).toEqual(expected);
  });

  it("maps the unset zero value to neutral, distinctly from an unrecognised one", () => {
    expect(ingestStatusIntent(IngestStatus.UNSPECIFIED)).toEqual({
      intent: "neutral",
      label: "Unspecified",
    });
  });

  it("falls back to a deliberate warning, not neutral or danger, for a value outside the generated union", () => {
    expect(ingestStatusIntent(999 as unknown as IngestStatus)).toEqual({
      intent: "warning",
      label: "Unknown",
    });
  });
});

describe("workBranchStateIntent", () => {
  const known: ReadonlyArray<readonly [WorkBranchState, StatusBadgeContent]> = [
    [WorkBranchState.DRAFT, { intent: "neutral", label: "Draft" }],
    [WorkBranchState.REVIEWABLE, { intent: "info", label: "Reviewable" }],
    [WorkBranchState.REVIEWED, { intent: "warning", label: "Reviewed" }],
    [WorkBranchState.COMPLETE, { intent: "success", label: "Complete" }],
    [WorkBranchState.CLOSED, { intent: "neutral", label: "Closed" }],
  ];

  it("covers every named WorkBranchState member besides UNSPECIFIED", () => {
    const namedValues = Object.entries(WorkBranchState)
      .filter(([name]) => name !== "UNSPECIFIED")
      .map(([, value]) => value);
    expect(known.map(([value]) => value).sort()).toEqual(namedValues.sort());
  });

  it.each(known)("maps %s to %o", (value, expected) => {
    expect(workBranchStateIntent(value)).toEqual(expected);
  });

  it("maps the unset zero value to neutral, distinctly from an unrecognised one", () => {
    expect(workBranchStateIntent(WorkBranchState.UNSPECIFIED)).toEqual({
      intent: "neutral",
      label: "Unspecified",
    });
  });

  it("falls back to a deliberate warning, not neutral or danger, for a value outside the generated union", () => {
    expect(workBranchStateIntent(999 as unknown as WorkBranchState)).toEqual({
      intent: "warning",
      label: "Unknown",
    });
  });
});

describe("verdictOutcomeIntent", () => {
  const known: ReadonlyArray<readonly [VerdictOutcome, StatusBadgeContent]> = [
    [VerdictOutcome.APPROVE, { intent: "success", label: "Approve" }],
    [VerdictOutcome.DISAPPROVE, { intent: "danger", label: "Disapprove" }],
    [VerdictOutcome.NEUTRAL, { intent: "neutral", label: "Neutral" }],
  ];

  it("covers every named VerdictOutcome member besides UNSPECIFIED", () => {
    const namedValues = Object.entries(VerdictOutcome)
      .filter(([name]) => name !== "UNSPECIFIED")
      .map(([, value]) => value);
    expect(known.map(([value]) => value).sort()).toEqual(namedValues.sort());
  });

  it.each(known)("maps %s to %o", (value, expected) => {
    expect(verdictOutcomeIntent(value)).toEqual(expected);
  });

  it("maps the unset zero value to neutral, distinctly from an unrecognised one", () => {
    expect(verdictOutcomeIntent(VerdictOutcome.UNSPECIFIED)).toEqual({
      intent: "neutral",
      label: "Unspecified",
    });
  });

  it("falls back to a deliberate warning, not neutral or danger, for a value outside the generated union", () => {
    expect(verdictOutcomeIntent(999 as unknown as VerdictOutcome)).toEqual({
      intent: "warning",
      label: "Unknown",
    });
  });
});

// verdictSummaryIntent is the fix for loam-2xe6: VerdictSummary.stale is a
// bool FIELD (field 3 on the message), not a VerdictOutcome enum member, so
// verdictOutcomeIntent structurally cannot see it. Requesting review marks
// the prior round's verdicts stale, and only non-stale verdicts count toward
// the approval bar (loam/v1/common.proto, VerdictSummary doc comment) -- a
// screen that renders a stale APPROVE via the outcome alone would show
// success/green "Approve", telling an admin the approval bar is met when it
// is not. Every case below is built from a real create()'d VerdictSummary,
// never a bare outcome, so the test exercises the same shape a screen does.
describe("verdictSummaryIntent", () => {
  const summary = (outcome: VerdictOutcome, stale: boolean, round: number): VerdictSummary =>
    create(VerdictSummarySchema, { reviewer: "agent-7-reviewer", outcome, stale, round });

  const knownNonStale: ReadonlyArray<readonly [VerdictOutcome, StatusBadgeContent]> = [
    [VerdictOutcome.APPROVE, { intent: "success", label: "Approve" }],
    [VerdictOutcome.DISAPPROVE, { intent: "danger", label: "Disapprove" }],
    [VerdictOutcome.NEUTRAL, { intent: "neutral", label: "Neutral" }],
  ];

  it("covers every named VerdictOutcome member besides UNSPECIFIED", () => {
    const namedValues = Object.entries(VerdictOutcome)
      .filter(([name]) => name !== "UNSPECIFIED")
      .map(([, value]) => value);
    expect(knownNonStale.map(([value]) => value).sort()).toEqual(namedValues.sort());
  });

  it.each(knownNonStale)(
    "passes a non-stale %s verdict through to verdictOutcomeIntent unchanged",
    (outcome, expected) => {
      expect(verdictSummaryIntent(summary(outcome, false, 1))).toEqual(expected);
    },
  );

  it.each(knownNonStale)(
    "forces neutral and appends '(stale)' to the label once a %s verdict goes stale",
    (outcome, expected) => {
      expect(verdictSummaryIntent(summary(outcome, true, 2))).toEqual({
        intent: "neutral",
        label: `${expected.label} (stale)`,
      });
    },
  );

  it("renders a stale APPROVE as neutral, not success -- the case that misleads an admin about the approval bar", () => {
    const result = verdictSummaryIntent(summary(VerdictOutcome.APPROVE, true, 3));
    expect(result.intent).not.toBe("success");
    expect(result).toEqual({ intent: "neutral", label: "Approve (stale)" });
  });

  it("keeps the unset zero value neutral, and still appends '(stale)' once it goes stale", () => {
    expect(verdictSummaryIntent(summary(VerdictOutcome.UNSPECIFIED, true, 1))).toEqual({
      intent: "neutral",
      label: "Unspecified (stale)",
    });
  });

  it("passes an unrecognised outcome through as the warning fallback when not stale", () => {
    expect(verdictSummaryIntent(summary(999 as unknown as VerdictOutcome, false, 1))).toEqual({
      intent: "warning",
      label: "Unknown",
    });
  });

  it("overrides even an unrecognised outcome's warning fallback to neutral once stale", () => {
    expect(verdictSummaryIntent(summary(999 as unknown as VerdictOutcome, true, 1))).toEqual({
      intent: "neutral",
      label: "Unknown (stale)",
    });
  });

  it("does not fold `round` into the intent or label -- two stale APPROVEs from different rounds render identically", () => {
    const earlyRound = verdictSummaryIntent(summary(VerdictOutcome.APPROVE, true, 1));
    const laterRound = verdictSummaryIntent(summary(VerdictOutcome.APPROVE, true, 9));
    expect(earlyRound).toEqual({ intent: "neutral", label: "Approve (stale)" });
    expect(laterRound).toEqual({ intent: "neutral", label: "Approve (stale)" });
  });
});
