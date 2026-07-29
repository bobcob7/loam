import type { ReactElement, ReactNode } from "react";
import styles from "./ErrorBanner.module.css";

export interface ErrorBannerProps {
  /** Optional heading, e.g. the action that failed ("Could not enrol repo"). */
  readonly title?: string;
  /** The message itself — a mapped Connect error, never a raw stringified one. */
  readonly message: string;
  /** Optional trailing controls, e.g. a Retry Button. See the note below. */
  readonly children?: ReactNode;
}

/**
 * ErrorBanner is the app's non-field error surface: the thing that renders a
 * Connect failure that belongs to no single input (docs/web-frontend-spec.md →
 * Data Layer). `RemoveRepo`'s `RemovalBlocked` detail is explicitly not this —
 * it gets its own structured panel.
 *
 * It is the one live region among these primitives. `role="alert"` is an
 * assertive region, so a banner appearing after a failed mutation is announced
 * without the user having to go looking for it, and without stealing focus —
 * which matters because the user's focus is usually still on the button they
 * just pressed. Field errors deliberately do not do this (see Field); one
 * assertive announcement per failed action is the point.
 *
 * A control passed as `children` should be a secondary Button:
 * --color-border-strong measures 3.25:1 against --color-danger-bg, so an
 * outlined control keeps its boundary on the tint, whereas --color-accent
 * measures 2.88:1 there and a primary Button would lose its edge — the one
 * place in the app where the filled variant is the wrong choice.
 */
export function ErrorBanner({ title, message, children }: ErrorBannerProps): ReactElement {
  return (
    <div className={styles.root} role="alert">
      {title === undefined ? null : <p className={styles.title}>{title}</p>}
      <p className={styles.message}>{message}</p>
      {children === undefined ? null : <div className={styles.actions}>{children}</div>}
    </div>
  );
}
