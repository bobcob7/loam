import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Markdown } from "./Markdown";

/**
 * Two kinds of claim here, and only one of them is about formatting.
 *
 * The formatting suite asserts that the block constructs agents actually
 * write survive the renderer -- headings, lists, fenced code, tables. Those
 * are cheap and they would catch a plugin being dropped.
 *
 * The security suite is the reason this component exists as a component. Its
 * assertions are all made against the RENDERED DOM: what elements exist, what
 * `href` values survive. None of them reads the configuration in Markdown.tsx,
 * because a correct `urlTransform` that is never passed to `ReactMarkdown`,
 * or a `rehype-raw` added "to make an escape work", both leave the
 * configuration looking right and the page exploitable. See
 * ProposalDetail.test.tsx for the same assertions made at the comment-body
 * call site, which is where the untrusted content actually arrives.
 */

describe("Markdown: nothing to render", () => {
  it("renders nothing for an empty string", () => {
    const { container } = render(<Markdown source="" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing for a whitespace-only body", () => {
    // A body of "\n\n" is markdown for "no blocks"; an empty bordered box in
    // its place reads as a comment whose content failed to load.
    const { container } = render(<Markdown source={"  \n\n\t "} />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("Markdown: formatting", () => {
  it("renders an ATX heading as a heading element", () => {
    render(<Markdown source="## What changed" />);
    expect(screen.getByRole("heading", { name: "What changed" })).toBeInTheDocument();
  });

  it("renders a bulleted list as list items, not as one paragraph", () => {
    render(<Markdown source={"- first\n- second\n- third"} />);
    expect(screen.getAllByRole("listitem")).toHaveLength(3);
  });

  it("renders an ordered list", () => {
    const { container } = render(<Markdown source={"1. one\n2. two"} />);
    expect(container.querySelectorAll("ol > li")).toHaveLength(2);
  });

  it("renders a fenced block as pre > code, unhighlighted", () => {
    // "Unhighlighted" is the assertion that matters: a highlighter would
    // shred the text into per-token <span>s, so a single text node whose
    // content is the whole block proves none was added (loam-ba6a).
    const { container } = render(<Markdown source={"```sh\ntask web:test\n```"} />);
    const code = container.querySelector("pre > code");
    expect(code).not.toBeNull();
    expect(code?.textContent).toBe("task web:test\n");
    expect(code?.children).toHaveLength(0);
  });

  it("renders inline code as a code element", () => {
    const { container } = render(<Markdown source="call `safeUrl` first" />);
    expect(container.querySelector("p > code")?.textContent).toBe("safeUrl");
  });

  it("renders a GFM table, which core markdown alone would not", () => {
    render(<Markdown source={"| File | Lines |\n| --- | --- |\n| a.ts | 12 |"} />);
    const table = screen.getByRole("table");
    expect(table).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "File" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "a.ts" })).toBeInTheDocument();
  });

  it("renders a GFM strikethrough", () => {
    const { container } = render(<Markdown source="~~dropped~~" />);
    expect(container.querySelector("del")?.textContent).toBe("dropped");
  });

  it("renders an http link with its text and destination", () => {
    render(<Markdown source="see [the spec](https://example.com/spec)" />);
    expect(screen.getByRole("link", { name: "the spec" })).toHaveAttribute(
      "href",
      "https://example.com/spec",
    );
  });

  it("gives every link rel=noreferrer noopener", () => {
    render(<Markdown source="[x](https://example.com)" />);
    expect(screen.getByRole("link", { name: "x" })).toHaveAttribute(
      "rel",
      "noreferrer noopener",
    );
  });
});

describe("Markdown: hostile input renders inert", () => {
  it("creates no script node from a script tag in the source", () => {
    const { container } = render(
      <Markdown source={"before\n\n<script>window.stolen = 1</script>\n\nafter"} />,
    );
    expect(container.querySelector("script")).toBeNull();
    // The surrounding prose still renders -- the tag is dropped, not the body.
    expect(screen.getByText(/before/)).toBeInTheDocument();
    expect(screen.getByText(/after/)).toBeInTheDocument();
  });

  it("creates no img node, and therefore no onerror handler, from raw HTML", () => {
    const { container } = render(
      <Markdown source={'<img src="x" onerror="window.stolen = 1">'} />,
    );
    expect(container.querySelector("img")).toBeNull();
    // No element anywhere in the tree carries the handler as an ATTRIBUTE.
    // The characters are still present as text -- the tag renders as the
    // literal string it was -- and that is the correct outcome: inert text is
    // what "raw HTML passthrough is off" means.
    expect(container.querySelectorAll("[onerror]")).toHaveLength(0);
    expect(container.textContent).toContain('<img src="x" onerror=');
  });

  it("creates no iframe from raw HTML", () => {
    const { container } = render(
      <Markdown source={'<iframe src="https://evil.example/"></iframe>'} />,
    );
    expect(container.querySelector("iframe")).toBeNull();
  });

  it("strips a javascript: href, leaving the link inert", () => {
    const { container } = render(<Markdown source="[click me](javascript:alert(1))" />);
    const link = container.querySelector("a");
    expect(link).not.toBeNull();
    expect(link?.getAttribute("href")).toBe("");
    expect(container.innerHTML).not.toContain("javascript:");
  });

  it("strips a javascript: href written in mixed case", () => {
    // A prefix check against the literal string "javascript:" passes this
    // straight through; schemes are case-insensitive.
    const { container } = render(<Markdown source="[click](JaVaScRiPt:alert(1))" />);
    expect(container.querySelector("a")?.getAttribute("href")).toBe("");
  });

  it("strips a javascript: href obfuscated with character references", () => {
    // `&#106;` is "j" and `&#x3A;` is ":". Markdown decodes both in a link
    // destination, so what reaches the DOM would be a working javascript:
    // URL -- this is the realistic obfuscation, not a hand-typed scheme.
    const { container } = render(
      <Markdown source="[click](&#106;avascript&#x3A;alert&#40;1&#41;)" />,
    );
    expect(container.querySelector("a")?.getAttribute("href")).toBe("");
    expect(container.innerHTML).not.toContain("javascript");
  });

  it("strips a data: href", () => {
    const { container } = render(
      <Markdown source="[doc](data:text/html;base64,PHNjcmlwdD4pPC9zY3JpcHQ+)" />,
    );
    expect(container.querySelector("a")?.getAttribute("href")).toBe("");
    expect(container.innerHTML).not.toContain("data:text/html");
  });

  it("strips a vbscript: href, a scheme the allow-list never enumerated", () => {
    const { container } = render(<Markdown source="[x](vbscript:msgbox(1))" />);
    expect(container.querySelector("a")?.getAttribute("href")).toBe("");
  });

  it("strips a javascript: image source", () => {
    // The image element survives (with its alt text); its source does not.
    // React omits the attribute entirely rather than emitting src="", hence
    // the `?? ""` -- the claim is "nothing loadable", either shape satisfies it.
    const { container } = render(<Markdown source="![alt](javascript:alert(1))" />);
    const image = container.querySelector("img");
    expect(image).not.toBeNull();
    expect(image?.getAttribute("src") ?? "").toBe("");
    expect(container.innerHTML).not.toContain("javascript:");
  });

  it("keeps a mailto: link, which the allow-list permits", () => {
    render(<Markdown source="[mail](mailto:admin@example.com)" />);
    expect(screen.getByRole("link", { name: "mail" })).toHaveAttribute(
      "href",
      "mailto:admin@example.com",
    );
  });

  it("keeps a relative link, which carries no scheme to check", () => {
    render(<Markdown source="[proposals](/proposals)" />);
    expect(screen.getByRole("link", { name: "proposals" })).toHaveAttribute(
      "href",
      "/proposals",
    );
  });

  it("renders an HTML entity as text rather than as markup", () => {
    // `&lt;script&gt;` decodes to the STRING "<script>"; if that string ever
    // reaches innerHTML it becomes an element again.
    const { container } = render(<Markdown source={"&lt;script&gt;alert(1)&lt;/script&gt;"} />);
    expect(container.querySelector("script")).toBeNull();
    expect(screen.getByText("<script>alert(1)</script>")).toBeInTheDocument();
  });
});
