import type { ReactElement } from "react";

export interface ProposalDetailProps {
  /** The enrolled repo identifier in its wire form, `<group>/<repo_name>`. */
  readonly repo: string;
  /** The work branch name, `wb-<hex>` — unique within the repo. */
  readonly workBranch: string;
}

/**
 * Proposal detail (`/proposals/:group/:name/:workBranch`) — the review, its
 * diff, threads and verdicts, plus the admin decision. Filled in by
 * loam-nvb.13. This is the screen that mixes `loam.admin.v1` with `loam.v1`
 * (`WorkBranchService`, reached as a superuser), which is why the shell
 * provides one transport for both packages rather than one per package.
 */
export function ProposalDetail({ repo, workBranch }: ProposalDetailProps): ReactElement {
  return (
    <>
      <h1>{workBranch}</h1>
      <p>{repo}</p>
    </>
  );
}
