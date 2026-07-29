import { useId } from "react";
import type {
  InputHTMLAttributes,
  ReactElement,
  SelectHTMLAttributes,
  TextareaHTMLAttributes,
} from "react";
import styles from "./Field.module.css";

interface FieldOwnProps {
  /** Rendered in a real `<label for>`; never a placeholder standing in for one. */
  readonly label: string;
  /** Persistent helper text, e.g. an expected format. */
  readonly hint?: string;
  /** A validation message — typically one `invalid_argument` detail. */
  readonly error?: string;
}

/** The three attributes Field owns; a caller passing them would break the wiring. */
type Controlled<T> = Omit<T, "aria-invalid" | "aria-describedby" | "className">;

export type FieldProps =
  | ({ readonly as?: "input" } & FieldOwnProps &
      Controlled<InputHTMLAttributes<HTMLInputElement>>)
  | ({ readonly as: "textarea" } & FieldOwnProps &
      Controlled<TextareaHTMLAttributes<HTMLTextAreaElement>>)
  | ({ readonly as: "select" } & FieldOwnProps &
      Controlled<SelectHTMLAttributes<HTMLSelectElement>>);

/**
 * Field is a labelled native control — input, textarea or select — with a
 * hint slot and an error slot.
 *
 * It renders the control itself rather than accepting one as a child, because
 * the association it exists to guarantee (`label for` ↔ control `id`,
 * `aria-describedby` ↔ hint and error ids, `aria-invalid` when there is an
 * error) is exactly the kind that is invisible when it is missing: a stray
 * `<label>` next to an unassociated input looks correct on screen and is
 * unusable with a screen reader. Passing the control in would make that
 * wiring the caller's job on four screens; owning it here makes it
 * unforgettable, which is why those three attributes are typed out of the
 * props.
 *
 * The `id` is generated with `useId` unless one is supplied, so two Fields
 * with the same label on one screen never collide.
 *
 * The error is announced through `aria-invalid` + `aria-describedby`, not a
 * live region. A form that fails validation moves focus to the first invalid
 * control (the screens' job), and the description is read on arrival; making
 * every field error assertive instead would talk over itself the moment two
 * fields fail at once. ErrorBanner is the live region for the errors that
 * belong to no field.
 */
export function Field(props: FieldProps): ReactElement {
  const generatedId = useId();
  const { label, hint, error } = props;
  const id = props.id ?? generatedId;
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;
  const describedBy = [
    hint === undefined ? undefined : hintId,
    error === undefined ? undefined : errorId,
  ]
    .filter((value): value is string => value !== undefined)
    .join(" ");
  const shared = {
    id,
    "aria-invalid": error === undefined ? undefined : (true as const),
    "aria-describedby": describedBy === "" ? undefined : describedBy,
  };
  const controlClass = (extra?: string): string =>
    [styles.control, extra].filter((name): name is string => typeof name === "string").join(" ");
  const control = ((): ReactElement => {
    switch (props.as) {
      case "textarea": {
        const { as: _as, label: _label, hint: _hint, error: _error, ...rest } = props;
        return <textarea {...rest} {...shared} className={controlClass(styles.textarea)} />;
      }
      case "select": {
        const { as: _as, label: _label, hint: _hint, error: _error, ...rest } = props;
        return <select {...rest} {...shared} className={controlClass()} />;
      }
      default: {
        const { as: _as, label: _label, hint: _hint, error: _error, ...rest } = props;
        return <input {...rest} {...shared} className={controlClass()} />;
      }
    }
  })();
  return (
    <div className={styles.root}>
      <label className={styles.label} htmlFor={id}>
        {label}
        {/* aria-hidden: `required` is already on the control, so a screen
            reader announces it; the asterisk is the sighted-user half. */}
        {props.required === true ? (
          <span className={styles.required} aria-hidden="true">
            *
          </span>
        ) : null}
      </label>
      {control}
      {hint === undefined ? null : (
        <p id={hintId} className={styles.hint}>
          {hint}
        </p>
      )}
      {error === undefined ? null : (
        <p id={errorId} className={styles.error}>
          {error}
        </p>
      )}
    </div>
  );
}
