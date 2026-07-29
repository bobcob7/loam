import { render, screen, within } from "@testing-library/react";
import { Button } from "./Button";
import { ErrorBanner } from "./ErrorBanner";

// The one thing that makes this component more than a red box is that it is a
// live region: an error appearing after a mutation has to be announced without
// the user going looking for it. `getByRole("alert")` is that assertion — it
// resolves only through the accessibility tree, so it fails the moment the
// role is dropped, which a text query would not.
describe("ErrorBanner", () => {
  it("announces its message as an alert", () => {
    render(<ErrorBanner message="upstream host unreachable" />);
    expect(screen.getByRole("alert")).toHaveTextContent("upstream host unreachable");
  });

  it("includes its title in the announced region, not beside it", () => {
    render(<ErrorBanner title="Could not enrol repo" message="upstream host unreachable" />);
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("Could not enrol repo");
    expect(alert).toHaveTextContent("upstream host unreachable");
  });

  it("renders no title element when none is given", () => {
    render(<ErrorBanner message="upstream host unreachable" />);
    expect(screen.getByRole("alert").textContent).toBe("upstream host unreachable");
  });

  it("renders an action inside the region so it is announced with the error", () => {
    render(
      <ErrorBanner message="upstream host unreachable">
        <Button>Retry</Button>
      </ErrorBanner>,
    );
    const action = within(screen.getByRole("alert")).getByRole("button", { name: "Retry" });
    expect(action).toBeInTheDocument();
  });

  it("does not steal focus, since the alert role already announces it", () => {
    render(<ErrorBanner message="upstream host unreachable" />);
    expect(screen.getByRole("alert")).not.toHaveFocus();
    expect(document.activeElement).toBe(document.body);
  });
});
