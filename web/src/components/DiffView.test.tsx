import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { DiffView, diffFileElementId, parseUnifiedDiff } from "./DiffView";

/**
 * The parser gets tested directly, because its interesting failure mode --
 * silently attributing one file's lines to another -- is invisible in the
 * rendered output until someone reads a diff carefully. The component gets
 * tested through the DOM for the things a reviewer relies on: the index is
 * readable without expanding, and each section opens on its own.
 */

const twoFiles = [
  "diff --git a/src/a.ts b/src/a.ts",
  "index 1111111..2222222 100644",
  "--- a/src/a.ts",
  "+++ b/src/a.ts",
  "@@ -1,3 +1,4 @@",
  " const a = 1;",
  "-const b = 2;",
  "+const b = 3;",
  "+const c = 4;",
  " export { a };",
  "diff --git a/docs/notes.md b/docs/notes.md",
  "index 3333333..4444444 100644",
  "--- a/docs/notes.md",
  "+++ b/docs/notes.md",
  "@@ -1,2 +1,2 @@",
  "-old note",
  "+new note",
  " end",
].join("\n");

/** Every line must land in exactly one bucket -- nothing dropped, nothing duplicated. */
const rejoin = (diff: string): string => {
  const parsed = parseUnifiedDiff(diff);
  return [...parsed.preamble, ...parsed.files.flatMap((file) => file.text.split("\n"))].join("\n");
};

describe("parseUnifiedDiff", () => {
  it("splits a two-file diff into two sections with their paths", () => {
    const { files } = parseUnifiedDiff(twoFiles);
    expect(files.map((file) => file.path)).toEqual(["src/a.ts", "docs/notes.md"]);
  });

  it("counts added and removed lines per file, excluding the +++/--- headers", () => {
    const { files } = parseUnifiedDiff(twoFiles);
    // src/a.ts: one "-" and two "+" inside the hunk. The "--- a/src/a.ts" and
    // "+++ b/src/a.ts" header lines start with the same characters and must
    // not be counted; this assertion is the one that catches that.
    expect(files[0]).toMatchObject({ added: 2, removed: 1 });
    expect(files[1]).toMatchObject({ added: 1, removed: 1 });
  });

  it("keeps each file's section verbatim", () => {
    const { files } = parseUnifiedDiff(twoFiles);
    expect(files[0]?.text).toBe(
      [
        "diff --git a/src/a.ts b/src/a.ts",
        "index 1111111..2222222 100644",
        "--- a/src/a.ts",
        "+++ b/src/a.ts",
        "@@ -1,3 +1,4 @@",
        " const a = 1;",
        "-const b = 2;",
        "+const b = 3;",
        "+const c = 4;",
        " export { a };",
      ].join("\n"),
    );
  });

  it("does not split on a 'diff --git' line that is a file's CONTENT", () => {
    // A proposal that edits a document quoting a diff -- which this repo's own
    // docs do. Bead .2's criterion 4 is this case. The quoted header arrives
    // prefixed (`+diff --git ...`), so what is asserted is that the prefix is
    // respected and the section is not cut in two.
    const diff = [
      "diff --git a/docs/git-spec.md b/docs/git-spec.md",
      "--- a/docs/git-spec.md",
      "+++ b/docs/git-spec.md",
      "@@ -1,2 +1,6 @@",
      " Example output:",
      "+",
      "+```",
      "+diff --git a/src/x.ts b/src/x.ts",
      "+--- a/src/x.ts",
      "+++++ b/src/x.ts",
      " end",
    ].join("\n");
    const { files } = parseUnifiedDiff(diff);
    expect(files).toHaveLength(1);
    expect(files[0]?.path).toBe("docs/git-spec.md");
    expect(files[0]?.text).toContain("+diff --git a/src/x.ts b/src/x.ts");
  });

  it("starts the next file even when a blank line separates the two sections", () => {
    // Not a claim about header-shaped content: this hunk declares +1,3 and
    // `+a/+b/+c` consume it, so the second header arrives with hunk mode
    // already exited. What it pins is that the blank line between sections
    // stays with the first file rather than ending it, and that a /dev/null
    // pre-image still names the file from the `+++` side.
    const diff = [
      "diff --git a/patch.txt b/patch.txt",
      "--- /dev/null",
      "+++ b/patch.txt",
      "@@ -0,0 +1,3 @@",
      "+a",
      "+b",
      "+c",
      "",
      "diff --git a/second.txt b/second.txt",
      "--- /dev/null",
      "+++ b/second.txt",
      "@@ -0,0 +1,1 @@",
      "+x",
    ].join("\n");
    const { files } = parseUnifiedDiff(diff);
    expect(files.map((file) => file.path)).toEqual(["patch.txt", "second.txt"]);
    expect(files[0]?.text).toContain("\n");
    expect(files[0]).toMatchObject({ added: 3, removed: 0 });
  });

  it("counts only hunk-body lines, not the --- and +++ header lines", () => {
    // This is one of the two things the counters actually buy (DiffView.tsx).
    // A parser that counts by first character alone reports 2 added and 2
    // removed here, because `--- a/x.ts` and `+++ b/x.ts` start with the same
    // characters a body line does.
    const diff = "--- a/x.ts\n+++ b/x.ts\n@@ -1 +1 @@\n-old\n+new";
    expect(parseUnifiedDiff(diff).files[0]).toMatchObject({ added: 1, removed: 1 });
  });

  it("treats a bare empty line inside a hunk as a context line", () => {
    // NOT what git emits -- git writes " " for a blank context line. This
    // covers the trailing element `split("\n")` yields on a newline-terminated
    // diff, and any transport that strips trailing whitespace in flight. A
    // parser that demands a leading space loses the rest of the hunk to either.
    const diff = [
      "diff --git a/a.txt b/a.txt",
      "--- a/a.txt",
      "+++ b/a.txt",
      "@@ -1,3 +1,3 @@",
      " one",
      "",
      "-two",
      "+three",
    ].join("\n");
    const { files } = parseUnifiedDiff(diff);
    expect(files).toHaveLength(1);
    expect(files[0]).toMatchObject({ added: 1, removed: 1 });
  });

  it("handles a diff with no 'diff --git' headers at all", () => {
    // What POSIX `diff -u` produces, and what this screen's own test fixture
    // has always used. Not what the server sends: `git diff --no-ext-diff`
    // (internal/gitdiff/diff.go:243) always emits `diff --git`.
    const diff = "--- a/src/index.ts\n+++ b/src/index.ts\n@@ -1 +1 @@\n-old\n+new\n";
    const { files } = parseUnifiedDiff(diff);
    expect(files).toHaveLength(1);
    expect(files[0]?.path).toBe("src/index.ts");
    expect(files[0]).toMatchObject({ added: 1, removed: 1 });
  });

  it("names a deleted file by its pre-image path, since the post-image is /dev/null", () => {
    const diff = [
      "diff --git a/gone.ts b/gone.ts",
      "--- a/gone.ts",
      "+++ /dev/null",
      "@@ -1 +0,0 @@",
      "-was here",
    ].join("\n");
    expect(parseUnifiedDiff(diff).files[0]?.path).toBe("gone.ts");
  });

  it("reads a hunk header with omitted counts, which mean one line", () => {
    const diff = "--- a/x.ts\n+++ b/x.ts\n@@ -1 +1 @@\n-a\n+b";
    expect(parseUnifiedDiff(diff).files[0]).toMatchObject({ added: 1, removed: 1 });
  });

  it("does not count a 'no newline at end of file' marker against either side", () => {
    const diff = ["--- a/x.ts", "+++ b/x.ts", "@@ -1 +1 @@", "-a", "\\ No newline at end of file", "+b"].join("\n");
    expect(parseUnifiedDiff(diff).files[0]).toMatchObject({ added: 1, removed: 1 });
  });

  it("puts a binary-file section in its own file entry", () => {
    const diff = [
      "diff --git a/logo.png b/logo.png",
      "index 1111111..2222222 100644",
      "Binary files a/logo.png and b/logo.png differ",
    ].join("\n");
    const { files } = parseUnifiedDiff(diff);
    expect(files).toHaveLength(1);
    expect(files[0]).toMatchObject({ path: "logo.png", added: 0, removed: 0 });
  });

  it("yields no files for an empty diff", () => {
    expect(parseUnifiedDiff("")).toEqual({ preamble: [""], files: [] });
  });

  it.each([
    ["a two-file diff", twoFiles],
    ["a single-file diff", "--- a/x.ts\n+++ b/x.ts\n@@ -1 +1 @@\n-a\n+b\n"],
    ["content that looks like headers", "diff --git a/p b/p\n--- /dev/null\n+++ b/p\n@@ -0,0 +1,2 @@\n+diff --git a/q b/q\n+@@ -1 +1 @@"],
    ["something that is not a diff at all", "fatal: bad revision"],
  ])("loses no line when reassembling %s", (_name, diff) => {
    expect(rejoin(diff)).toBe(diff);
  });
});

describe("DiffView", () => {
  it("lists every changed file with its counts before anything is expanded", async () => {
    render(<DiffView diff={twoFiles} />);
    const index = screen.getByRole("navigation", { name: "Files changed" });
    const entries = within(index).getAllByRole("button");
    expect(entries).toHaveLength(2);
    expect(entries[0]).toHaveTextContent("src/a.ts");
    expect(entries[0]).toHaveTextContent("+2");
    expect(entries[0]).toHaveTextContent("−1");
    // And the whole-change summary line.
    expect(screen.getByText(/2 files changed/)).toBeInTheDocument();
  });

  it.each([
    ["a two-file diff", twoFiles],
    ["a one-line, single-file diff", "--- a/x.ts\n+++ b/x.ts\n@@ -1 +1 @@\n-a\n+b"],
  ])("starts every file collapsed in %s", (_name, diff) => {
    // The recorded decision (see DiffView.tsx): no size threshold, no
    // exceptions -- not even for the smallest possible diff, so the page's
    // height is a function of the file count alone.
    const { container } = render(<DiffView diff={diff} />);
    const sections = [...container.querySelectorAll("details")];
    expect(sections.length).toBeGreaterThan(0);
    for (const details of sections) expect(details.open).toBe(false);
  });

  it("expands one file without expanding the other", async () => {
    const user = userEvent.setup();
    const { container } = render(<DiffView diff={twoFiles} />);
    const sections = [...container.querySelectorAll("details")];
    expect(sections).toHaveLength(2);
    await user.click(within(sections[0] as HTMLElement).getByText("src/a.ts"));
    expect(sections[0]?.open).toBe(true);
    expect(sections[1]?.open).toBe(false);
  });

  it("expands and collapses every file from the toolbar", async () => {
    const user = userEvent.setup();
    const { container } = render(<DiffView diff={twoFiles} />);
    await user.click(screen.getByRole("button", { name: "Expand all" }));
    for (const details of container.querySelectorAll("details")) {
      expect(details.open).toBe(true);
    }
    await user.click(screen.getByRole("button", { name: "Collapse all" }));
    for (const details of container.querySelectorAll("details")) {
      expect(details.open).toBe(false);
    }
  });

  it("expands the matching file when its index entry is chosen", async () => {
    const user = userEvent.setup();
    const { container } = render(<DiffView diff={twoFiles} />);
    const index = screen.getByRole("navigation", { name: "Files changed" });
    await user.click(within(index).getAllByRole("button")[1] as HTMLElement);
    const sections = [...container.querySelectorAll("details")];
    expect(sections[0]?.open).toBe(false);
    expect(sections[1]?.open).toBe(true);
  });

  it("gives each file section the id a comment anchor would compute", () => {
    // Not used yet; it is what keeps linking a thread's file:line anchor to
    // its hunk possible later without guessing at the id format.
    const { container } = render(<DiffView diff={twoFiles} />);
    expect(container.querySelector(`#${CSS.escape(diffFileElementId("src/a.ts"))}`)).not.toBeNull();
  });

  it("keeps a collapsed file's content in the DOM so find-in-page can reach it", () => {
    // The counterpart of the no-per-line-rendering decision: one <pre> per
    // file is cheap enough to leave mounted.
    const { container } = render(<DiffView diff={twoFiles} />);
    expect(container.textContent).toContain("const c = 4;");
  });

  it("renders one <pre> per file and no per-line elements", () => {
    const { container } = render(<DiffView diff={twoFiles} />);
    const bodies = container.querySelectorAll("details > pre");
    expect(bodies).toHaveLength(2);
    for (const body of bodies) expect(body.children).toHaveLength(0);
  });

  it("says so plainly when the diff is empty", () => {
    render(<DiffView diff="" />);
    expect(screen.getByText("This proposal changes no files.")).toBeInTheDocument();
  });

  it("renders an unparseable payload rather than swallowing it", () => {
    render(<DiffView diff="fatal: bad revision 'wb-nope'" />);
    expect(screen.getByText(/fatal: bad revision/)).toBeInTheDocument();
  });

  it("renders a single-file diff as one section", () => {
    const { container } = render(
      <DiffView diff={"--- a/x.ts\n+++ b/x.ts\n@@ -1 +1 @@\n-a\n+b\n"} />,
    );
    expect(container.querySelectorAll("details")).toHaveLength(1);
    expect(screen.getByText("1 file changed", { exact: false })).toBeInTheDocument();
  });
});
