import {
  create,
  type DescMessage,
  type Message,
  type MessageInitShape,
  type MessageShape,
} from "@bufbuild/protobuf";
import {
  BlockedWorkBranchSchema,
  EnrolledRepoSchema,
  IngestJobSchema,
  IngestKind,
  IngestStatus,
  SyncState,
  SyncStatusSchema,
  type BlockedWorkBranch,
  type EnrolledRepo,
  type IngestJob,
  type SyncStatus,
} from "../gen/loam/admin/v1/repo_admin_pb";
import { ProposalSchema, type Proposal } from "../gen/loam/admin/v1/proposal_pb";
import {
  UpstreamDrift,
  VerdictOutcome,
  VerdictSummarySchema,
  WorkBranchConflict,
  WorkBranchSchema,
  WorkBranchState,
  type VerdictSummary,
  type WorkBranch,
} from "../gen/loam/v1/common_pb";

/**
 * Shared fixture builders for the messages a web test hand-builds.
 *
 * EVERY enum field is spelled out in every builder below, and that is the
 * whole reason these exist rather than a `create(Schema, {…})` literal per
 * test file (loam-mvso, generalised by loam-yhcz). `MessageInitShape` makes
 * every field optional, so an omitted proto3 enum type-checks, decodes as
 * `UNSPECIFIED = 0`, and is indistinguishable in review from a deliberate
 * unset -- while being a value a real server never sends, because it writes a
 * positive named member on every healthy record.
 *
 * `docs/web-spec.md` is explicit that `UNSPECIFIED` never means "fine": `NONE`
 * is a positive claim. So a fixture producing `UNSPECIFIED` describes a record
 * the console must treat as blocked, and any assertion written against it is
 * an assertion about a world that does not exist. Paired with a component that
 * reads `UNSPECIFIED` as healthy, the two errors cancel and the suite goes
 * green over a real regression.
 *
 * DOES A SHARED BUILDER JUST CENTRALISE THE OMISSION? It would, if nothing
 * checked it -- one helper defaulting a field invisibly is the same defect as
 * N test files doing so, made harder to see. Two things stop that:
 *
 *  1. `fixtures.test.ts` runs `expectNoUnspecifiedEnums` over every builder's
 *     output WITH NO OVERRIDES, so the default itself is proven complete by a
 *     descriptor walk rather than by review. A new enum field added to any
 *     message below fails there without anyone editing a list.
 *  2. Nested messages get their own builder, and call sites override through
 *     it (`enrolledRepoFixture({ sync: syncStatusFixture({ state: … }) })`).
 *     This matters: `create()` does NOT deep-merge, so passing a bare
 *     `{ sync: { state: SYNCING } }` would build a FRESH SyncStatus from that
 *     partial and silently drop everything else -- reintroducing the omission
 *     at the override site, one level down, where the sweep cannot see it
 *     because the sweep only ever runs on the un-overridden default.
 *
 * Overrides are applied last, so a test that wants a conflicted, drifted or
 * deliberately-UNSPECIFIED record names exactly the field it is varying and
 * inherits a faithful rest. That is also the ordinary way to opt out of the
 * guard: `workBranchFixture({ conflict: WorkBranchConflict.UNSPECIFIED })` is
 * loud at the call site because the word UNSPECIFIED is typed there.
 */

/**
 * A `loam.v1.WorkBranch` shaped like one a real server sends: reviewed,
 * merging cleanly, upstream where Loam left it -- the branch the proposal
 * queue exists to offer.
 *
 * `conflict` and `upstream_drift` are `NONE = 1` because the columns are NOT
 * NULL under a CHECK constraint and `workBranchToProto` maps every stored
 * value, so protojson emits `NONE` on every healthy branch. `fixtures.test.ts`
 * is what pins this -- and on the proposal queue it is the ONLY thing that
 * does, because that screen's "Blocked by" cell short-circuits on
 * `Proposal.acceptable` and never reads these two fields on an acceptable row.
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

/**
 * A `loam.v1.VerdictSummary`: a live (non-stale) approve, the verdict that
 * actually counts toward the approval bar.
 */
export const verdictSummaryFixture = (
  overrides: MessageInitShape<typeof VerdictSummarySchema> = {},
): VerdictSummary =>
  create(VerdictSummarySchema, {
    reviewer: "agent-2-reviewer",
    outcome: VerdictOutcome.APPROVE,
    stale: false,
    round: 2,
    ...overrides,
  });

/**
 * A `loam.admin.v1.Proposal`: an acceptable branch with one live approve.
 *
 * This is the recursion case the sweep exists for -- the enum fields at risk
 * are two levels down, on the embedded `WorkBranch` and inside the `verdicts`
 * list, where a `create(ProposalSchema, { workBranch: {…} })` literal would
 * omit them without a word from `tsc`.
 */
export const proposalFixture = (overrides: MessageInitShape<typeof ProposalSchema> = {}): Proposal =>
  create(ProposalSchema, {
    acceptable: true,
    workBranch: workBranchFixture(),
    verdicts: [verdictSummaryFixture()],
    ...overrides,
  });

/** A `loam.admin.v1.SyncStatus` for a repo that last synced cleanly. */
export const syncStatusFixture = (
  overrides: MessageInitShape<typeof SyncStatusSchema> = {},
): SyncStatus =>
  create(SyncStatusSchema, {
    state: SyncState.IDLE,
    lastSyncedAt: "2026-07-20T10:00:00Z",
    error: "",
    ...overrides,
  });

/** A `loam.admin.v1.EnrolledRepo` with one target branch, indexed and synced. */
export const enrolledRepoFixture = (
  overrides: MessageInitShape<typeof EnrolledRepoSchema> = {},
): EnrolledRepo =>
  create(EnrolledRepoSchema, {
    repo: "acme/widgets",
    upstreamUrl: "https://forge.example/acme/widgets",
    targetBranches: ["main"],
    indexedBranch: "main",
    ingestedRef: "a1b2c3d",
    sync: syncStatusFixture(),
    ...overrides,
  });

/** A `loam.admin.v1.IngestJob`: a full ingest that has finished successfully. */
export const ingestJobFixture = (overrides: MessageInitShape<typeof IngestJobSchema> = {}): IngestJob =>
  create(IngestJobSchema, {
    id: "11111111-1111-1111-1111-111111111111",
    repo: "acme/widgets",
    targetBranch: "main",
    kind: IngestKind.FULL,
    status: IngestStatus.SUCCEEDED,
    attempts: 1,
    error: "",
    queuedAt: "2026-07-20T10:00:00Z",
    startedAt: "2026-07-20T10:00:02Z",
    finishedAt: "2026-07-20T10:01:00Z",
    ...overrides,
  });

/**
 * A `loam.admin.v1.BlockedWorkBranch`: one non-terminal branch standing in the
 * way of a `RemoveRepo`, as attached to the FAILED_PRECONDITION detail.
 */
export const blockedWorkBranchFixture = (
  overrides: MessageInitShape<typeof BlockedWorkBranchSchema> = {},
): BlockedWorkBranch =>
  create(BlockedWorkBranchSchema, {
    name: "wb-aaa111",
    title: "Add retry logic",
    state: WorkBranchState.REVIEWABLE,
    ...overrides,
  });

/** A builder paired with the schema it builds, so the sweep can walk it. */
export interface FixtureBuilder {
  readonly schema: DescMessage;
  readonly build: () => Message;
}

/**
 * Pairs a builder with its schema. `build` is stored BY IDENTITY -- the
 * exported function itself, never a `() => fooFixture()` wrapper -- because
 * that identity is what `fixtures.test.ts` matches against this module's
 * exports. A wrapper would be a fresh closure and would defeat the check.
 *
 * The `MessageShape<Desc>` return type also makes a mis-paired registration
 * (`builder(SyncStatusSchema, workBranchFixture)`) a compile error rather
 * than a silent sweep of the wrong descriptor.
 */
const builder = <Desc extends DescMessage>(
  schema: Desc,
  build: () => MessageShape<Desc>,
): FixtureBuilder => ({ schema, build });

/**
 * Every builder above, so `fixtures.test.ts` can sweep them all.
 *
 * THIS ARRAY IS NOT TRUSTED, and that is the point -- an unchecked registry is
 * the enumerated guard this bead exists to kill, displaced one layer up. The
 * earlier claim here ("you cannot register a builder you did not write") only
 * covered ADDING. Removal was the hole: `ProposalSchema` and
 * `EnrolledRepoSchema` declare no enum field of their own, so deleting either
 * line sat outside the coverage equality on both sides and SURVIVED the whole
 * suite -- while silently un-sweeping the container whose embedded WorkBranch
 * and SyncStatus are exactly where the omission hides.
 *
 * So the array is now checked against this module's own exports, in both
 * directions, and the two ways of breaking it are both closed:
 *
 *  - delete an entry and leave the builder exported -> the builder is
 *    unregistered, and `fixtures.test.ts` fails naming it;
 *  - delete the entry AND the exported builder -> the route tests that import
 *    it stop compiling, because these are the builders they use.
 *
 * What decides whether the SET of builders is sufficient is separately
 * derived: `fixtures.test.ts` discovers every message in the generated
 * descriptors that declares an enum field and fails if one has neither a
 * builder here nor a written-down reason. Adding an enum field to a covered
 * message fails via the sweep; adding a new enum-carrying message fails via
 * that equality. Neither needs this array edited to notice.
 */
export const fixtureBuilders: readonly FixtureBuilder[] = [
  builder(WorkBranchSchema, workBranchFixture),
  builder(VerdictSummarySchema, verdictSummaryFixture),
  builder(ProposalSchema, proposalFixture),
  builder(SyncStatusSchema, syncStatusFixture),
  builder(EnrolledRepoSchema, enrolledRepoFixture),
  builder(IngestJobSchema, ingestJobFixture),
  builder(BlockedWorkBranchSchema, blockedWorkBranchFixture),
];
