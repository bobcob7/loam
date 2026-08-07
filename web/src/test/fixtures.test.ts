import { describe, expect, it } from "vitest";
import {
  UpstreamDrift,
  VerdictOutcome,
  WorkBranchConflict,
  WorkBranchSchema,
  WorkBranchState,
} from "../gen/loam/v1/common_pb";
import { IngestKind, IngestStatus, SyncState } from "../gen/loam/admin/v1/repo_admin_pb";
import { expectNoUnspecifiedEnums, generatedFiles, messagesDeclaringEnumFields } from "./enumGuard";
import * as fixtureModule from "./fixtures";
import {
  blockedWorkBranchFixture,
  enrolledRepoFixture,
  fixtureBuilders,
  ingestJobFixture,
  proposalFixture,
  syncStatusFixture,
  verdictSummaryFixture,
  workBranchFixture,
} from "./fixtures";

/**
 * The guard that stops loam-mvso's defect from being reintroduced by the next
 * field or the next message, rather than only fixing the fields that had it.
 *
 * loam-mvso pinned `["conflict", "state", "upstreamDrift"]` by hand, and that
 * hard-coded list -- not the schema sweep standing next to it -- was what
 * actually closed the holes: the sweep's `not.toBe(0)` PASSES on an `optional`
 * enum (unset is `undefined`, not 0) and its `fieldKind === "enum"` filter
 * drops a `repeated` enum entirely. Both were verified on a scratch branch by
 * adding one of each to `WorkBranch`: the old assertions caught 1 of 3.
 *
 * So nothing here is enumerated. Two derived checks between them cover the
 * whole space:
 *
 *  - THE SWEEP walks each builder's descriptor recursively, so a new enum
 *    field on any covered message -- at any depth, any cardinality -- fails.
 *  - THE COVERAGE CHECK discovers every message in `src/gen` that declares an
 *    enum field and demands a builder or a written reason, so a new MESSAGE
 *    carrying an enum fails too.
 *
 * The only hand-written list left is {@link notFixtured}, and it enumerates
 * DECISIONS ALREADY TAKEN rather than fields. It fails closed on a new entry
 * and fails on a stale one.
 */

/**
 * Messages that declare an enum field and deliberately have no fixture
 * builder. Each needs a reason; `accounts for every message declaring an enum
 * field` below is the equality that fails if an entry stops applying.
 */
const notFixtured: Readonly<Record<string, string>> = {
  "loam.v1.ListWorkBranchesRequest":
    "A request, not a payload the console renders, and the SPA never builds one -- " +
    "ListWorkBranches is a CLI/agent RPC with no call site under web/src outside " +
    "gen/. Its `optional state` is NOT an unfiltered default either: " +
    "workbranch.proto documents 'Defaults to REVIEWABLE when unset', so unset is a " +
    "server-side choice this console never makes.",
  "loam.admin.v1.ListIngestJobsRequest":
    "Built by the Jobs screen itself (src/routes/Jobs.tsx), not by a fixture. Its " +
    "`status` is an `optional` filter and unset is the intended value for 'all " +
    "statuses' -- the one enum in this schema where absence is a real answer.",
  "loam.v1.SubmitVerdictRequest":
    "Reviewer-agent RPC; the admin console never submits a verdict, so no web test " +
    "builds this. Its `outcome` is the subject of any test that would.",
  "loam.v1.SubmitVerdictResponse":
    "The echo of the above, and equally unbuilt by the console. Verdicts reach the " +
    "SPA through ListVerdicts as VerdictSummary, which IS fixtured.",
};

describe("fixture builders", () => {
  it.each(fixtureBuilders.map((entry) => [entry.schema.typeName, entry] as const))(
    "leaves no enum field at its UNSPECIFIED zero, at any depth: %s",
    (_typeName, entry) => {
      expectNoUnspecifiedEnums(entry.schema, entry.build());
    },
  );
});

/**
 * `fixtureBuilders` is the sweep's only entry point, so an entry going missing
 * silently un-sweeps a message. That is the enumerated-guard defect this bead
 * exists to kill, one layer up from where the guard applies it -- and it bit:
 * deleting the `Proposal` or `EnrolledRepo` entry SURVIVED the whole suite,
 * because neither message declares an enum field of its own and so neither
 * appears on either side of the coverage equality. With the Proposal entry
 * gone, a `proposalFixture` embedding a bare `create(WorkBranchSchema, {…})`
 * stopped failing the sweep entirely.
 *
 * So the array is derived against, rather than trusted. Builders are
 * discovered by CALLING every exported function and keeping the ones that
 * return a protobuf message -- no naming convention, so a builder named
 * anything at all is still found.
 */
describe("fixture registry", () => {
  /** Exported functions that, called with no arguments, produce a message. */
  const exportedBuilders = Object.entries(fixtureModule).filter(([, value]) => {
    if (typeof value !== "function") return false;
    try {
      const built: unknown = (value as () => unknown)();
      return typeof built === "object" && built !== null && "$typeName" in built;
    } catch {
      return false;
    }
  });

  it("finds the exported builders by calling them, not by trusting their names", () => {
    // Without this the two assertions below are vacuous: a probe that matched
    // nothing would make "every export is registered" trivially true.
    expect(exportedBuilders.length).toBeGreaterThan(0);
  });

  it("sweeps every builder this module exports, so forgetting to register one is impossible", () => {
    const registered = new Set<unknown>(fixtureBuilders.map((entry) => entry.build));
    const unregistered = exportedBuilders.filter(([, fn]) => !registered.has(fn)).map(([name]) => name);
    expect(unregistered, "an exported fixture builder that fixtureBuilders does not sweep").toEqual([]);
  });

  it("registers nothing this module does not export, so an entry cannot outlive its builder", () => {
    // The other half of the pincer. Deleting an entry AND its exported builder
    // passes here -- and breaks compilation in every route test that imports
    // it, which is why these builders are the ones the route tests use.
    const exported = new Set<unknown>(exportedBuilders.map(([, fn]) => fn));
    const orphaned = fixtureBuilders.filter((entry) => !exported.has(entry.build));
    expect(orphaned.map((entry) => entry.schema.typeName)).toEqual([]);
  });
});

describe("fixture coverage", () => {
  const declaring = messagesDeclaringEnumFields(generatedFiles()).map((message) => message.typeName);
  const built = new Set(fixtureBuilders.map((entry) => entry.schema.typeName));

  it("names each message that can carry the defect exactly once", () => {
    // Deliberately pins NO count and NO names. A count literal would be the
    // same self-staling defect the guard exists to remove, and it would add
    // nothing: the equality below already fails on any change to `declaring`
    // that is not covered, which is the only change worth stopping.
    //
    // What this does check is that `declaring` is a set: a duplicate typeName
    // would make the equality below compare the wrong shape and could mask a
    // genuinely uncovered message.
    expect(new Set(declaring).size).toBe(declaring.length);
  });

  it("accounts for every message declaring an enum field, by builder or by written reason", () => {
    // Stated as an EQUALITY, not as "nothing is uncovered". The subset form
    // (`uncovered).toEqual([])`) is vacuous while everything happens to be
    // covered -- deleting the check entirely would pass -- so it would report
    // coverage without testing for it, which is the exact failure mode this
    // bead exists to avoid one layer up. Equality fails in both directions: a
    // new enum-carrying message with no builder, AND a notFixtured entry that
    // has quietly stopped applying.
    //
    // It doubles as the glob's own sanity check, because these names span
    // both proto packages: a `generatedFiles()` that matched nothing yields an
    // empty `declaring` and fails here rather than passing everything.
    const withoutBuilder = declaring.filter((name) => !built.has(name)).sort();
    expect(
      withoutBuilder,
      "add a fixture builder, or a notFixtured reason, for each new enum-carrying message",
    ).toEqual(Object.keys(notFixtured).sort());
  });
});

describe("fixture faithfulness", () => {
  it("workBranchFixture describes the branch the proposal queue exists to offer", () => {
    const wb = workBranchFixture();
    expect(wb.state).toBe(WorkBranchState.REVIEWED);
    expect(wb.conflict).toBe(WorkBranchConflict.NONE);
    expect(wb.upstreamDrift).toBe(UpstreamDrift.NONE);
  });

  it("verdictSummaryFixture is a live approve, the verdict that counts toward the bar", () => {
    const verdict = verdictSummaryFixture();
    expect(verdict.outcome).toBe(VerdictOutcome.APPROVE);
    expect(verdict.stale).toBe(false);
  });

  it("proposalFixture is acceptable, and carries a branch and a verdict that agree", () => {
    const proposal = proposalFixture();
    expect(proposal.acceptable).toBe(true);
    expect(proposal.workBranch?.state).toBe(WorkBranchState.REVIEWED);
    expect(proposal.verdicts.map((verdict) => verdict.outcome)).toEqual([VerdictOutcome.APPROVE]);
  });

  it("syncStatusFixture is a repo that last synced cleanly", () => {
    expect(syncStatusFixture().state).toBe(SyncState.IDLE);
    expect(syncStatusFixture().error).toBe("");
  });

  it("enrolledRepoFixture carries a COMPLETE SyncStatus, not a partial one", () => {
    // Compared against the nested builder whole, not spot-checked on `state`.
    // `state` is the one field a bare `{ state }` partial still sets, so the
    // spot-check PASSED under exactly the mutation this test is named for --
    // it was killed incidentally, by a Repos.test.tsx assertion that happens
    // to read `lastSyncedAt`. Equality cannot drift as SyncStatus grows.
    expect(enrolledRepoFixture().sync).toEqual(syncStatusFixture());
  });

  it("ingestJobFixture is a completed full ingest", () => {
    expect(ingestJobFixture().kind).toBe(IngestKind.FULL);
    expect(ingestJobFixture().status).toBe(IngestStatus.SUCCEEDED);
  });

  it("blockedWorkBranchFixture is non-terminal, which is what makes it a blocker", () => {
    expect(blockedWorkBranchFixture().state).toBe(WorkBranchState.REVIEWABLE);
  });
});

describe("fixture overrides", () => {
  it("apply last, so a test can vary one field and inherit a faithful rest", () => {
    const wb = workBranchFixture({ conflict: WorkBranchConflict.FLAGGED });
    expect(wb.conflict).toBe(WorkBranchConflict.FLAGGED);
    expect(wb.upstreamDrift).toBe(UpstreamDrift.NONE);
    expect(wb.state).toBe(WorkBranchState.REVIEWED);
  });

  it("let a test ask for UNSPECIFIED deliberately, which is the loud opt-out", () => {
    // The guard's rule is "no enum may be unset", not "no enum may be zero".
    // A test for how the console renders a field an OLDER server did not send
    // needs the zero, and gets it by typing the word at the call site -- there
    // is nothing to configure and nothing to silence.
    const wb = workBranchFixture({ conflict: WorkBranchConflict.UNSPECIFIED });
    expect(wb.conflict).toBe(WorkBranchConflict.UNSPECIFIED);
    expect(wb.upstreamDrift).toBe(UpstreamDrift.NONE);
  });

  it("replace a nested message wholesale, which is why nested builders exist", () => {
    // `create()` does not deep-merge. Overriding `sync` with a bare partial
    // builds a FRESH SyncStatus from it, dropping lastSyncedAt -- so call
    // sites go through syncStatusFixture instead.
    const bare = enrolledRepoFixture({ sync: { state: SyncState.SYNCING } });
    expect(bare.sync?.lastSyncedAt).toBe("");
    const built = enrolledRepoFixture({ sync: syncStatusFixture({ state: SyncState.SYNCING }) });
    expect(built.sync?.lastSyncedAt).toBe("2026-07-20T10:00:00Z");
    expect(built.sync?.state).toBe(SyncState.SYNCING);
  });
});

describe("WorkBranchSchema", () => {
  it("still has upstreamPrUrl as the one optional field, so the guard's presence rule is exercised", () => {
    // Not an enum, but it is the field that proves `isSet` and `!== 0` are
    // different questions: an unset explicit-presence field reads `undefined`.
    expect(workBranchFixture().upstreamPrUrl).toBeUndefined();
    expect(WorkBranchSchema.field["upstreamPrUrl"]?.presence).not.toBe(
      WorkBranchSchema.field["state"]?.presence,
    );
  });
});
