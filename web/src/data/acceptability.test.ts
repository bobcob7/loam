import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import {
  UpstreamDrift,
  WorkBranchConflict,
  WorkBranchSchema,
  WorkBranchState,
  type WorkBranch,
} from "../gen/loam/v1/common_pb";
import { acceptBlocker } from "./acceptability";

/** A branch nothing blocks: reviewed, merges cleanly, upstream untouched. */
const acceptable = (overrides: MessageInitShape<typeof WorkBranchSchema> = {}): WorkBranch =>
  create(WorkBranchSchema, {
    repo: "acme/widgets",
    name: "wb-9c2f1a",
    target: "main",
    state: WorkBranchState.REVIEWED,
    conflict: WorkBranchConflict.NONE,
    upstreamDrift: UpstreamDrift.NONE,
    ...overrides,
  });

describe("acceptBlocker", () => {
  it("returns undefined for a reviewed, unconflicted, undrifted branch", () => {
    expect(acceptBlocker(acceptable())).toBeUndefined();
  });

  // loam-u84g's own case: demoted out of REVIEWED by a conflicting target
  // advance. Both the state and the conflict are set, and the STATE is what the
  // server reports first (acceptableNow checks it first), so the sentence must
  // name the state -- telling the operator to "catch it up" when the server
  // will say "not reviewed" sends them to the wrong remedy.
  it("names the state, not the conflict, for a branch a conflicting advance demoted", () => {
    const blocker = acceptBlocker(
      acceptable({ state: WorkBranchState.DRAFT, conflict: WorkBranchConflict.RESET }),
    );
    expect(blocker).toContain("draft");
    expect(blocker).toContain("not reviewed");
  });

  it("names the conflict for a reviewed branch flagged against its target", () => {
    const blocker = acceptBlocker(acceptable({ conflict: WorkBranchConflict.FLAGGED }));
    expect(blocker).toContain("merge");
    expect(blocker).toContain("re-reviewed");
  });

  // Drift and conflict must never collapse into one message: a conflict is
  // fixed by a catch-up push, drift only by reconciling the forge
  // (statusIntent.ts, docs/web-spec.md). Sending the admin to the wrong one
  // costs a wasted push against a branch that cannot take it.
  it("names drift distinctly from a conflict, and does not mention catching up", () => {
    const blocker = acceptBlocker(acceptable({ upstreamDrift: UpstreamDrift.DIVERGED }));
    expect(blocker).toContain("forge");
    expect(blocker).toContain("catch-up push will not clear this");
  });

  it("reports a reviewable branch as not reviewed rather than as acceptable", () => {
    expect(acceptBlocker(acceptable({ state: WorkBranchState.REVIEWABLE }))).toContain("not reviewed");
  });

  // UNSPECIFIED must block, never pass. NONE is a positive claim that the
  // branch merges cleanly; a value that never arrived is not evidence of it,
  // and the safe default here is the one that withholds the button.
  it("blocks on an unrecognised conflict or drift value rather than treating it as clean", () => {
    expect(acceptBlocker(acceptable({ conflict: WorkBranchConflict.UNSPECIFIED }))).toBeDefined();
    expect(acceptBlocker(acceptable({ upstreamDrift: UpstreamDrift.UNSPECIFIED }))).toBeDefined();
    expect(acceptBlocker(acceptable({ conflict: 99 as WorkBranchConflict }))).toBeDefined();
    expect(acceptBlocker(acceptable({ upstreamDrift: 99 as UpstreamDrift }))).toBeDefined();
  });

  it("blocks on a state outside the generated union", () => {
    expect(acceptBlocker(acceptable({ state: 99 as WorkBranchState }))).toContain("does not recognise");
  });
});
