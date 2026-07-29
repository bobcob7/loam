import type { ReactElement } from "react";

export interface RepoDetailProps {
  /** The enrolled repo identifier in its wire form, `<group>/<repo_name>`. */
  readonly repo: string;
}

/**
 * Repo detail (`/repos/:group/:name`) — target branches, the indexed branch,
 * and credential status. Filled in by loam-nvb.9.
 *
 * The identifier arrives as a prop, already rejoined from its two URL
 * segments by the route table (see ./paths.ts). Screens in this directory
 * take plain props rather than calling `useParams` so a screen bead can test
 * one without standing up a router.
 */
export function RepoDetail({ repo }: RepoDetailProps): ReactElement {
  return <h1>{repo}</h1>;
}
