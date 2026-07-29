import { render, screen } from "@testing-library/react";
import { StatusBadge, type StatusIntent } from "./StatusBadge";
import styles from "./StatusBadge.module.css";

const intents: readonly StatusIntent[] = ["neutral", "info", "success", "warning", "danger"];

/** Looks up a CSS Module class, failing loudly rather than via `!` if the
    module ever stops exporting it (e.g. a renamed selector). */
const classFor = (intent: StatusIntent): string => {
  const name = styles[intent];
  if (name === undefined) throw new Error(`StatusBadge.module.css has no .${intent} class`);
  return name;
};

// StatusBadge's whole job is two things: carry its label as real text (so a
// screen reader gets what a sighted user gets from colour -- SC 1.4.1), and
// apply the tint for the intent it was given. Unlike Button, where class
// names are mostly implementation detail alongside more meaningful behaviour
// (see Button.test.tsx), here both ARE the component's entire observable
// contract, so both are asserted directly against the real CSS Module
// export rather than a hard-coded string.
describe("StatusBadge", () => {
  it.each(intents)("renders its children as the %s pill's text content", (intent) => {
    render(<StatusBadge intent={intent}>Some label</StatusBadge>);
    expect(screen.getByText("Some label")).toBeInTheDocument();
  });

  it.each(intents)("applies the %s intent's style, and no other intent's", (intent) => {
    render(<StatusBadge intent={intent}>Some label</StatusBadge>);
    const badge = screen.getByText("Some label");
    expect(badge.className).toContain(classFor(intent));
    for (const other of intents.filter((candidate) => candidate !== intent)) {
      expect(badge.className).not.toContain(classFor(other));
    }
  });
});
