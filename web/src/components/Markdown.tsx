import type { ReactElement } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import styles from "./Markdown.module.css";

/**
 * The only URL schemes a link or image in agent-authored prose may carry.
 *
 * `javascript:` is the obvious attack; `data:` is the less obvious one (a
 * `data:text/html` document inherits the origin it was opened from in older
 * engines, and `data:image/svg+xml` carries script). Everything else --
 * `vbscript:`, `file:`, custom app schemes -- is denied by omission rather
 * than enumerated, so a scheme nobody has thought of yet is denied too.
 */
const safeProtocols: ReadonlySet<string> = new Set(["http:", "https:", "mailto:"]);

/**
 * Returns `url` unchanged when it is relative or carries an allowed scheme,
 * and `""` -- an inert, same-page href -- when it does not.
 *
 * The check is on the SCHEME, matched case-insensitively, not on a list of
 * bad prefixes: `JaVaScRiPt:` and `javascript&#x3A;` (which the markdown
 * layer decodes to `javascript:` before this ever sees it) both have to fail,
 * and `startsWith("javascript:")` catches neither. C0 controls and spaces are
 * removed before matching because a browser strips them before resolving a
 * URL, so `java\tscript:` would navigate as `javascript:` -- remark happens
 * to percent-encode such a character first, which makes the scheme
 * unparseable to a browser too, but that is remark's behaviour to change and
 * this is the layer that owns the guarantee.
 *
 * The value returned is the ORIGINAL, so a legitimate URL is never silently
 * rewritten -- this function only ever answers yes or no.
 */
function safeUrl(url: string): string {
  const collapsed = url.replace(/[\u0000-\u0020\u007f]/g, "");
  const scheme = /^([a-z][a-z0-9+.-]*):/i.exec(collapsed);
  if (scheme === null) return url;
  const protocol = scheme[1];
  if (protocol === undefined) return url;
  return safeProtocols.has(`${protocol.toLowerCase()}:`) ? url : "";
}

export interface MarkdownProps {
  /**
   * The markdown source. Agent-authored and untrusted -- see the component
   * doc comment. An empty or whitespace-only value renders nothing at all,
   * not an empty block.
   */
  readonly source: string;
  /**
   * Accessible label for the region, when the surrounding markup does not
   * already name it. Omitted for the proposal description, which sits
   * directly under the page's `<h1>`.
   */
  readonly ariaLabel?: string;
}

/**
 * Markdown is the app's ONE renderer for agent-authored prose: the proposal
 * description and every comment body go through it (loam-ba6a.1/.3). It is a
 * single component rather than a per-call-site choice so the security
 * properties below are asserted once and cannot drift apart.
 *
 * WHY react-markdown, given that web/package.json's runtime dependency list
 * is deliberately short. The alternative shape -- `marked`/`markdown-it`
 * producing an HTML string, sanitised with DOMPurify and injected through
 * `dangerouslySetInnerHTML` -- is not fewer dependencies (it is two: a parser
 * and a sanitiser) and it is a materially worse security posture, because the
 * safety of every render then depends on a sanitiser call being present at
 * the injection site. react-markdown never produces an HTML string at all:
 * it builds a React element tree, so raw HTML in the source is dropped by
 * construction rather than by configuration, and there is no
 * `dangerouslySetInnerHTML` anywhere in the path to forget. That is a
 * structural property, which is what justifies the bytes. `remark-gfm` is
 * added because agent prose in this project uses tables, task lists and
 * strikethrough routinely, and this repo's own bead text does.
 *
 * THE THREE SECURITY CLAIMS, each asserted on the rendered DOM in
 * Markdown.test.tsx and at the comment-body call site in
 * ProposalDetail.test.tsx -- never on this configuration, since configuration
 * can be right while the wiring is wrong:
 *
 * 1. Raw HTML never becomes markup. react-markdown drops `html` nodes unless
 *    `rehype-raw` is installed and passed as a rehype plugin. It is NOT
 *    installed and MUST NOT BE: adding it re-enables `<script>`, `<iframe>`
 *    and every event-handler attribute in text written by another agent.
 * 2. Link and image URLs are scheme-restricted by `urlTransform` (see
 *    `safeUrl`). react-markdown ships a default transform with a similar
 *    allow-list; this passes an explicit one rather than inheriting it, so a
 *    future upgrade that relaxes the default cannot relax this.
 * 3. Links carry `rel="noreferrer noopener"`. An untrusted link that reaches
 *    `window.opener` can navigate the admin console it was opened from, and
 *    the referrer would leak the proposal URL to whatever host the agent
 *    chose.
 * 4. Images are NOT auto-loaded. `![](https://evil.example/beacon.png)` in a
 *    comment body is a read receipt: it fires on page open with no
 *    interaction, handing the reader's IP, user-agent and a timestamp to
 *    whoever wrote the comment -- and a comment is written by a different
 *    agent to the branch's author. A `referrerPolicy` does not fix that; it
 *    removes the proposal URL from the request, but the REQUEST ITSELF is the
 *    leak. So `![alt](url)` renders as a link the reader can choose to open,
 *    and nothing is fetched until they do. The scheme allow-list still
 *    applies, so a denied source renders as inert text with no destination at
 *    all.
 *
 * No syntax highlighting, deliberately (loam-ba6a's notes): a fenced block
 * renders as a styled, unhighlighted `<pre><code>`. Highlighting is a much
 * larger dependency and its own decision.
 */
export function Markdown({ source, ariaLabel }: MarkdownProps): ReactElement | null {
  if (source.trim() === "") return null;
  return (
    <div className={styles.root} aria-label={ariaLabel}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        urlTransform={safeUrl}
        components={{
          a: ({ node: _node, ...props }) => (
            <a {...props} target="_blank" rel="noreferrer noopener" referrerPolicy="no-referrer" />
          ),
          // Deliberately NOT an <img>: see claim 4 in the doc comment above.
          img: ({ node: _node, src, alt, title }) => {
            const label = typeof alt === "string" && alt.trim() !== "" ? alt : "image";
            if (typeof src !== "string" || src === "") {
              return <span className={styles.blockedImage}>{label} (image source blocked)</span>;
            }
            return (
              <a
                className={styles.imageLink}
                href={src}
                title={title}
                target="_blank"
                rel="noreferrer noopener"
                referrerPolicy="no-referrer"
              >
                {label} (image — opens in a new tab)
              </a>
            );
          },
        }}
      >
        {source}
      </ReactMarkdown>
    </div>
  );
}
