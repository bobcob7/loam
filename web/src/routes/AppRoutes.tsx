import type { ReactElement } from "react";
import { Route, Switch } from "wouter";
import { Credentials } from "./Credentials";
import { Jobs } from "./Jobs";
import { NotFound } from "./NotFound";
import { ProposalDetail } from "./ProposalDetail";
import { Proposals } from "./Proposals";
import { RepoDetail } from "./RepoDetail";
import { Repos } from "./Repos";
import { Roles } from "./Roles";
import { repoFromSegments, routePatterns } from "./paths";

/**
 * The route table: every screen in docs/web-frontend-spec.md -> Routing &
 * Screens, plus the fallback.
 *
 * `<Switch>` rather than bare `<Route>`s so exactly one screen renders —
 * without it the trailing pathless `<Route>` (the fallback) would match every
 * location and render *alongside* the real screen.
 *
 * The two parameterised routes use the render-prop form so the URL segments
 * are turned into a domain value here, once, instead of each screen calling
 * `useParams` and re-deriving it. `params.group` and `params.name` are typed
 * `string` (not `string | undefined`) because wouter infers parameter names
 * from the literal pattern type — which is why routePatterns is `as const`.
 */
export function AppRoutes(): ReactElement {
  return (
    <Switch>
      <Route path={routePatterns.repos} component={Repos} />
      <Route path={routePatterns.repoDetail}>
        {(params) => <RepoDetail repo={repoFromSegments(params.group, params.name)} />}
      </Route>
      <Route path={routePatterns.credentials} component={Credentials} />
      <Route path={routePatterns.roles} component={Roles} />
      <Route path={routePatterns.proposals} component={Proposals} />
      <Route path={routePatterns.proposalDetail}>
        {(params) => (
          <ProposalDetail
            repo={repoFromSegments(params.group, params.name)}
            workBranch={params.workBranch}
          />
        )}
      </Route>
      <Route path={routePatterns.jobs} component={Jobs} />
      <Route component={NotFound} />
    </Switch>
  );
}
