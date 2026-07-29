import type { ButtonHTMLAttributes, MouseEvent, ReactElement, ReactNode } from "react";
import styles from "./Button.module.css";

export type ButtonVariant = "primary" | "secondary" | "danger";
export type ButtonSize = "md" | "sm";

export interface ButtonProps
  extends Omit<
    ButtonHTMLAttributes<HTMLButtonElement>,
    "className" | "aria-disabled" | "aria-busy"
  > {
  /** `secondary` (the default) is the neutral action; `primary` is the one filled
      action on a screen; `danger` is destructive (RemoveRepo, DeleteRole). */
  readonly variant?: ButtonVariant;
  /** `sm` matches --control-height-sm for in-row and pager actions. */
  readonly size?: ButtonSize;
  /** An in-flight mutation. See the note on disabled-vs-pending below. */
  readonly pending?: boolean;
  readonly children: ReactNode;
}

/**
 * Button is a native `<button>` with the app's three variants and a pending
 * state.
 *
 * Two deliberate calls:
 *
 * `type` defaults to `"button"`, not the HTML default of `"submit"`. A button
 * placed in a form for some unrelated action (Probe, Add row, Cancel) that
 * silently submits it is the single most common bug in hand-rolled forms;
 * Form's submit button opts in with `type="submit"`.
 *
 * `disabled` and `pending` are not the same state and are not implemented the
 * same way. `disabled` means the action is unavailable on its own terms
 * (DeleteRole on a builtin role) and maps to the native attribute: not
 * focusable, not clickable. `pending` means the same action is momentarily
 * in flight, and maps to `aria-disabled` + `aria-busy` with the click
 * suppressed here — the element stays in the tab order, because a submit
 * button that removes itself from the tab order at the moment it is pressed
 * drops keyboard focus to the body and loses the screen-reader user's place
 * exactly when the app has something to say. Suppressing the click in the
 * handler is required, since `aria-disabled` is an announcement, not a
 * behaviour.
 */
export function Button({
  variant = "secondary",
  size = "md",
  pending = false,
  type = "button",
  onClick,
  children,
  ...rest
}: ButtonProps): ReactElement {
  const className = [
    styles.root,
    styles[variant],
    styles[size],
    pending ? styles.pending : undefined,
  ]
    .filter((name): name is string => typeof name === "string")
    .join(" ");
  const handleClick = (event: MouseEvent<HTMLButtonElement>): void => {
    if (pending) {
      event.preventDefault();
      event.stopPropagation();
      return;
    }
    onClick?.(event);
  };
  return (
    <button
      {...rest}
      type={type}
      className={className}
      aria-disabled={pending ? true : undefined}
      aria-busy={pending ? true : undefined}
      onClick={handleClick}
    >
      {children}
    </button>
  );
}
