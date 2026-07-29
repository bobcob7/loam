import { fireEvent, render, screen } from "@testing-library/react";
import { Button } from "./Button";
import { Field } from "./Field";
import { Form, FormActions } from "./Form";

// The claim worth testing about Form is that a submit never reaches the
// browser. In an SPA behind basic auth a real form submission is a full page
// navigation: state gone, and the failure looks like "the app randomly
// reloads", which is expensive to trace back to a missing preventDefault.
// `fireEvent` returns false when a handler prevented the default, so that is
// asserted directly rather than inferred.
describe("Form", () => {
  it("prevents the browser's own submission", () => {
    render(
      <Form aria-label="Enrol repo" onSubmit={vi.fn()}>
        <Button type="submit">Enrol</Button>
      </Form>,
    );
    const notPrevented = fireEvent.submit(screen.getByRole("form", { name: "Enrol repo" }));
    expect(notPrevented).toBe(false);
  });

  it("calls onSubmit when its submit Button is clicked", () => {
    const onSubmit = vi.fn();
    render(
      <Form aria-label="Enrol repo" onSubmit={onSubmit}>
        <Field label="Upstream URL" />
        <FormActions>
          <Button>Cancel</Button>
          <Button type="submit" variant="primary">
            Enrol
          </Button>
        </FormActions>
      </Form>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Enrol" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("is not submitted by a Button that did not ask to be a submit button", () => {
    const onSubmit = vi.fn();
    render(
      <Form aria-label="Enrol repo" onSubmit={onSubmit}>
        <FormActions>
          <Button>Cancel</Button>
        </FormActions>
      </Form>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("passes attributes through to the native form element", () => {
    render(
      <Form aria-label="Enrol repo" noValidate onSubmit={vi.fn()}>
        <Button type="submit">Enrol</Button>
      </Form>,
    );
    expect(screen.getByRole("form", { name: "Enrol repo" })).toHaveAttribute("novalidate");
  });
});
