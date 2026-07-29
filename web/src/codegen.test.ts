import { create, toJson } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { acceptProposal, listProposals } from "./gen/loam/admin/v1/proposal-ProposalService_connectquery";
import { ListProposalsRequestSchema } from "./gen/loam/admin/v1/proposal_pb";
import { PageSchema, WorkBranchState } from "./gen/loam/v1/common_pb";
import { getWorkBranch } from "./gen/loam/v1/workbranch-WorkBranchService_connectquery";

// `npm run typecheck` already type-checks src/gen (tsconfig's include covers
// src/), so these tests deliberately do NOT re-assert types. What they cover
// is the part a type check cannot see: that the generated descriptors are
// usable at RUNTIME against the pinned @bufbuild/protobuf and
// @connectrpc/connect-query versions.
//
// That is the specific failure this guards. protoc-gen-es 2.13.0 emits
// imports from "@bufbuild/protobuf/codegenv2", a versioned entry point;
// protoc-gen-connect-query 2.2.0 likewise pairs with connect-query 2.2.0.
// Bump a plugin without bumping its runtime (or the reverse) and the
// generated code can still type-check while `create()` or a query hook
// blows up on a descriptor shape the runtime does not understand. Codegen
// that compiles but cannot run is exactly the drift committing src/gen is
// supposed to make visible.
describe("generated protobuf messages", () => {
  it("constructs and serialises a loam.admin.v1 message against the pinned runtime", () => {
    const request = create(ListProposalsRequestSchema, {
      page: create(PageSchema, { limit: 25, offset: 50 }),
    });
    expect(toJson(ListProposalsRequestSchema, request)).toEqual({
      page: { limit: 25, offset: 50 },
    });
  });

  it("generates enums as erasable `as const` objects, not TypeScript enums", () => {
    // buf.gen.yaml passes erasable_syntax=true so tsconfig's
    // erasableSyntaxOnly stays on for hand-written code. A regression to
    // plain `enum` output would still pass this value check, so assert the
    // shape: a TS enum object carries a reverse numeric->name mapping, an
    // `as const` object does not.
    expect(WorkBranchState.REVIEWED).toBe(3);
    expect(Object.keys(WorkBranchState)).not.toContain("3");
  });
});

describe("generated connect-query hooks", () => {
  it("exports method descriptors for the RPCs the SPA screens call", () => {
    // One per proto package, since the admin is a superuser and the SPA
    // mixes loam.admin.v1 with loam.v1 (docs/web-frontend-spec.md ->
    // Routing & Screens).
    expect(listProposals.parent.typeName).toBe("loam.admin.v1.ProposalService");
    expect(acceptProposal.methodKind).toBe("unary");
    expect(getWorkBranch.parent.typeName).toBe("loam.v1.WorkBranchService");
  });
});
