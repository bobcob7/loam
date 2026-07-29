import { create } from "@bufbuild/protobuf";
import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { PageInfoSchema, PageSchema } from "../gen/loam/v1/common_pb";
import { defaultPageLimit, hasMoreResults, toPagerState, useOffsetPagination } from "./pagination";

const page = (limit: number, offset: number) => create(PageSchema, { limit, offset });
const pageInfo = (total: number) => create(PageInfoSchema, { total });

describe("toPagerState", () => {
  it("carries PageInfo.total, Page.limit and Page.offset straight through", () => {
    expect(toPagerState(page(25, 50), pageInfo(120))).toEqual({ total: 120, limit: 25, offset: 50 });
  });

  it("does not swap limit and offset", () => {
    // Both are plain uint32s at the wire, so a copy-paste that reads the
    // wrong field is a real risk the assertion above alone would not catch
    // if limit and offset happened to be equal in that case.
    const state = toPagerState(page(10, 30), pageInfo(90));
    expect(state.limit).toBe(10);
    expect(state.offset).toBe(30);
  });
});

describe("hasMoreResults", () => {
  it("is true when the offset plus what came back is short of the total", () => {
    // Page 1 of 120 at limit 25: 0 + 25 < 120.
    expect(hasMoreResults(page(25, 0), pageInfo(120), 25)).toBe(true);
  });

  it("is false once offset plus returned count reaches the total exactly", () => {
    // Last full page: offset 100, 20 returned, total 120 -> 100 + 20 == 120.
    expect(hasMoreResults(page(25, 100), pageInfo(120), 20)).toBe(false);
  });

  it("is false on a short final page that already accounts for every record", () => {
    // total=120, offset=100, a short page returns only 20 (< limit 25).
    expect(hasMoreResults(page(25, 100), pageInfo(120), 20)).toBe(false);
  });

  it("is false for an empty result set", () => {
    expect(hasMoreResults(page(25, 0), pageInfo(0), 0)).toBe(false);
  });
});

describe("useOffsetPagination", () => {
  it("starts at offset 0 with the requested limit", () => {
    const { result } = renderHook(() => useOffsetPagination(50));
    expect(result.current.page.offset).toBe(0);
    expect(result.current.page.limit).toBe(50);
  });

  it("defaults the limit to 25 when none is given", () => {
    // A literal, not `defaultPageLimit`: comparing the hook's output against
    // the very constant it reads from is vacuous -- it cannot fail no matter
    // what that constant is changed to, since both sides move together.
    const { result } = renderHook(() => useOffsetPagination());
    expect(result.current.page.limit).toBe(25);
    expect(defaultPageLimit).toBe(25);
  });

  it("updates the page's offset when setOffset is called", () => {
    const { result } = renderHook(() => useOffsetPagination(25));
    act(() => result.current.setOffset(50));
    expect(result.current.page.offset).toBe(50);
    // The limit a screen picked must survive an offset change untouched.
    expect(result.current.page.limit).toBe(25);
  });

  it("clamps a negative offset to zero rather than sending it to the server", () => {
    const { result } = renderHook(() => useOffsetPagination(25));
    act(() => result.current.setOffset(-10));
    expect(result.current.page.offset).toBe(0);
  });

  it("returns to offset 0 on reset", () => {
    const { result } = renderHook(() => useOffsetPagination(25));
    act(() => result.current.setOffset(75));
    act(() => result.current.reset());
    expect(result.current.page.offset).toBe(0);
  });

  it("builds a real Page message, not a plain object", () => {
    const { result } = renderHook(() => useOffsetPagination(25));
    expect(result.current.page.$typeName).toBe("loam.v1.Page");
  });
});
