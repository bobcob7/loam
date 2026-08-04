import type { ReactElement } from "react";
import { useState } from "react";
import styles from "./DiffView.module.css";

/** One file's section of a unified diff, verbatim, plus its line counts. */
export interface DiffFileSection {
  /** Display path: the post-image path, or the pre-image path for a deletion. */
  readonly path: string;
  readonly added: number;
  readonly removed: number;
  /** The section's lines exactly as they arrived, headers included. */
  readonly text: string;
}

export interface ParsedDiff {
  /**
   * Lines before the first file section. Empty for a well-formed diff; it
   * exists so that parsing is LOSSLESS -- `[...preamble, ...files.map(text)]`
   * rejoined reproduces the input, which is what makes it safe to render the
   * pieces instead of the whole string.
   */
  readonly preamble: readonly string[];
  readonly files: readonly DiffFileSection[];
}

interface MutableFile {
  headerPath: string;
  oldPath: string | undefined;
  newPath: string | undefined;
  sawHunk: boolean;
  added: number;
  removed: number;
  lines: string[];
}

/** `@@ -12,7 +12,9 @@`, with either count optional (absent means 1). */
const hunkHeaderPattern = /^@@ -\d+(?:,(\d+))? \+\d+(?:,(\d+))? @@/;

/** `a/src/x.ts`, `"b/has space.ts"`, `/dev/null`, or a path with a timestamp. */
function stripPathPrefix(raw: string): string {
  const beforeTab = raw.split("\t")[0] ?? raw;
  const unquoted =
    beforeTab.length > 1 && beforeTab.startsWith('"') && beforeTab.endsWith('"')
      ? beforeTab.slice(1, -1)
      : beforeTab;
  if (unquoted === "/dev/null") return unquoted;
  return unquoted.replace(/^[ab]\//, "");
}

/** The post-image path out of `diff --git a/<old> b/<new>`, best effort. */
function gitHeaderPath(line: string): string {
  const rest = line.slice("diff --git ".length);
  const split = rest.lastIndexOf(" b/");
  if (split === -1) return stripPathPrefix(rest);
  return stripPathPrefix(rest.slice(split + 1));
}

/**
 * Splits a unified diff into per-file sections.
 *
 * WHY THIS IS A STATE MACHINE AND NOT `diff.split(/^diff --git/m)`, stated
 * precisely, because the tempting version of this argument is wrong.
 *
 * The tempting version is "a file's content can contain a diff header". It
 * can -- this repo's docs quote diffs -- but inside a hunk those lines arrive
 * PREFIXED (`+diff --git ...`), so a line-anchored split already survives
 * them. That is not what the counters buy.
 *
 * What they actually buy is two things a split cannot do at all:
 *
 * 1. HEADERLESS DIFFS. A diff may carry no `diff --git` lines whatsoever --
 *    just a `--- `/`+++ ` pair, which is what POSIX `diff -u` produces (with
 *    a tab and a timestamp appended to each path, which is why
 *    `stripPathPrefix` splits on `\t`) and what this screen's own test
 *    fixture has always used. Splitting on `diff --git` yields one section
 *    containing everything. Note the scope: the server runs `git diff
 *    --no-ext-diff <target>...<wb>` (internal/gitdiff/diff.go:243), which
 *    ALWAYS emits `diff --git`, so a headerless diff never arrives from
 *    production traffic -- this reason defends fixtures and hand-written
 *    patches, not the live path.
 * 2. CORRECT COUNTS. `--- a/x` and `+++ b/x` start with `-` and `+`, so
 *    counting by first character alone reports one spurious removal and one
 *    spurious addition per file. Only knowing where the hunk BODY starts
 *    gets `added`/`removed` right, and the body's extent is exactly what the
 *    `@@` header declares.
 *
 * So the parser tracks how many old and new lines each `@@` hunk declared and
 * treats every line until those are consumed as body, whatever it looks like;
 * a header is only recognised outside a hunk body.
 *
 * THE LIMIT, stated so nobody reads more into the above than is there: if a
 * hunk's declared counts run out early -- a truncated or hand-edited patch --
 * the escape hatch below leaves hunk mode, and a following header-shaped line
 * WOULD start a new section. Git does not produce that, and it costs nothing
 * either way, because every input line is appended to exactly one bucket:
 * nothing is ever dropped, worst case a section is bigger than it should be.
 * DiffView.test.tsx asserts that round-trip on four shapes, including input
 * that is not a diff at all.
 */
export function parseUnifiedDiff(diff: string): ParsedDiff {
  const preamble: string[] = [];
  const files: MutableFile[] = [];
  let current: MutableFile | undefined;
  let oldLeft = 0;
  let newLeft = 0;

  const push = (line: string): void => {
    if (current === undefined) preamble.push(line);
    else current.lines.push(line);
  };
  const startFile = (line: string, headerPath: string): MutableFile => {
    const file: MutableFile = {
      headerPath,
      oldPath: undefined,
      newPath: undefined,
      sawHunk: false,
      added: 0,
      removed: 0,
      lines: [line],
    };
    files.push(file);
    current = file;
    oldLeft = 0;
    newLeft = 0;
    return file;
  };

  const lines = diff.split("\n");
  for (const [index, line] of lines.entries()) {
    if (oldLeft > 0 || newLeft > 0) {
      const marker = line.charAt(0);
      if (marker === "+") {
        newLeft -= 1;
        if (current !== undefined) current.added += 1;
        push(line);
        continue;
      }
      if (marker === "-") {
        oldLeft -= 1;
        if (current !== undefined) current.removed += 1;
        push(line);
        continue;
      }
      if (marker === " " || line === "") {
        // Git writes `" "` for a blank context line, not `""`. The `""` case
        // is still handled, but for different reasons than git's output: the
        // trailing element `split("\n")` yields on a newline-terminated diff,
        // and any transport that strips trailing whitespace in flight.
        oldLeft -= 1;
        newLeft -= 1;
        push(line);
        continue;
      }
      if (marker === "\\") {
        // "\ No newline at end of file" belongs to the preceding line and
        // counts against neither side.
        push(line);
        continue;
      }
      // Not a body line: the declared counts were wrong. Leave hunk mode and
      // let the header rules below have this line rather than swallowing the
      // rest of the diff.
      oldLeft = 0;
      newLeft = 0;
    }

    if (line.startsWith("diff --git ")) {
      startFile(line, gitHeaderPath(line));
      continue;
    }
    if (line.startsWith("--- ") && (lines[index + 1] ?? "").startsWith("+++ ")) {
      // A `--- `/`+++ ` pair starts a section only when there is no open
      // section to attach it to, or the open one already had its hunks --
      // otherwise this is the file header inside a `diff --git` section.
      if (current === undefined || current.sawHunk) startFile(line, "");
      else push(line);
      if (current !== undefined) current.oldPath = stripPathPrefix(line.slice(4));
      continue;
    }
    if (line.startsWith("+++ ") && current !== undefined) {
      current.newPath = stripPathPrefix(line.slice(4));
      push(line);
      continue;
    }
    const hunk = hunkHeaderPattern.exec(line);
    if (hunk !== null && current !== undefined) {
      current.sawHunk = true;
      oldLeft = hunk[1] === undefined ? 1 : Number(hunk[1]);
      newLeft = hunk[2] === undefined ? 1 : Number(hunk[2]);
      push(line);
      continue;
    }
    push(line);
  }

  return {
    preamble,
    files: files.map((file) => ({
      path: displayPath(file),
      added: file.added,
      removed: file.removed,
      text: file.lines.join("\n"),
    })),
  };
}

function displayPath(file: MutableFile): string {
  if (file.newPath !== undefined && file.newPath !== "/dev/null") return file.newPath;
  if (file.oldPath !== undefined && file.oldPath !== "/dev/null") return file.oldPath;
  if (file.headerPath !== "") return file.headerPath;
  return "(unknown file)";
}

/**
 * The DOM id of a file's diff section.
 *
 * Exported so a future change can link a thread's `file:line` anchor to the
 * hunk it refers to (loam-ba6a's notes leave that unfiled but ask that it not
 * be precluded) by COMPUTING the id rather than guessing at it.
 */
export function diffFileElementId(path: string): string {
  return `diff-file:${path}`;
}

export interface DiffViewProps {
  /** The whole unified diff as `GetWorkBranchDiff` returns it. */
  readonly diff: string;
}

/**
 * DiffView renders a unified diff as a files-changed index plus one
 * independently collapsible section per file (loam-ba6a.2).
 *
 * DEFAULT STATE: every file collapsed, with no size threshold and no
 * exceptions -- not even for a single-file diff.
 *
 * The alternative, "expand anything under N lines", needs an N that nobody
 * agrees on and makes the page's height a function of the diff's contents,
 * so a reviewer cannot learn where the Verdicts section is. Collapsed-always
 * makes the page height a function of the FILE COUNT alone, which is both
 * predictable and small, and it puts the question a reviewer actually asks
 * first -- what is the shape of this change -- above the fold with nothing to
 * scroll past. The cost of the uniform rule is one extra click when the diff
 * is one small file, and Expand all pays that for a diff of any size.
 *
 * NO PER-LINE COLOURING, and this is what keeps collapsed content cheap. Each
 * file's body is ONE `<pre>` holding one text node, so a 1705-line diff over
 * a dozen files is about two dozen elements rather than the ~1705 the
 * colour-each-line approach needs -- on a page that also polls four queries.
 * Because the cost is that low, the bodies stay in the DOM while collapsed
 * rather than being unmounted, which is what lets the browser's find-in-page
 * and Ctrl+F still reach them.
 *
 * NO SYNTAX HIGHLIGHTING, deliberately (loam-ba6a's notes).
 */
export function DiffView({ diff }: DiffViewProps): ReactElement {
  const { preamble, files } = parseUnifiedDiff(diff);
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());

  const setOpen = (path: string, open: boolean): void => {
    setExpanded((previous) => {
      if (previous.has(path) === open) return previous;
      const next = new Set(previous);
      if (open) next.add(path);
      else next.delete(path);
      return next;
    });
  };

  const preambleText = preamble.join("\n");
  if (files.length === 0) {
    return preambleText.trim() === "" ? (
      <p>This proposal changes no files.</p>
    ) : (
      // Not a diff we recognise -- show it rather than swallowing it.
      <pre className={styles.body}>{preambleText}</pre>
    );
  }

  const added = files.reduce((total, file) => total + file.added, 0);
  const removed = files.reduce((total, file) => total + file.removed, 0);

  return (
    <div className={styles.root}>
      <div className={styles.toolbar}>
        <p className={styles.total}>
          {files.length === 1 ? "1 file changed" : `${files.length} files changed`}
          {", "}
          <span className={styles.added}>+{added}</span>{" "}
          <span className={styles.removed}>−{removed}</span>
        </p>
        <div className={styles.toolbarActions}>
          <button
            type="button"
            className={styles.toolbarButton}
            onClick={() => setExpanded(new Set(files.map((file) => file.path)))}
          >
            Expand all
          </button>
          <button
            type="button"
            className={styles.toolbarButton}
            onClick={() => setExpanded(new Set())}
          >
            Collapse all
          </button>
        </div>
      </div>

      <nav aria-label="Files changed">
        <ul className={styles.index}>
          {files.map((file) => (
            <li key={file.path}>
              <button
                type="button"
                className={styles.indexEntry}
                onClick={() => {
                  setOpen(file.path, true);
                  scrollTo(diffFileElementId(file.path));
                }}
              >
                <span className={styles.path}>{file.path}</span>
                <span className={styles.counts}>
                  <span className={styles.added}>+{file.added}</span>{" "}
                  <span className={styles.removed}>−{file.removed}</span>
                </span>
              </button>
            </li>
          ))}
        </ul>
      </nav>

      {files.map((file) => (
        <details
          key={file.path}
          id={diffFileElementId(file.path)}
          className={styles.file}
          open={expanded.has(file.path)}
          onToggle={(event) => setOpen(file.path, event.currentTarget.open)}
        >
          <summary className={styles.fileSummary}>
            <span className={styles.path}>{file.path}</span>
            <span className={styles.counts}>
              <span className={styles.added}>+{file.added}</span>{" "}
              <span className={styles.removed}>−{file.removed}</span>
            </span>
          </summary>
          <pre className={styles.body}>{file.text}</pre>
        </details>
      ))}
    </div>
  );
}

/**
 * Brings a file's section into view after the index expanded it.
 *
 * Feature-detected rather than called directly: jsdom implements neither
 * `scrollIntoView` nor layout, so an unguarded call turns every test that
 * clicks the index into a TypeError about a method the real browser has.
 */
function scrollTo(elementId: string): void {
  const element = document.getElementById(elementId);
  if (element !== null && typeof element.scrollIntoView === "function") {
    element.scrollIntoView({ block: "start" });
  }
}
