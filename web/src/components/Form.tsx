import type { FormEvent, FormHTMLAttributes, ReactElement, ReactNode } from "react";
import styles from "./Form.module.css";

export interface FormProps
  extends Omit<FormHTMLAttributes<HTMLFormElement>, "onSubmit" | "className"> {
  /**
   * Called on submit, after the default is prevented. It takes no event: the
   * one thing a caller ever did with it was `preventDefault()`, and forgetting
   * that reloads the SPA and loses every bit of state — so Form does it and
   * does not hand out the means to skip it. Field values come from the
   * screen's own React state, not from the event.
   */
  readonly onSubmit: () => void;
  readonly children: ReactNode;
}

/**
 * Form is a native `<form>` that stacks its children and always prevents the
 * browser's own submission.
 *
 * Native validation is left switched on (no `noValidate`): `required` on a
 * Field is announced and enforced by the user agent for free, and a screen
 * that wants to own validation entirely can still pass `noValidate` through.
 * Keeping a real `<form>` rather than a `<div>` plus a click handler is what
 * makes Enter-to-submit work from inside a text input, which is keyboard
 * behaviour no component code has to implement.
 */
export function Form({ onSubmit, children, ...rest }: FormProps): ReactElement {
  const handleSubmit = (event: FormEvent<HTMLFormElement>): void => {
    event.preventDefault();
    onSubmit();
  };
  return (
    <form {...rest} className={styles.root} onSubmit={handleSubmit}>
      {children}
    </form>
  );
}

export interface FormActionsProps {
  readonly children: ReactNode;
}

/** The trailing row of Buttons in a Form. Layout only — no semantics. */
export function FormActions({ children }: FormActionsProps): ReactElement {
  return <div className={styles.actions}>{children}</div>;
}
