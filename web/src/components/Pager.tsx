import type { ReactElement } from "react";
import styles from "./Pager.module.css";

export interface PagerProps {
  /** `PageInfo.total` from the list response: records across all pages. */
  readonly total: number;
  /** The `Page.limit` the request was made with. Must be a real limit, not 0. */
  readonly limit: number;
  /** The `Page.offset` the request was made with. */
  readonly offset: number;
  /** Called with the offset to fetch. The screen re-queries; Pager holds no state. */
  readonly onOffsetChange: (offset: number) => void;
  /** Plural noun for the records, e.g. "proposals". Used in the visible summary. */
  readonly itemNoun?: string;
}

/**
 * Pager renders prev/next controls for the schema's offset pagination
 * (`Page` / `PageInfo`, proto/loam/v1/common.proto).
 *
 * It is stateless: page position is derived from the `limit`/`offset` the
 * caller queried with and the `total` the server returned, so it cannot
 * disagree with the data on screen.
 *
 * ACCESSIBILITY DECISIONS:
 *
 *   - A `<nav>` labelled "Pagination", so it is reachable as a landmark and
 *     distinguishable from the app's main navigation.
 *   - The buttons are named "Go to page N", not "Previous"/"Next". A screen
 *     reader user listing the controls on the page hears where each one
 *     goes; "Next" alone tells them nothing about position.
 *   - The summary is a `role="status"` live region, so paging -- which
 *     changes the table above without moving focus -- is announced.
 *   - Native `disabled`, not `aria-disabled`, at the ends of the range. The
 *     known cost is that clicking to the last page drops focus to the body
 *     as Next becomes disabled; the compensation is the live region
 *     announcing the new position. Native `disabled` is unambiguous to every
 *     assistive technology, which `aria-disabled` on a still-clickable
 *     button is not.
 *
 * Renders nothing when the whole result set fits on one page: controls that
 * can never do anything are noise, and the screen shows its own count.
 */
export function Pager({
  total,
  limit,
  offset,
  onOffsetChange,
  itemNoun = "results",
}: PagerProps): ReactElement | null {
  if (limit <= 0 || total <= limit) return null;
  const pageCount = Math.ceil(total / limit);
  // Clamped: an offset past the end (a record deleted under us, a hand-typed
  // query string) must not render "Page 7 of 5".
  const page = Math.min(Math.floor(offset / limit) + 1, pageCount);
  const firstShown = (page - 1) * limit + 1;
  const lastShown = Math.min(page * limit, total);
  const hasPrevious = page > 1;
  const hasNext = page < pageCount;
  return (
    <nav className={styles.root} aria-label="Pagination">
      <p className={styles.summary} role="status">
        Page {page} of {pageCount} &middot; showing {firstShown}&ndash;{lastShown} of {total}{" "}
        {itemNoun}
      </p>
      <div className={styles.controls}>
        <button
          type="button"
          className={styles.button}
          disabled={!hasPrevious}
          // No destination when it is disabled, so no page number to name:
          // "Go to page 0" would be worse than the generic label.
          aria-label={hasPrevious ? `Go to page ${page - 1}` : "Previous page"}
          onClick={() => onOffsetChange((page - 2) * limit)}
        >
          Previous
        </button>
        <button
          type="button"
          className={styles.button}
          disabled={!hasNext}
          aria-label={hasNext ? `Go to page ${page + 1}` : "Next page"}
          onClick={() => onOffsetChange(page * limit)}
        >
          Next
        </button>
      </div>
    </nav>
  );
}
