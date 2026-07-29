import type { ReactElement } from "react";
import { useEffect, useId, useRef, useState } from "react";
import styles from "./CopyField.module.css";

export interface CopyFieldProps {
  /** Names the value. Also names the copy button ("Copy pull request URL"). */
  readonly label: string;
  readonly value: string;
}

type CopyState = "idle" | "copied" | "failed";

/** How long the confirmation stays up before the status region goes quiet. */
const CONFIRMATION_MS = 2000;

/**
 * CopyField shows a read-only value with a copy-to-clipboard button.
 *
 * Generic by design, not SSH-specific: the consumers are Proposal detail
 * (loam-nvb.13) copying `AcceptProposal`'s `pr_url` and `upstream_branch`,
 * and Repo detail (loam-nvb.9) copying a repo identifier or upstream URL.
 *
 * ACCESSIBILITY DECISIONS:
 *
 *   - The value lives in a read-only `<input>` with a real `<label>`, not in
 *     a `<code>` block. That keeps it in the tab order and announced as a
 *     labelled value, and -- the point -- it stays selectable, so the
 *     keyboard route (focus, Ctrl+A, Ctrl+C) works when the Clipboard API
 *     does not. `readOnly`, not `disabled`, because a disabled input is
 *     neither focusable nor selectable.
 *   - The button's accessible name includes the label, so a screen with two
 *     CopyFields does not offer two buttons both called "Copy".
 *   - The outcome is announced, not just coloured: a `role="status"` live
 *     region carries "Copied <label>" / "Could not copy <label>". A colour
 *     change alone reaches nobody using a screen reader, and a tick alone
 *     reaches nobody who cannot distinguish it.
 *   - The mono face is functional here (tokens.css -> Typography): these
 *     values are identifiers where 0/O and 1/l have to be told apart.
 */
export function CopyField({ label, value }: CopyFieldProps): ReactElement {
  const inputId = useId();
  const [state, setState] = useState<CopyState>("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(
    () => () => {
      if (timer.current !== null) clearTimeout(timer.current);
    },
    [],
  );

  const settle = (next: CopyState): void => {
    setState(next);
    if (timer.current !== null) clearTimeout(timer.current);
    timer.current = setTimeout(() => setState("idle"), CONFIRMATION_MS);
  };

  const handleCopy = async (): Promise<void> => {
    // navigator.clipboard is absent outside a secure context (and in jsdom),
    // and writeText rejects when the permission is denied. Both land the
    // user on the manual select-and-copy route, so both say so.
    const clipboard: Clipboard | undefined = navigator.clipboard;
    if (clipboard === undefined) {
      settle("failed");
      return;
    }
    try {
      await clipboard.writeText(value);
      settle("copied");
    } catch {
      settle("failed");
    }
  };

  return (
    <div className={styles.root}>
      <label className={styles.label} htmlFor={inputId}>
        {label}
      </label>
      <div className={styles.row}>
        <input
          id={inputId}
          className={styles.value}
          type="text"
          readOnly
          value={value}
          spellCheck={false}
        />
        <button
          type="button"
          className={styles.button}
          aria-label={`Copy ${label}`}
          onClick={() => {
            void handleCopy();
          }}
        >
          Copy
        </button>
      </div>
      <p
        className={state === "failed" ? styles.statusFailed : styles.status}
        role="status"
      >
        {state === "copied" && `Copied ${label}`}
        {state === "failed" && `Could not copy ${label}. Select the value and copy it manually.`}
      </p>
    </div>
  );
}
