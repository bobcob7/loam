import { IngestStatus, SyncState } from "../gen/loam/admin/v1/repo_admin_pb";
import { VerdictOutcome, WorkBranchState } from "../gen/loam/v1/common_pb";
import {
  ingestStatusIntent,
  syncStateIntent,
  verdictOutcomeIntent,
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
