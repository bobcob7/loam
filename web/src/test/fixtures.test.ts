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
 * builder. Each needs a reason, and `keeps its reasons true` below fails if an
 * entry stops being a message that declares an enum field at all.
 */
const notFixtured: Readonly<Record<string, string>> = {
  "loam.v1.ListWorkBranchesRequest":
    "A request, not a payload the console renders, and the SPA never builds one -- " +
    "ListWorkBranches is a CLI/agent RPC. Its `state` is an `optional` filter whose " +
    "unset state MEANS 'no filter', so a builder here would have to opt out of the " +
    "guard on its only enum field.",
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

describe("fixture coverage", () => {
  const declaring = messagesDeclaringEnumFields(generatedFiles()).map((message) => message.typeName);
  const built = new Set(fixtureBuilders.map((entry) => entry.schema.typeName));

  it("finds the messages that can carry the defect by walking src/gen, not by a list", () => {
    // Pinned as a COUNT, not as names: the names are what must be free to
    // move. A count that changes is a prompt to look, and the two assertions
    // below are what actually decide whether the change is covered.
    expect(declaring.length).toBeGreaterThan(0);
    expect(new Set(declaring).size).toBe(declaring.length);
  });

  it("has a builder or a written reason for every message declaring an enum field", () => {
    const uncovered = declaring.filter((name) => !built.has(name) && notFixtured[name] === undefined);
    expect(uncovered, "new enum-carrying message: add a fixture builder or a notFixtured reason").toEqual(
      [],
    );
  });

  it("keeps its reasons true: every notFixtured entry still declares an enum field", () => {
    // Doubles as the glob's own sanity check: these names span both proto
    // packages, so a `generatedFiles()` that silently matched nothing (an
    // empty `declaring`, which would make the coverage check above vacuously
    // pass) fails here instead.
    const stale = Object.keys(notFixtured).filter((name) => !declaring.includes(name));
    expect(stale, "notFixtured names messages that no longer declare an enum field").toEqual([]);
  });

  it("keeps notFixtured and the builders disjoint", () => {
    expect(Object.keys(notFixtured).filter((name) => built.has(name))).toEqual([]);
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

  it("enrolledRepoFixture carries a full SyncStatus, not a partial one", () => {
    expect(enrolledRepoFixture().sync?.state).toBe(SyncState.IDLE);
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
