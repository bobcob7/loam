import type { KeyboardEvent, MouseEvent, ReactElement, ReactNode, RefObject } from "react";
import { useEffect, useId, useRef } from "react";
import { createPortal } from "react-dom";
import styles from "./Dialog.module.css";

export interface DialogProps {
  /** Closed dialogs unmount, so a form inside starts fresh on every open. */
  readonly open: boolean;
  /** The dialog's accessible name, rendered as its heading. */
  readonly title: string;
  /** Optional supporting line, wired up as `aria-describedby`. */
  readonly description?: string;
  /** Called for every dismissal route: Escape, the close button, the scrim. */
  readonly onClose: () => void;
  readonly children: ReactNode;
  /** Action row, pinned below the body. Typically the confirm/cancel buttons. */
  readonly footer?: ReactNode;
  /**
   * Element to focus on open instead of the dialog itself -- a form's first
   * field, say. See the focus note on {@link Dialog}.
   */
  readonly initialFocusRef?: RefObject<HTMLElement | null>;
}

/**
 * Elements that can hold focus. Deliberately not filtered by visibility:
 * jsdom reports every element as unrendered (`offsetParent` is always null),
 * so a visibility filter would make the trap untestable while adding nothing
 * a dialog -- which controls its own subtree -- actually needs.
 */
const FOCUSABLE_SELECTOR = [
  "a[href]",
  "button:not([disabled])",
  "input:not([disabled]):not([type='hidden'])",
  "select:not([disabled])",
  "textarea:not([disabled])",
  "[tabindex]:not([tabindex='-1'])",
].join(",");

const focusableWithin = (root: HTMLElement): readonly HTMLElement[] =>
  Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)).filter(
    (element) => !element.hasAttribute("inert"),
  );

/**
 * Dialog is a modal, hand-rolled rather than built on `<dialog>`.
 *
 * WHY NOT NATIVE `<dialog>`: `showModal()` would hand us the top layer,
 * `::backdrop`, background inertness and Escape for free -- but jsdom (29.x,
 * the environment `npm test` runs in) implements none of `HTMLDialogElement`:
 * `showModal` and `close` are `undefined`. Every behaviour this bead is
 * accountable for would become untestable, or testable only against a shim
 * that is not the thing shipping. The behaviours are therefore implemented
 * explicitly below, which is also what makes them assertable in Dialog.test.tsx.
 *
 * ACCESSIBILITY DECISIONS:
 *
 *   - `role="dialog"` + `aria-modal="true"`, named by its heading through
 *     `aria-labelledby` (not `aria-label`, so the name and the visible title
 *     cannot drift apart) and optionally described by `aria-describedby`.
 *   - Background content is made `inert` while the dialog is open: every
 *     direct child of `<body>` except this dialog's own scrim. `inert`, not
 *     `aria-hidden`, because `aria-hidden` hides a subtree from assistive
 *     technology while leaving it tabbable -- focusable content inside an
 *     `aria-hidden` subtree is an ARIA violation, and the sequential focus
 *     order would still walk out of the dialog.
 *   - The Tab/Shift+Tab cycle is trapped as well. That is redundant with
 *     `inert` in a modern browser, and deliberately so: it is the layer that
 *     survives a stray focusable element rendered inside the scrim, and it
 *     is the layer a test can prove.
 *   - Initial focus goes to the dialog container (`tabindex=-1`), so the
 *     name and description are announced on open, rather than to the close
 *     button, which would announce "Close, button" and nothing else. Pass
 *     `initialFocusRef` when the dialog is a form and the first field is the
 *     better landing point.
 *   - Focus is restored to whatever was focused when the dialog opened --
 *     normally the trigger -- provided that element is still in the document.
 *   - Dismissal by Escape, by the close button, and by pressing the pointer
 *     down on the scrim. Pointer-DOWN on the scrim, not click: a click fires
 *     on the common ancestor, so selecting text inside the dialog and
 *     releasing the mouse over the scrim would otherwise close it.
 *
 * NOT HANDLED: stacked dialogs. One modal at a time; a second would
 * mistakenly un-inert the first on close.
 */
export function Dialog(props: DialogProps): ReactElement | null {
  if (!props.open) return null;
  // Keyed on nothing: mounting IS opening, so the panel's effects are the
  // open/close lifecycle and need no `open` in their dependency lists.
  return <DialogPanel {...props} />;
}

function DialogPanel({
  title,
  description,
  onClose,
  children,
  footer,
  initialFocusRef,
}: DialogProps): ReactElement {
  const scrimRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const descriptionId = useId();
  // Kept in a ref so the focus effect below can have an empty dependency
  // list: it must run exactly once per open. Listing `initialFocusRef` would
  // let a consumer passing a fresh ref object re-run the cleanup mid-open,
  // yanking focus back to the trigger while the dialog is still up.
  const initialFocusRefRef = useRef(initialFocusRef);

  useEffect(() => {
    const scrim = scrimRef.current;
    const marked: Element[] = [];
    for (const child of Array.from(document.body.children)) {
      if (child === scrim || child.hasAttribute("inert")) continue;
      // setAttribute rather than the `inert` IDL property: jsdom does not
      // implement the property, so assigning it would silently create an
      // expando and set nothing on the element.
      child.setAttribute("inert", "");
      marked.push(child);
    }
    return () => {
      for (const element of marked) element.removeAttribute("inert");
    };
  }, []);

  useEffect(() => {
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previousOverflow;
    };
  }, []);

  // Declared LAST on purpose. React runs an unmounting component's effect
  // cleanups in declaration order, so this one runs after the inert effect
  // above has un-inerted the page -- and focusing an element inside an inert
  // subtree is a silent no-op in a real browser. Restoration would fail in
  // production while still passing in jsdom, which does not implement inert.
  useEffect(() => {
    const restoreTo = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const target = initialFocusRefRef.current?.current ?? panelRef.current;
    target?.focus();
    return () => {
      if (restoreTo !== null && restoreTo.isConnected) restoreTo.focus();
    };
  }, []);

  const handleKeyDown = (event: KeyboardEvent<HTMLDivElement>): void => {
    if (event.key === "Escape") {
      event.preventDefault();
      onClose();
      return;
    }
    if (event.key !== "Tab") return;
    const panel = panelRef.current;
    if (panel === null) return;
    const focusable = focusableWithin(panel);
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (first === undefined || last === undefined) {
      // Defensive: the close button means a panel always has at least one
      // focusable child today. Keep focus on the dialog rather than letting
      // it walk out onto the page behind if that ever stops being true.
      event.preventDefault();
      panel.focus();
      return;
    }
    const active = document.activeElement;
    if (!(active instanceof HTMLElement) || !focusable.includes(active)) {
      // Focus is on the panel itself (where it lands on open) or somewhere
      // outside the cycle. Do not delegate to the browser's sequential
      // navigation: from a `tabindex=-1` container it is inconsistent about
      // where "next" is, and getting it wrong steps onto the page behind.
      event.preventDefault();
      (event.shiftKey ? last : first).focus();
      return;
    }
    if (event.shiftKey && active === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && active === last) {
      event.preventDefault();
      first.focus();
    }
  };

  const handleScrimMouseDown = (event: MouseEvent<HTMLDivElement>): void => {
    if (event.target === event.currentTarget) onClose();
  };

  return createPortal(
    <div
      ref={scrimRef}
      className={styles.scrim}
      onMouseDown={handleScrimMouseDown}
      onKeyDown={handleKeyDown}
    >
      <div
        ref={panelRef}
        className={styles.panel}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description === undefined ? undefined : descriptionId}
        tabIndex={-1}
      >
        <div className={styles.header}>
          <h2 id={titleId} className={styles.title}>
            {title}
          </h2>
          {/* A plain <button>: loam-nvb.5's Button does not exist on this
              branch, and a second Button component would be worse than an
              element. Swap it when the branches meet. */}
          <button type="button" className={styles.close} onClick={onClose} aria-label="Close">
            <span aria-hidden="true">&times;</span>
          </button>
        </div>
        {description !== undefined && (
          <p id={descriptionId} className={styles.description}>
            {description}
          </p>
        )}
        <div className={styles.body}>{children}</div>
        {footer !== undefined && <div className={styles.footer}>{footer}</div>}
      </div>
    </div>,
    document.body,
  );
}
