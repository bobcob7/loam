import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  UpstreamDrift,
  WorkBranchConflict,
  WorkBranchSchema,
  WorkBranchState,
  type WorkBranch,
} from "../gen/loam/v1/common_pb";

/**
 * A `loam.v1.WorkBranch` shaped like one a real server sends: reviewed,
 * merging cleanly, upstream where Loam left it -- the branch the proposal
 * queue exists to offer.
 *
 * EVERY enum field is spelled out, and that is the whole point of this
 * builder existing rather than a `create(WorkBranchSchema, {…})` literal per
 * test file (loam-mvso). `conflict` and `upstream_drift` are proto3 enums
 * whose zero value is `UNSPECIFIED` and whose healthy value is `NONE = 1`, so
 * a fixture that OMITS them decodes as `UNSPECIFIED` -- indistinguishable
 * from a deliberate unset, and a value the server cannot produce: the columns
 * are NOT NULL under a CHECK constraint and `workBranchToProto` maps every
 * stored value, so protojson emits `NONE` on every healthy branch.
 *
 * That omission is not cosmetic. `docs/web-spec.md` is explicit that
 * `UNSPECIFIED` never means "fine" -- `NONE` is a positive claim -- so a
 * fixture producing `UNSPECIFIED` describes a branch the console must treat
 * as blocked, and any assertion written against it is an assertion about a
 * world that does not exist. Paired with a component that reads `UNSPECIFIED`
 * as healthy, the two errors cancel and the suite goes green over a real
 * regression. `fixtures.test.ts` fails if a future field re-opens the gap.
 *
 * Overrides are applied last, so a test that wants a conflicted or drifted
 * branch names exactly the field it is varying and inherits a faithful rest.
 */
export const workBranchFixture = (
  overrides: MessageInitShape<typeof WorkBranchSchema> = {},
): WorkBranch =>
  create(WorkBranchSchema, {
    repo: "acme/widgets",
    name: "wb-9c2f1a",
    target: "main",
    title: "Add retry to the sync loop",
    description: "",
    state: WorkBranchState.REVIEWED,
    author: "agent-3-implementer",
    conflict: WorkBranchConflict.NONE,
    upstreamDrift: UpstreamDrift.NONE,
    ...overrides,
  });
