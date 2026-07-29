import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Pager } from "./Pager";

// The interesting claims are arithmetic ones -- which page a given
// limit/offset/total is, which offset each button asks for, and where the
// range ends -- so every case asserts a value that a sign error would change.

const noop = (): void => {};

describe("Pager", () => {
  it("renders nothing when everything fits on one page", () => {
    const { container } = render(
      <Pager total={25} limit={25} offset={0} onOffsetChange={noop} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when the limit is the server default sentinel (0)", () => {
    // Page.limit = 0 means "use the server default" (proto/loam/v1/common.proto);
    // there is no page size to divide by, so there is no pager to draw.
    const { container } = render(
      <Pager total={500} limit={0} offset={0} onOffsetChange={noop} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("is a landmark distinguishable from the app's own navigation", () => {
    render(<Pager total={120} limit={25} offset={0} onOffsetChange={noop} />);
    expect(screen.getByRole("navigation", { name: "Pagination" })).toBeInTheDocument();
  });

  it("disables the previous control on the first page and drops its page number", () => {
    // "Go to page 0" would be a lie; a control with no destination gets the
    // generic name.
    render(<Pager total={120} limit={25} offset={0} onOffsetChange={noop} />);
    expect(screen.getByRole("button", { name: "Previous page" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Go to page 2" })).toBeEnabled();
  });

  it("disables the next control on the last page", () => {
    render(<Pager total={120} limit={25} offset={100} onOffsetChange={noop} />);
    // 120 records at 25 a page is 5 pages; offset 100 is the fifth.
    expect(screen.getByRole("button", { name: "Next page" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Go to page 4" })).toBeEnabled();
  });

  it("names each control by the page it goes to, not just 'Next'", () => {
    render(<Pager total={120} limit={25} offset={50} onOffsetChange={noop} />);
    expect(screen.getByRole("button", { name: "Go to page 2" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Go to page 4" })).toBeEnabled();
  });

  it("announces the position and range in a live region", () => {
    render(<Pager total={120} limit={25} offset={50} onOffsetChange={noop} itemNoun="proposals" />);
    expect(screen.getByRole("status")).toHaveTextContent(
      "Page 3 of 5 · showing 51–75 of 120 proposals",
    );
  });

  it("ends the range at the total on a short final page", () => {
    render(<Pager total={120} limit={50} offset={100} onOffsetChange={noop} />);
    expect(screen.getByRole("status")).toHaveTextContent("showing 101–120 of 120");
  });

  it("asks for the offset of the next page", async () => {
    const user = userEvent.setup();
    const onOffsetChange = vi.fn();
    render(<Pager total={120} limit={25} offset={50} onOffsetChange={onOffsetChange} />);
    await user.click(screen.getByRole("button", { name: "Go to page 4" }));
    expect(onOffsetChange).toHaveBeenCalledWith(75);
  });

  it("asks for the offset of the previous page", async () => {
    const user = userEvent.setup();
    const onOffsetChange = vi.fn();
    render(<Pager total={120} limit={25} offset={50} onOffsetChange={onOffsetChange} />);
    await user.click(screen.getByRole("button", { name: "Go to page 2" }));
    expect(onOffsetChange).toHaveBeenCalledWith(25);
  });

  it("clamps an offset that has fallen past the end of the result set", () => {
    // Records can be removed between the query and the render; "Page 9 of 5"
    // would be worse than landing on the last page.
    render(<Pager total={120} limit={25} offset={400} onOffsetChange={noop} />);
    expect(screen.getByRole("status")).toHaveTextContent("Page 5 of 5");
    expect(screen.getByRole("button", { name: "Next page" })).toBeDisabled();
  });
});
