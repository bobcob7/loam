import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import type { Comment, Thread } from "../gen/loam/v1/common_pb";
import { ThreadList, anchorLabel, groupThreadsByAnchor, roundRuns } from "./ThreadList";

/**
 * The derivations are tested as functions, because the claim they make --
 * "these two things are related" -- is exactly the kind that looks right on
 * screen while being wrong. The component is tested for the two visual claims
 * the bead asks for: a later-round reply is distinguishable at a glance, and
 * nothing on screen asserts a thread-to-thread relationship.
 *
 * The generated message types carry `$typeName` and a `$unknown` slot; these
 * fixtures build the shape the component reads and cast, rather than pulling
 * in `create()` and a schema for data that never round-trips through the wire.
 */

const comment = (author: string, round: number, body = "ok"): Comment =>
  ({ author, round, body }) as Comment;

const thread = (overrides: Partial<Thread> & Pick<Thread, "id">): Thread =>
  ({
    resolved: false,
    round: 1,
    comments: [comment("reviewer-a-1-reviewer", 1)],
    ...overrides,
  }) as Thread;

const anchored = (id: string, file: string, line: number | undefined, rest: Partial<Thread> = {}) =>
  thread({ id, anchor: { file, line }, ...rest } as Partial<Thread> & Pick<Thread, "id">);

describe("anchorLabel", () => {
  it("names an unanchored thread rather than showing an empty location", () => {
    expect(anchorLabel(thread({ id: "t1" }))).toBe("General comment");
  });

  it("shows file:line for a line anchor", () => {
    expect(anchorLabel(anchored("t1", "src/a.ts", 42))).toBe("src/a.ts:42");
  });

  it("shows the bare file for a whole-file anchor", () => {
    expect(anchorLabel(anchored("t1", "src/a.ts", undefined))).toBe("src/a.ts");
  });
});

describe("groupThreadsByAnchor", () => {
  it("puts every thread on one file into one group", () => {
    const groups = groupThreadsByAnchor([
      anchored("t1", "src/a.ts", 10),
      anchored("t2", "src/b.ts", 5),
      anchored("t3", "src/a.ts", 20),
    ]);
    expect(groups.map((group) => group.file)).toEqual(["src/a.ts", "src/b.ts"]);
    expect(groups[0]?.threads.map((t) => t.id)).toEqual(["t1", "t3"]);
  });

  it("orders threads within a file by anchor line", () => {
    const groups = groupThreadsByAnchor([
      anchored("late", "src/a.ts", 90),
      anchored("early", "src/a.ts", 4),
    ]);
    expect(groups[0]?.threads.map((t) => t.id)).toEqual(["early", "late"]);
  });

  it("puts a whole-file anchor before any line anchor on the same file", () => {
    const groups = groupThreadsByAnchor([
      anchored("line", "src/a.ts", 1),
      anchored("file", "src/a.ts", undefined),
    ]);
    expect(groups[0]?.threads.map((t) => t.id)).toEqual(["file", "line"]);
  });

  it("leaves threads on the same line in server order", () => {
    // Two reviewers remarking on one line are not a conversation; reordering
    // them would invent a sequence.
    const groups = groupThreadsByAnchor([
      anchored("second-sent", "src/a.ts", 7),
      anchored("first-sent", "src/a.ts", 7),
    ]);
    expect(groups[0]?.threads.map((t) => t.id)).toEqual(["second-sent", "first-sent"]);
  });

  it("gathers unanchored threads into one group placed first", () => {
    const groups = groupThreadsByAnchor([
      anchored("t1", "src/a.ts", 1),
      thread({ id: "general-a" }),
      thread({ id: "general-b" }),
    ]);
    expect(groups[0]?.file).toBeUndefined();
    expect(groups[0]?.threads.map((t) => t.id)).toEqual(["general-a", "general-b"]);
  });

  it("keeps files in the order the server sent them, not alphabetical order", () => {
    // Alphabetising would be this component inventing an ordering the data
    // does not have.
    const groups = groupThreadsByAnchor([
      anchored("t1", "z.ts", 1),
      anchored("t2", "a.ts", 1),
    ]);
    expect(groups.map((group) => group.file)).toEqual(["z.ts", "a.ts"]);
  });

  it("yields nothing for no threads", () => {
    expect(groupThreadsByAnchor([])).toEqual([]);
  });
});

describe("roundRuns", () => {
  it("returns one run for a thread that never left its round", () => {
    expect(roundRuns([comment("a", 1), comment("b", 1)])).toEqual([
      { round: 1, comments: [comment("a", 1), comment("b", 1)] },
    ]);
  });

  it("splits at each round change", () => {
    const runs = roundRuns([comment("a", 1), comment("b", 3), comment("c", 3)]);
    expect(runs.map((run) => run.round)).toEqual([1, 3]);
    expect(runs[1]?.comments).toHaveLength(2);
  });

  it("does not merge non-adjacent runs of the same round", () => {
    // Merging would reorder the conversation into something the server never
    // sent.
    expect(roundRuns([comment("a", 1), comment("b", 2), comment("c", 1)]).map((r) => r.round)).toEqual(
      [1, 2, 1],
    );
  });

  it("returns nothing for a thread with no comments", () => {
    expect(roundRuns([])).toEqual([]);
  });
});

describe("ThreadList", () => {
  it("shows the round a thread was raised in", () => {
    render(<ThreadList threads={[thread({ id: "t1", round: 2 })]} />);
    expect(screen.getByText("Raised in round 2")).toBeInTheDocument();
  });

  it("marks a reply that landed in a later round than the thread was raised in", () => {
    // The single most useful derivation: proto/loam/v1/common.proto:76-79 says
    // a reply can land in a LATER round, and the page used to discard it.
    render(
      <ThreadList
        threads={[
          thread({
            id: "t1",
            round: 1,
            comments: [comment("author-1-author", 1), comment("reviewer-1-reviewer", 3)],
          }),
        ]}
      />,
    );
    expect(screen.getByText("Round 1")).toBeInTheDocument();
    expect(screen.getByText("Round 3")).toBeInTheDocument();
    // Colour is not the only signal.
    expect(screen.getByText("after the round this thread was raised in")).toBeInTheDocument();
  });

  it("does not mark a thread whose comments all sit in its own round", () => {
    render(
      <ThreadList
        threads={[
          thread({ id: "t1", round: 2, comments: [comment("a", 2), comment("b", 2)] }),
        ]}
      />,
    );
    expect(screen.queryByText("after the round this thread was raised in")).toBeNull();
    // One label for the run, not one per comment.
    expect(screen.getAllByText("Round 2")).toHaveLength(1);
  });

  it("renders a thread with a single comment", () => {
    render(<ThreadList threads={[thread({ id: "t1", comments: [comment("solo-1-reviewer", 1)] })]} />);
    expect(screen.getByText("solo-1-reviewer")).toBeInTheDocument();
  });

  it("renders a thread spanning three rounds as three labelled runs", () => {
    render(
      <ThreadList
        threads={[
          thread({
            id: "t1",
            round: 1,
            comments: [comment("a", 1), comment("b", 2), comment("c", 4)],
          }),
        ]}
      />,
    );
    for (const label of ["Round 1", "Round 2", "Round 4"]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    expect(screen.getAllByText("after the round this thread was raised in")).toHaveLength(2);
  });

  it("renders a thread with no anchor under its own heading", () => {
    render(<ThreadList threads={[thread({ id: "t1" })]} />);
    expect(screen.getByRole("heading", { name: "Not anchored to a file" })).toBeInTheDocument();
    expect(screen.getByText("General comment")).toBeInTheDocument();
  });

  it("heads each anchor group with the file path alone", () => {
    render(<ThreadList threads={[anchored("t1", "src/a.ts", 3)]} />);
    expect(screen.getByRole("heading", { name: "src/a.ts" })).toBeInTheDocument();
  });

  it("starts a resolved thread collapsed and an unresolved one open", () => {
    const { container } = render(
      <ThreadList
        threads={[thread({ id: "open", resolved: false }), thread({ id: "done", resolved: true })]}
      />,
    );
    const sections = [...container.querySelectorAll("details")];
    expect(sections[0]?.open).toBe(true);
    expect(sections[1]?.open).toBe(false);
  });

  it("lets a resolved thread be opened, and keeps it open across a re-render", async () => {
    // The page polls; a refetch must not slam shut a thread the admin opened.
    const user = userEvent.setup();
    const threads = [thread({ id: "done", resolved: true })];
    const { container, rerender } = render(<ThreadList threads={threads} />);
    await user.click(screen.getByText("General comment"));
    expect(container.querySelector("details")?.open).toBe(true);
    rerender(<ThreadList threads={[...threads]} />);
    expect(container.querySelector("details")?.open).toBe(true);
  });

  it("badges a resolved thread in words, not only in colour", () => {
    render(<ThreadList threads={[thread({ id: "done", resolved: true })]} />);
    expect(screen.getByText("Resolved")).toBeInTheDocument();
  });

  it("renders comment bodies through the shared markdown renderer", () => {
    render(
      <ThreadList
        threads={[
          thread({ id: "t1", comments: [comment("a", 1, "### Blocking\n\n- one\n- two")] }),
        ]}
      />,
    );
    expect(screen.getByRole("heading", { name: "Blocking" })).toBeInTheDocument();
  });

  it("renders a hostile comment body inert", () => {
    const { container } = render(
      <ThreadList
        threads={[
          thread({
            id: "t1",
            comments: [comment("a", 1, "<script>window.x=1</script>\n\n[go](javascript:alert(1))")],
          }),
        ]}
      />,
    );
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector("a")?.getAttribute("href")).toBe("");
  });

  it("draws no relationship between two threads beyond the anchor they share", () => {
    // `Thread` carries no parent, reply-to or continuation field, so nothing
    // here may nest one thread inside another or number them as a sequence.
    // Each thread is a sibling <li> under its group; a thread's <details>
    // never contains another thread's.
    const { container } = render(
      <ThreadList threads={[anchored("t1", "src/a.ts", 1), anchored("t2", "src/a.ts", 2)]} />,
    );
    const sections = [...container.querySelectorAll("details")];
    expect(sections).toHaveLength(2);
    for (const section of sections) {
      expect(section.querySelector("details")).toBeNull();
    }
    const group = screen.getByRole("heading", { name: "src/a.ts" }).closest("section");
    expect(group).not.toBeNull();
    // Two thread items, flat -- no nesting level implying "t2 replies to t1".
    const list = group?.querySelector("ul");
    expect(list?.children).toHaveLength(2);
    for (const item of [...(list?.children ?? [])]) {
      expect(within(item as HTMLElement).queryAllByText(/^Thread \d/)).toHaveLength(0);
    }
  });
});
