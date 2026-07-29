import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useRef, useState, type ReactElement, type ReactNode } from "react";
import { Dialog } from "./Dialog";

// Every case here is a behaviour that can actually break: focus landing,
// focus wrapping, focus restoration, Escape, the scrim's press target, and
// background inertness. None of them is observable from the markup alone,
// which is why a snapshot of this component would be worthless.

interface HarnessProps {
  readonly children?: ReactNode;
  readonly description?: string;
  readonly focusFirstField?: boolean;
}

/**
 * A trigger plus the dialog it opens -- the real shape a screen uses, and
 * the only way to test that focus returns to the trigger on close.
 */
function Harness({ children, description, focusFirstField = false }: HarnessProps): ReactElement {
  const [open, setOpen] = useState(false);
  const fieldRef = useRef<HTMLInputElement>(null);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Enroll repo
      </button>
      <Dialog
        open={open}
        title="Enroll a repo"
        description={description}
        onClose={() => setOpen(false)}
        footer={
          <button type="button" onClick={() => setOpen(false)}>
            Cancel
          </button>
        }
        {...(focusFirstField ? { initialFocusRef: fieldRef } : {})}
      >
        {children ?? <input ref={fieldRef} aria-label="Upstream URL" />}
      </Dialog>
    </>
  );
}

const openDialog = async (
  user: ReturnType<typeof userEvent.setup>,
): Promise<HTMLElement> => {
  await user.click(screen.getByRole("button", { name: "Enroll repo" }));
  return screen.getByRole("dialog", { name: "Enroll a repo" });
};

describe("Dialog", () => {
  it("renders nothing while closed", () => {
    render(<Harness />);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("is a modal dialog named by its own heading", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const dialog = await openDialog(user);
    expect(dialog).toHaveAttribute("aria-modal", "true");
    // Named by aria-labelledby pointing at the rendered <h2>, so the
    // accessible name cannot drift from the visible title.
    expect(dialog).toHaveAccessibleName("Enroll a repo");
  });

  it("is described by its description when one is given", async () => {
    const user = userEvent.setup();
    render(<Harness description="Loam clones it and indexes the default branch." />);
    const dialog = await openDialog(user);
    expect(dialog).toHaveAccessibleDescription("Loam clones it and indexes the default branch.");
  });

  it("puts focus on the dialog itself so its name and description are announced", async () => {
    const user = userEvent.setup();
    render(<Harness description="Loam clones it and indexes the default branch." />);
    const dialog = await openDialog(user);
    expect(dialog).toHaveFocus();
  });

  it("puts focus on initialFocusRef's element instead when one is supplied", async () => {
    const user = userEvent.setup();
    render(<Harness focusFirstField />);
    await openDialog(user);
    expect(screen.getByLabelText("Upstream URL")).toHaveFocus();
  });

  it("closes on Escape", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await openDialog(user);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("returns focus to the trigger after closing", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await openDialog(user);
    expect(screen.getByRole("button", { name: "Enroll repo" })).not.toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.getByRole("button", { name: "Enroll repo" })).toHaveFocus();
  });

  it("closes when the close button is pressed", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await openDialog(user);
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("closes when the pointer goes down on the scrim", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const dialog = await openDialog(user);
    const scrim = dialog.parentElement;
    expect(scrim).not.toBeNull();
    if (scrim === null) return;
    await user.pointer({ target: scrim, keys: "[MouseLeft]" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("stays open when the pointer goes down inside the panel", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const dialog = await openDialog(user);
    await user.pointer({ target: dialog, keys: "[MouseLeft]" });
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("survives a text selection dragged out of the panel and released on the scrim", async () => {
    // The specific bug this guards: dismissing on the scrim's *click*. A
    // click fires on the common ancestor of the press and the release, so
    // pressing inside the dialog and releasing over the scrim produces a
    // click ON the scrim -- and tears the dialog down mid-gesture, which is
    // how you lose a half-typed form by selecting the text in it.
    // fireEvent, not userEvent: this hand-assembles the exact event sequence
    // a browser emits for a split-target drag.
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: "Enroll repo" }));
    const dialog = screen.getByRole("dialog", { name: "Enroll a repo" });
    const scrim = dialog.parentElement;
    expect(scrim).not.toBeNull();
    if (scrim === null) return;
    fireEvent.mouseDown(dialog);
    fireEvent.mouseUp(scrim);
    fireEvent.click(scrim);
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("wraps Tab from the last focusable element back to the first", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await openDialog(user);
    const cancel = screen.getByRole("button", { name: "Cancel" });
    cancel.focus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Close" })).toHaveFocus();
  });

  it("wraps Shift+Tab from the first focusable element back to the last", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await openDialog(user);
    screen.getByRole("button", { name: "Close" }).focus();
    await user.tab({ shift: true });
    expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus();
  });

  it("never lets Tab reach the trigger behind it", async () => {
    const user = userEvent.setup();
    render(<Harness>{<p>Read-only content, no fields.</p>}</Harness>);
    const dialog = await openDialog(user);
    for (let press = 0; press < 6; press += 1) {
      await user.tab();
      expect(dialog.contains(document.activeElement)).toBe(true);
    }
  });

  it("makes the rest of the page inert while it is open, and restores it on close", async () => {
    const user = userEvent.setup();
    const { container } = render(<Harness />);
    expect(container).not.toHaveAttribute("inert");
    await openDialog(user);
    // Testing Library renders into a div appended to <body>; the dialog
    // portals a sibling scrim next to it. Everything but the scrim is
    // inerted -- not aria-hidden, which would leave the trigger tabbable.
    expect(container).toHaveAttribute("inert");
    await user.keyboard("{Escape}");
    expect(container).not.toHaveAttribute("inert");
  });

  it("locks and restores the page scroll", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    expect(document.body.style.overflow).toBe("");
    await openDialog(user);
    expect(document.body.style.overflow).toBe("hidden");
    await user.keyboard("{Escape}");
    expect(document.body.style.overflow).toBe("");
  });
});
