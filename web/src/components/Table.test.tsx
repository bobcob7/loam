import { render, screen } from "@testing-library/react";
import { Table, type TableColumn, type TableProps } from "./Table";

// Queried by role throughout, deliberately: `getByRole("rowheader")` passes
// only if the cell really is a `<th scope="row">`, and
// `getByRole("table", { name })` passes only if the caption really is the
// table's accessible name. A getByTestId or snapshot suite would go green on
// a table built entirely from `<div>`s.

interface Repo {
  readonly repo: string;
  readonly branch: string;
  readonly branchCount: number;
}

const repos: readonly Repo[] = [
  { repo: "acme/widgets", branch: "main", branchCount: 3 },
  { repo: "acme/gadgets", branch: "trunk", branchCount: 1 },
];

const columns: readonly TableColumn<Repo>[] = [
  { key: "repo", header: "Repo", cell: (row) => row.repo, mono: true },
  { key: "branch", header: "Indexed branch", cell: (row) => row.branch, mono: true },
  { key: "branches", header: "Target branches", cell: (row) => row.branchCount, align: "end" },
];

const renderRepos = (overrides: Partial<TableProps<Repo>> = {}): void => {
  render(
    <Table
      caption="Enrolled repos"
      columns={columns}
      rows={repos}
      rowKey={(row) => row.repo}
      {...overrides}
    />,
  );
};

const currentRows = (): readonly string[] =>
  screen
    .getAllByRole("row")
    .filter((row) => row.hasAttribute("aria-current"))
    .map((row) => row.textContent ?? "");

describe("Table", () => {
  it("takes its accessible name from the caption even though the caption is visually hidden", () => {
    renderRepos();
    expect(screen.getByRole("table", { name: "Enrolled repos" })).toBeInTheDocument();
  });

  it("marks column headings as column headers", () => {
    renderRepos();
    expect(screen.getAllByRole("columnheader").map((cell) => cell.textContent)).toEqual([
      "Repo",
      "Indexed branch",
      "Target branches",
    ]);
  });

  it("labels each row with the first column when no column claims the row header", () => {
    renderRepos();
    const rowHeaders = screen.getAllByRole("rowheader");
    expect(rowHeaders.map((cell) => cell.textContent)).toEqual(["acme/widgets", "acme/gadgets"]);
    // The role above comes from the element and its position; `scope` is the
    // separate half of the contract, and the half some screen readers use to
    // decide which header labels a cell.
    expect(rowHeaders.map((cell) => cell.getAttribute("scope"))).toEqual(["row", "row"]);
  });

  it("uses the column that declares rowHeader instead of the first one", () => {
    renderRepos({
      columns: [
        { key: "branches", header: "Target branches", cell: (row) => row.branchCount },
        { key: "repo", header: "Repo", cell: (row) => row.repo, rowHeader: true },
      ],
    });
    expect(screen.getAllByRole("rowheader").map((cell) => cell.textContent)).toEqual([
      "acme/widgets",
      "acme/gadgets",
    ]);
  });

  it("marks the selected row with aria-current, so selection is not carried by colour alone", () => {
    renderRepos({ selectedRowKey: "acme/gadgets" });
    expect(currentRows()).toEqual([expect.stringContaining("acme/gadgets")]);
  });

  it("marks no row current when nothing is selected", () => {
    renderRepos();
    expect(currentRows()).toEqual([]);
  });

  it("replaces the rows with a message spanning every column when there is nothing to show", () => {
    renderRepos({ rows: [], emptyMessage: "No repos enrolled." });
    expect(screen.getByRole("cell", { name: "No repos enrolled." })).toHaveAttribute(
      "colspan",
      String(columns.length),
    );
    expect(screen.queryByRole("rowheader")).not.toBeInTheDocument();
  });

  it("makes the scroll container keyboard-reachable and names it", () => {
    // A scroll container that cannot be focused cannot be scrolled by
    // keyboard (WCAG 2.1.1), and an unnamed tab stop is an anonymous one.
    renderRepos();
    const group = screen.getByRole("group");
    expect(group).toHaveAttribute("tabindex", "0");
    expect(group).toHaveAccessibleName("Enrolled repos");
  });
});
