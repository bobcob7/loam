import { render, screen } from "@testing-library/react";
import { Placeholder } from "./Placeholder";

// These two cases exist to prove the scaffold, not the component: the first
// fails unless src/test/setup.ts imported the jest-dom matchers, and the
// second fails unless the same file's afterEach(cleanup) tore the previous
// render down. An empty suite would go green without either.
describe("Placeholder", () => {
  it("renders its message, with jest-dom matchers registered by the setup file", () => {
    render(<Placeholder message="Loam admin" />);
    const heading = screen.getByRole("heading", { level: 1 });
    expect(heading).toBeInTheDocument();
    expect(heading).toHaveTextContent("Loam admin");
  });

  it("leaves nothing mounted from the previous test, proving afterEach cleanup ran", () => {
    expect(screen.queryByRole("heading", { level: 1 })).not.toBeInTheDocument();
  });
});
