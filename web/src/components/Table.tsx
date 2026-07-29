import type { ReactElement, ReactNode } from "react";
import { useId } from "react";
import styles from "./Table.module.css";

/**
 * One column of a {@link Table}, described rather than rendered, so a screen
 * declares its columns as data and the table stays responsible for the
 * markup (`<th scope>`, the empty row's `colSpan`, alignment).
 */
export interface TableColumn<Row> {
  /** Stable identity for React's key; not rendered. */
  readonly key: string;
  /** Column heading. A string, because it is also the cell's programmatic label. */
  readonly header: string;
  readonly cell: (row: Row) => ReactNode;
  /** `end` right-aligns numeric columns (counts, durations). Defaults to `start`. */
  readonly align?: "start" | "end";
  /**
   * Render the cell in the mono face. Set it for identifiers -- repo names,
   * branch names, commit SHAs, paths -- where character disambiguation
   * matters (src/styles/tokens.css -> Typography).
   */
  readonly mono?: boolean;
  /**
   * Render this column's cells as `<th scope="row">`, making it the row's
   * label. Defaults to the first column when no column claims it.
   */
  readonly rowHeader?: boolean;
}

export interface TableProps<Row> {
  /**
   * The table's accessible name, rendered as a `<caption>`. Required: an
   * unnamed table is a wall of cells to a screen reader navigating by table.
   */
  readonly caption: string;
  /**
   * Show the caption. Off by default because a screen normally carries a
   * visible heading directly above the table, and a second visible copy is
   * redundant -- the caption stays in the accessibility tree either way.
   */
  readonly captionVisible?: boolean;
  readonly columns: readonly TableColumn<Row>[];
  readonly rows: readonly Row[];
  readonly rowKey: (row: Row) => string;
  /** Shown in place of the rows when `rows` is empty. */
  readonly emptyMessage?: string;
  /** Pin the header while the table body scrolls. */
  readonly stickyHeader?: boolean;
  /** `rowKey` of the row to mark as the one currently in view. */
  readonly selectedRowKey?: string;
}

/**
 * Table renders a semantic `<table>` from a typed column config.
 *
 * Accessibility decisions, all deliberate:
 *
 *   - Real table semantics, not `<div role="grid">`. `role="grid"` promises
 *     a full two-dimensional keyboard model (arrow-key cell navigation);
 *     these are static data tables, and claiming grid without implementing
 *     it is worse than plain markup.
 *   - `caption` is required and is the accessible name. Column headers are
 *     `<th scope="col">` and one column per row is `<th scope="row">`, so a
 *     screen reader announces "Repo, acme/widgets" when reading a cell
 *     rather than reciting a bare value.
 *   - The scroll wrapper is focusable (`tabindex=0`, `role="group"` named by
 *     the caption). A horizontally scrollable region that cannot be reached
 *     by keyboard fails WCAG 2.1.1; the cost is one extra tab stop, which is
 *     the accepted trade-off because overflow cannot be detected at render
 *     time.
 *   - The selected row is marked `aria-current="true"`, not just tinted, so
 *     the selection is not carried by colour alone.
 */
export function Table<Row>({
  caption,
  captionVisible = false,
  columns,
  rows,
  rowKey,
  emptyMessage = "No results.",
  stickyHeader = false,
  selectedRowKey,
}: TableProps<Row>): ReactElement {
  const captionId = useId();
  const declaredRowHeader = columns.findIndex((column) => column.rowHeader === true);
  // Fall back to the first column so every row has a label; -1 (no columns)
  // simply matches no cell.
  const rowHeaderIndex = declaredRowHeader === -1 ? 0 : declaredRowHeader;
  return (
    <div
      className={stickyHeader ? `${styles.wrapper} ${styles.scrolls}` : styles.wrapper}
      role="group"
      aria-labelledby={captionId}
      tabIndex={0}
    >
      <table className={styles.table}>
        <caption
          id={captionId}
          className={captionVisible ? styles.caption : styles.captionHidden}
        >
          {caption}
        </caption>
        <thead className={stickyHeader ? styles.stickyHead : undefined}>
          <tr>
            {columns.map((column) => (
              <th
                key={column.key}
                scope="col"
                className={column.align === "end" ? styles.alignEnd : undefined}
              >
                {column.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td className={styles.empty} colSpan={Math.max(columns.length, 1)}>
                {emptyMessage}
              </td>
            </tr>
          ) : (
            rows.map((row) => {
              const key = rowKey(row);
              const selected = selectedRowKey !== undefined && selectedRowKey === key;
              return (
                <tr
                  key={key}
                  className={selected ? styles.selected : undefined}
                  aria-current={selected ? "true" : undefined}
                >
                  {columns.map((column, index) => {
                    const className = [
                      column.mono === true ? styles.mono : "",
                      column.align === "end" ? styles.alignEnd : "",
                    ]
                      .filter((name) => name !== "")
                      .join(" ");
                    return index === rowHeaderIndex ? (
                      <th key={column.key} scope="row" className={className || undefined}>
                        {column.cell(row)}
                      </th>
                    ) : (
                      <td key={column.key} className={className || undefined}>
                        {column.cell(row)}
                      </td>
                    );
                  })}
                </tr>
              );
            })
          )}
        </tbody>
      </table>
    </div>
  );
}
