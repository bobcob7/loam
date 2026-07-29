import type { ReactElement } from "react";
import styles from "./Placeholder.module.css";

export interface PlaceholderProps {
  readonly message: string;
}

/**
 * Placeholder is the scaffold's only screen: it renders a single heading so
 * the app has something to mount before the real shell and routes land
 * (loam-nvb.3 onwards). It doubles as the fixture the scaffold's own test
 * renders, which is how the Vitest setup file gets exercised.
 */
export function Placeholder({ message }: PlaceholderProps): ReactElement {
  return (
    <main className={styles.root}>
      <h1 className={styles.title}>{message}</h1>
    </main>
  );
}
