import { fireEvent, render, screen } from "@testing-library/react";
import type { FormEvent } from "react";
import { Button } from "./Button";

// What can actually be false about a Button: that it submits a form it was
// never meant to submit, that a pending click still fires the mutation, and
// that the pending state either says nothing to assistive technology or says
// it by dropping out of the tab order. Class names and markup shape are not
// asserted — a snapshot of them could not distinguish a working button from a
// broken one.
describe("Button", () => {
  it("exposes its children as its accessible name", () => {
    render(<Button>Enrol repo</Button>);
    expect(screen.getByRole("button", { name: "Enrol repo" })).toBeInTheDocument();
  });

  it("does not submit its surrounding form, because type defaults to button", () => {
    const onSubmit = vi.fn();
    render(
      <form onSubmit={onSubmit}>
        <Button>Probe</Button>
      </form>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Probe" }));
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('submits its surrounding form when asked with type="submit"', () => {
    const onSubmit = vi.fn((event: FormEvent) => {
      event.preventDefault();
    });
    render(
      <form onSubmit={onSubmit}>
        <Button type="submit">Save</Button>
      </form>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("calls onClick when it is neither disabled nor pending", () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>Retry</Button>);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  describe("pending", () => {
    it("announces itself as busy and unavailable", () => {
      render(<Button pending>Save</Button>);
      const button = screen.getByRole("button", { name: "Save" });
      expect(button).toHaveAttribute("aria-busy", "true");
      expect(button).toHaveAttribute("aria-disabled", "true");
    });

    it("swallows the click, since aria-disabled is an announcement and not a behaviour", () => {
      const onClick = vi.fn();
      render(
        <Button pending onClick={onClick}>
          Save
        </Button>,
      );
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      expect(onClick).not.toHaveBeenCalled();
    });

    it("does not submit its form while in flight", () => {
      const onSubmit = vi.fn();
      render(
        <form onSubmit={onSubmit}>
          <Button type="submit" pending>
            Save
          </Button>
        </form>,
      );
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      expect(onSubmit).not.toHaveBeenCalled();
    });

    it("keeps keyboard focus, unlike a natively disabled button", () => {
      render(<Button pending>Save</Button>);
      const button = screen.getByRole("button", { name: "Save" });
      button.focus();
      // A native `disabled` button is unfocusable, so this assertion is what
      // distinguishes the pending implementation from the disabled one.
      expect(button).toHaveFocus();
      expect(button).not.toBeDisabled();
    });
  });

  describe("disabled", () => {
    it("is disabled natively, so it leaves the tab order entirely", () => {
      render(<Button disabled>Delete role</Button>);
      const button = screen.getByRole("button", { name: "Delete role" });
      expect(button).toBeDisabled();
      button.focus();
      expect(button).not.toHaveFocus();
    });

    it("is not marked busy — unavailable is not the same state as in flight", () => {
      render(<Button disabled>Delete role</Button>);
      expect(screen.getByRole("button", { name: "Delete role" })).not.toHaveAttribute("aria-busy");
    });
  });
});
