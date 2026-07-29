import type { ReactElement, ReactNode } from "react";
import { Link, matchRoute, useLocation, useRouter } from "wouter";
import styles from "./AppLayout.module.css";
import { routePatterns } from "./paths";

interface NavItem {
  readonly href: string;
  readonly label: string;
  /**
   * The route patterns this nav entry stands for. A detail screen has no nav
   * entry of its own, so it marks its section active: viewing
   * `/repos/acme/widgets` keeps "Repos" current.
   */
  readonly owns: readonly string[];
}

/** The five top-level screens (docs/web-spec.md -> Screens). */
const navItems: readonly NavItem[] = [
  {
    href: routePatterns.repos,
    label: "Repos",
    owns: [routePatterns.repos, routePatterns.repoDetail],
  },
  { href: routePatterns.credentials, label: "Credentials", owns: [routePatterns.credentials] },
  { href: routePatterns.roles, label: "Roles", owns: [routePatterns.roles] },
  {
    href: routePatterns.proposals,
    label: "Proposals",
    owns: [routePatterns.proposals, routePatterns.proposalDetail],
  },
  { href: routePatterns.jobs, label: "Jobs", owns: [routePatterns.jobs] },
];

export interface AppLayoutProps {
  readonly children: ReactNode;
}

/**
 * The chrome every screen renders inside: the brand, the top-level nav, and
 * the `<main>` the routed screen occupies.
 *
 * The active entry is decided by matching the current location against the
 * *route patterns* the entry owns, using the router's own parser — not by a
 * string prefix test. A prefix test gets two things wrong that this does not:
 * `"/"` is a prefix of every path, and `/repos` (no repo) would light up the
 * Repos tab while the not-found screen renders beneath it.
 *
 * Active is published as `aria-current="page"`, so it is announced rather
 * than only coloured — colour is never the sole signal (src/styles/tokens.css).
 */
export function AppLayout({ children }: AppLayoutProps): ReactElement {
  const router = useRouter();
  const [location] = useLocation();
  const isActive = (owns: readonly string[]): boolean =>
    owns.some((pattern) => matchRoute(router.parser, pattern, location)[0]);
  return (
    <div className={styles.shell}>
      <header className={styles.header}>
        <Link href={routePatterns.repos} className={styles.brand}>
          Loam
        </Link>
        <nav aria-label="Main">
          <ul className={styles.navList}>
            {navItems.map((item) => (
              <li key={item.href}>
                <Link
                  href={item.href}
                  className={styles.navLink}
                  aria-current={isActive(item.owns) ? "page" : undefined}
                >
                  {item.label}
                </Link>
              </li>
            ))}
          </ul>
        </nav>
      </header>
      <main className={styles.main}>{children}</main>
    </div>
  );
}
