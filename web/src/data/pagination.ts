import { create } from "@bufbuild/protobuf";
import { useCallback, useState } from "react";
import { PageSchema, type Page, type PageInfo } from "../gen/loam/v1/common_pb";

/**
 * Used when a screen has no reason to pick its own page size. Matches
 * `Pager`'s own assumption that `limit` is "a real limit, not 0" -- `Page`'s
 * wire default of 0 means "use the server default" and has no page count to
 * divide by (`src/components/Pager.tsx`).
 */
export const defaultPageLimit = 25;

/** The three numbers `<Pager>` needs, derived from one page of a List* call. */
export interface PagerState {
  readonly total: number;
  readonly limit: number;
  readonly offset: number;
}

/**
 * Derives `<Pager>`'s props from the request `Page` and the response
 * `PageInfo` it produced (proto/loam/v1/common.proto). A pure mapping, not
 * duplicated arithmetic: `Pager` already turns `{ total, limit, offset }`
 * into a page number and prev/next state, so this only adapts the schema's
 * two messages into that shape -- it never recomputes what `Pager` computes.
 * Keeping the request `page` and this call paired (rather than reading
 * `offset`/`limit` from wherever a screen kept them) is what keeps a screen's
 * query and its pager in agreement by construction.
 */
export function toPagerState(page: Page, pageInfo: PageInfo): PagerState {
  return { total: pageInfo.total, limit: page.limit, offset: page.offset };
}

/**
 * Whether records beyond this page remain, per the exact rule
 * `PageInfo.total`'s doc comment states (proto/loam/v1/common.proto ->
 * PageInfo: "the caller knows more remain when offset + returned-count <
 * total"). `returnedCount` is the response list's length, not derivable from
 * `Page`/`PageInfo` alone: a short final page returns fewer than `limit`
 * records, so `limit` cannot stand in for it.
 */
export function hasMoreResults(page: Page, pageInfo: PageInfo, returnedCount: number): boolean {
  return page.offset + returnedCount < pageInfo.total;
}

/** What a screen needs to drive one offset-paginated query. */
export interface OffsetPagination {
  /** The `Page` to send as the request's `page` field this render. */
  readonly page: Page;
  /** Set the offset directly -- the shape `Pager`'s `onOffsetChange` wants. */
  readonly setOffset: (offset: number) => void;
  /** Back to the first page, e.g. after a filter narrows the result set. */
  readonly reset: () => void;
}

/**
 * Manages the offset half of offset pagination as React state; `limit` is
 * fixed for the screen's lifetime (docs/web-frontend-spec.md -> Pagination:
 * "offset paging with a ... pager driven by PageInfo.total"). `page` is a
 * real `Page` message built with `create()`, ready to spread straight into a
 * List* request's `page` field -- never a plain `{ limit, offset }` literal
 * (CLAUDE.md -> Go standards' TS analogue, docs/web-frontend-spec.md's
 * "Messages are built via create(Schema, {...})").
 */
export function useOffsetPagination(limit: number = defaultPageLimit): OffsetPagination {
  const [offset, setOffsetState] = useState(0);
  const setOffset = useCallback((next: number) => setOffsetState(Math.max(0, next)), []);
  const reset = useCallback(() => setOffsetState(0), []);
  const page = create(PageSchema, { limit, offset });
  return { page, setOffset, reset };
}
