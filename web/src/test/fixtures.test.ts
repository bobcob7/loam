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
 * must). So it is asserted here, driven off the schema's own field list
 * rather than a hand-written checklist: add an enum field to
 * `loam.v1.WorkBranch` and this fails until {@link workBranchFixture} says
 * what a healthy branch's value for it is.
 */
describe("workBranchFixture", () => {
  const enumFields = WorkBranchSchema.fields.filter((field) => field.fieldKind === "enum");

  it("has enum fields to check at all, so an empty sweep cannot pass vacuously", () => {
    expect(enumFields.map((field) => field.localName).sort()).toEqual([
      "conflict",
      "state",
      "upstreamDrift",
    ]);
  });

  it.each(enumFields.map((field) => [field.localName] as const))(
    "leaves no enum field at its UNSPECIFIED zero: %s",
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
