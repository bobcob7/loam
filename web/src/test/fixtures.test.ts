import { describe, expect, it } from "vitest";
import {
  UpstreamDrift,
  WorkBranchConflict,
  WorkBranchSchema,
  WorkBranchState,
} from "../gen/loam/v1/common_pb";
import { workBranchFixture } from "./fixtures";

/**
 * The guard that stops loam-mvso's defect from being reintroduced by the next
 * field, rather than only fixing the two fields that had it.
 *
 * A proto3 enum's zero value is `UNSPECIFIED`, so an OMITTED field and a
 * DELIBERATE unset are the same bytes -- and a hand-built fixture omits by
 * default. That makes "did this fixture set every enum?" invisible to review
 * and to `tsc` alike (`MessageInitShape` makes every field optional, as it
 * must). So it is asserted here.
 *
 * WHAT ACTUALLY CATCHES A NEW FIELD IS THE PINNED LIST, NOT THE SWEEP, and
 * the two tests are named accordingly. The sweep reaches less far than
 * "every enum field" sounds: a `repeated` enum arrives as
 * `fieldKind === "list"` and is filtered out, and an `optional`
 * (explicit-presence) enum is `undefined` rather than `0` when unset in
 * protobuf-es v2, so `not.toBe(0)` would PASS on exactly the omission it
 * exists to catch. Both holes are closed here only because the pinned list
 * fails first and forces a human to look. Generalising the sweep properly is
 * loam-yhcz; this file deliberately does not attempt it.
 */
describe("workBranchFixture", () => {
  const enumFields = WorkBranchSchema.fields.filter((field) => field.fieldKind === "enum");

  it("pins WorkBranch's exact enum field list, so a new field fails here first", () => {
    expect(enumFields.map((field) => field.localName).sort()).toEqual([
      "conflict",
      "state",
      "upstreamDrift",
    ]);
  });

  // Singular enum fields only -- see the holes recorded above (loam-yhcz).
  it.each(enumFields.map((field) => [field.localName] as const))(
    "leaves no singular enum field at its UNSPECIFIED zero: %s",
    (localName) => {
      const values = workBranchFixture() as unknown as Record<string, unknown>;
      expect(values[localName]).not.toBe(0);
    },
  );

  it("describes the branch the proposal queue exists to offer: reviewed, clean, undrifted", () => {
    const wb = workBranchFixture();
    expect(wb.state).toBe(WorkBranchState.REVIEWED);
    expect(wb.conflict).toBe(WorkBranchConflict.NONE);
    expect(wb.upstreamDrift).toBe(UpstreamDrift.NONE);
  });

  it("applies overrides last, so a test can vary one field and inherit a faithful rest", () => {
    const wb = workBranchFixture({ conflict: WorkBranchConflict.FLAGGED });
    expect(wb.conflict).toBe(WorkBranchConflict.FLAGGED);
    expect(wb.upstreamDrift).toBe(UpstreamDrift.NONE);
    expect(wb.state).toBe(WorkBranchState.REVIEWED);
  });
});
