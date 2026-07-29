import { fireEvent, render, screen } from "@testing-library/react";
import type { ReactElement } from "react";
import { useRef } from "react";
import { Dialog } from "./Dialog";
import { Field } from "./Field";

// Every query here goes through the accessibility tree on purpose.
// `getByLabelText` only finds a control that a label is genuinely associated
// with, and `toHaveAccessibleDescription` only passes if aria-describedby
// points at ids that exist — so these are assertions about the wiring, not
// about the markup. A getByTestId version of this suite would pass with the
// label association removed.
describe("Field", () => {
  it("associates its label with the control it renders", () => {
    render(<Field label="Upstream URL" />);
    const input = screen.getByLabelText("Upstream URL");
    expect(input.tagName).toBe("INPUT");
  });

  it("gives each instance a distinct id, so two identical labels do not collide", () => {
    render(
      <>
        <Field label="Branch" defaultValue="main" />
        <Field label="Branch" defaultValue="next" />
      </>,
    );
    // The length assertion is load-bearing: if both Fields shared an id, both
    // labels would resolve to the *same* input and this query would return one
    // element, at which point comparing ids would pass for the wrong reason.
    const inputs = screen.getAllByLabelText("Branch");
    expect(inputs).toHaveLength(2);
    const [first, second] = inputs;
    expect(first).toHaveValue("main");
    expect(second).toHaveValue("next");
    expect(first?.id).not.toBe(second?.id);
  });

  it("honours an explicit id", () => {
    render(<Field label="Upstream URL" id="upstream" />);
    expect(screen.getByLabelText("Upstream URL")).toHaveAttribute("id", "upstream");
  });

  it("is editable through its label, proving the association is functional", () => {
    const onChange = vi.fn();
    render(<Field label="Upstream URL" onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("Upstream URL"), {
      target: { value: "https://forge.example/x" },
    });
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  describe("error", () => {
    it("marks the control invalid and describes it with the message", () => {
      render(<Field label="Upstream URL" error="must be an absolute URL" />);
      const input = screen.getByLabelText("Upstream URL");
      expect(input).toHaveAttribute("aria-invalid", "true");
      expect(input).toHaveAccessibleDescription("must be an absolute URL");
    });

    it("leaves the control valid and undescribed when there is none", () => {
      render(<Field label="Upstream URL" />);
      const input = screen.getByLabelText("Upstream URL");
      expect(input).not.toHaveAttribute("aria-invalid");
      expect(input).toHaveAccessibleDescription("");
    });

    it("describes the control with both the hint and the error when both are present", () => {
      render(<Field label="Upstream URL" hint="https://host/owner/repo" error="repo not found" />);
      expect(screen.getByLabelText("Upstream URL")).toHaveAccessibleDescription(
        "https://host/owner/repo repo not found",
      );
    });
  });

  describe("required", () => {
    it("puts required on the control itself, not only in the visible label", () => {
      render(<Field label="Upstream URL" required />);
      expect(screen.getByLabelText(/Upstream URL/)).toBeRequired();
    });

    it("hides the asterisk from assistive technology, which already announces required", () => {
      render(<Field label="Upstream URL" required />);
      // The accessible name is the label text alone; the "*" is decorative.
      expect(screen.getByRole("textbox", { name: "Upstream URL" })).toBeInTheDocument();
    });
  });

  describe("ref", () => {
    // Regression for loam-cucq: Field must forward a ref to the control it
    // renders (not merely accept the prop) so a consumer can hand that ref
    // straight to Dialog's initialFocusRef -- the only way, per Dialog's own
    // effect-ordering note, to land initial focus on a Field-rendered input
    // instead of the dialog panel itself. A type-only ref (accepted but
    // never attached to the underlying <input>) would pass a type check and
    // still fail this: the assertion is about document.activeElement, not
    // about the ref's declared type.
    it("receives the ref that Dialog uses to place initial focus", () => {
      function Harness(): ReactElement {
        const initialFocusRef = useRef<HTMLInputElement>(null);
        return (
          <Dialog open title="Enroll a repo" onClose={() => {}} initialFocusRef={initialFocusRef}>
            <Field label="Upstream URL" ref={initialFocusRef} />
          </Dialog>
        );
      }
      render(<Harness />);
      expect(screen.getByLabelText("Upstream URL")).toBe(document.activeElement);
    });
  });

  describe("as", () => {
    it("renders a labelled textarea", () => {
      render(<Field as="textarea" label="Instructions" error="required" />);
      const control = screen.getByLabelText("Instructions");
      expect(control.tagName).toBe("TEXTAREA");
      expect(control).toHaveAccessibleDescription("required");
    });

    it("renders a labelled select whose options are selectable", () => {
      const onChange = vi.fn();
      render(
        <Field as="select" label="Indexed branch" defaultValue="main" onChange={onChange}>
          <option value="main">main</option>
          <option value="next">next</option>
        </Field>,
      );
      const select = screen.getByLabelText("Indexed branch");
      expect(select.tagName).toBe("SELECT");
      fireEvent.change(select, { target: { value: "next" } });
      expect(onChange).toHaveBeenCalledTimes(1);
    });
  });
});
