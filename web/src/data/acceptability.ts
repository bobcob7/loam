import { UpstreamDrift, WorkBranchConflict, WorkBranchState, type WorkBranch } from "../gen/loam/v1/common_pb";

/**
 * Why `AcceptProposal` would refuse this work branch right now, or
 * `undefined` when nothing about the branch itself blocks it.
 *
 * This mirrors the server's `acceptableNow`
 * (`internal/handler/proposal/proposal.go`) clause for clause and in the same
 * order, so the sentence shown here is the same objection the RPC would raise.
 * Duplicating a server predicate in the client is normally the wrong move, and
 * it is done deliberately here for one reason: the proposal DETAIL screen
 * reads `loam.v1.WorkBranchService.GetWorkBranch`, whose `WorkBranch` message
 * carries no `acceptable` field -- only `ListProposals` computes one. The three
 * inputs are all on `WorkBranch` itself, so the client can answer the same
 * question; what it must not do is answer a DIFFERENT one. `acceptability.test.ts`
 * pins every case, and the server is still the authority: this only decides
 * whether to offer the button, never whether the accept succeeds.
 *
 * The approve-verdict precondition is deliberately NOT here, matching
 * `acceptableNow`, which also omits it. It is a fact about the review round
 * rather than about the branch, `AcceptProposal` checks it separately, and
 * folding it in would make this disagree with the server for a branch whose
 * verdicts have not loaded yet.
 */
export function acceptBlocker(wb: WorkBranch): string | undefined {
  if (wb.state !== WorkBranchState.REVIEWED) {
    return `This work branch is ${workBranchStateWord(wb.state)}, not reviewed. Only a reviewed branch can be accepted.`;
  }
  if (wb.conflict !== WorkBranchConflict.NONE) {
    return "This work branch no longer merges cleanly into its target. It must be caught up and re-reviewed before it can be accepted.";
  }
  if (wb.upstreamDrift !== UpstreamDrift.NONE) {
    return "This work branch's loam/ branch on the forge was moved somewhere Loam cannot fast-forward into. Reconcile the upstream branch by hand first; a catch-up push will not clear this.";
  }
  return undefined;
}

/**
 * The lower-case state word for {@link acceptBlocker}'s sentence.
 *
 * `UNSPECIFIED` and any value this client does not recognise both read as
 * "not reviewed" rather than being named, because naming a state the client
 * cannot identify would be a worse sentence than the true and sufficient one:
 * whatever it is, it is not `REVIEWED`, which is the only state that accepts.
 */
function workBranchStateWord(state: WorkBranchState): string {
  switch (state) {
    case WorkBranchState.DRAFT:
      return "a draft";
    case WorkBranchState.REVIEWABLE:
      return "awaiting review";
    case WorkBranchState.COMPLETE:
      return "complete";
    case WorkBranchState.CLOSED:
      return "closed";
    default:
      return "in a state this console does not recognise";
  }
}
