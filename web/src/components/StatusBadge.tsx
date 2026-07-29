import type { ReactElement, ReactNode } from "react";
import styles from "./StatusBadge.module.css";

/**
 * One of the five status intents src/styles/tokens.css provisions, each a
 * `-fg`/`-bg` token pair tuned so a pill is a single interpolation. See
 * ./statusIntent for the helpers that map the schema's four status enums
 * (SyncState, IngestStatus, WorkBranchState, VerdictOutcome) onto these five.
 */
export type StatusIntent = "neutral" | "info" | "success" | "warning" | "danger";

export interface StatusBadgeProps {
  readonly intent: StatusIntent;
  /**
   * The status word, rendered as ordinary text. Required rather than
   * optional: colour is never the sole carrier of meaning
   * (docs/web-frontend-spec.md -> Conventions), so this is what a screen
   * reader gets in place of the hue a sighted user reads off the pill, and
   * an intent with nothing to say alongside it fails SC 1.4.1.
   */
  readonly children: ReactNode;
}

/**
 * StatusBadge is the app's one status pill: a small rounded tag tinted by
 * one of the five intents in src/styles/tokens.css, with its label carried
 * as plain text rather than through colour alone.
 *
 * It takes an intent, not a schema enum value, deliberately. Four different
 * generated enums (SyncState, IngestStatus, WorkBranchState, VerdictOutcome)
 * all need one of these five looks, and every one of those enums is an
 * open `as const` object carrying a trailing UnknownEnum member -- no
 * `switch` over any of them can be exhaustive, so each needs its own
 * explicit fallback for a value the frontend has never heard of. See
 * ./statusIntent, which centralises that mapping (and its default) once per
 * enum rather than leaving each of the seven screens that need a pill to
 * hand-roll (and inevitably drift on) its own `default: return "unknown"`.
 * A screen calls e.g. `syncStateIntent(status.state)` and spreads the
 * `{ intent, label }` result straight into this component.
 */
export function StatusBadge({ intent, children }: StatusBadgeProps): ReactElement {
  const className = [styles.root, styles[intent]]
    .filter((name): name is string => typeof name === "string")
    .join(" ");
  return <span className={className}>{children}</span>;
}
