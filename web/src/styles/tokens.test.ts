// @vitest-environment node
//
// This suite reads the stylesheets off disk and does arithmetic on them; it
// never renders. The project-wide jsdom environment (vite.config.ts) would
// serve `import.meta.url` as an http: URL, which fileURLToPath rejects.
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

/**
 * These four suites are the only tests worth writing for a styling
 * foundation, because they are the only claims about it that can be false.
 *
 * The deliverable is static CSS: asserting that tokens.css "contains
 * --color-bg" or that some declaration equals some hex would restate the
 * file rather than check it, and could not fail for any reason worth
 * knowing about. What CAN silently go wrong is (1) a colour pair that does
 * not actually meet WCAG, (2) a component referencing a token that does not
 * exist -- `var(--color-tex)` fails silently, rendering an inherited or
 * initial value with no console error and no build error, (3) a component
 * hard-coding a colour and drifting off the palette, and (4) the dark-only
 * decision quietly acquiring a light fork. Each is asserted below against
 * the real files on disk, so all four keep working as loam-nvb.5/.6 add
 * components without anyone editing this file.
 */

const stylesDir = fileURLToPath(new URL(".", import.meta.url));
const srcDir = join(stylesDir, "..");
const tokensCss = readFileSync(join(stylesDir, "tokens.css"), "utf8");
const resetCss = readFileSync(join(stylesDir, "reset.css"), "utf8");

const stripComments = (css: string): string => css.replace(/\/\*[\s\S]*?\*\//g, "");

/** Every `--name: value` declaration in tokens.css, comments removed. */
const parseTokens = (css: string): ReadonlyMap<string, string> => {
  const tokens = new Map<string, string>();
  for (const [, name, value] of stripComments(css).matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) {
    if (name !== undefined && value !== undefined) tokens.set(name, value.trim());
  }
  return tokens;
};

const tokens = parseTokens(tokensCss);

/** Every `*.module.css` under src/, as [relative path, contents]. */
const moduleStylesheets = (): ReadonlyArray<readonly [string, string]> =>
  readdirSync(srcDir, { recursive: true, encoding: "utf8" })
    .filter((entry) => entry.endsWith(".module.css"))
    .map((entry) => [entry, readFileSync(join(srcDir, entry), "utf8")] as const);

// --- WCAG 2.1 relative luminance and contrast ratio -----------------------
// Definitions: https://www.w3.org/TR/WCAG21/#dfn-relative-luminance and
// https://www.w3.org/TR/WCAG21/#dfn-contrast-ratio.

const channels = (hex: string): readonly [number, number, number] => {
  const digits = hex.replace("#", "");
  const [r, g, b] = [0, 2, 4].map((i) => parseInt(digits.slice(i, i + 2), 16) / 255);
  if (r === undefined || g === undefined || b === undefined || Number.isNaN(r * g * b)) {
    throw new Error(`not a six-digit hex colour: ${hex}`);
  }
  return [r, g, b];
};

const linearise = (channel: number): number =>
  channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;

const luminance = (hex: string): number => {
  const [r, g, b] = channels(hex).map(linearise) as unknown as [number, number, number];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};

const contrast = (aHex: string, bHex: string): number => {
  const [lighter, darker] = [luminance(aHex), luminance(bHex)].sort((x, y) => y - x) as [
    number,
    number,
  ];
  return (lighter + 0.05) / (darker + 0.05);
};

/** Resolves a token name to its hex value, failing loudly if it is neither. */
const hexToken = (name: string): string => {
  const value = tokens.get(name);
  if (value === undefined) throw new Error(`tokens.css does not define ${name}`);
  if (!/^#[0-9a-f]{6}$/i.test(value)) {
    throw new Error(`${name} is ${value}, not a six-digit hex colour`);
  }
  return value;
};

/** The four backgrounds any foreground token may legitimately land on. */
const surfaces = [
  "--color-bg",
  "--color-surface",
  "--color-surface-raised",
  "--color-surface-hover",
] as const;

const statusIntents = ["neutral", "info", "success", "warning", "danger"] as const;

// SC 1.4.3 Contrast (Minimum): 4.5:1 for body text. The base font size is
// 14px (--text-md) and the largest is 20px (--text-xl), so nothing in this
// app reaches the 24px/18.66px-bold "large text" threshold that would allow
// 3:1 -- every text pair below is held to 4.5:1, with no exceptions.
const TEXT_MIN = 4.5;
// SC 1.4.11 Non-text Contrast: 3:1 for the visual boundary of an active UI
// component and for focus indicators.
const NON_TEXT_MIN = 3;

/** [description, foreground token, background token, minimum ratio]. */
const textPairs: ReadonlyArray<readonly [string, string, string, number]> = [
  // Every text tier must survive on every surface -- there is no "this token
  // is only for the page background" caveat to remember.
  ...(["--color-text", "--color-text-muted", "--color-text-faint"] as const).flatMap((fg) =>
    surfaces.map((bg) => [`${fg} on ${bg}`, fg, bg, TEXT_MIN] as const),
  ),
  // A status foreground has to work twice: inside its own tinted pill, and
  // as bare coloured text in a table cell on any surface.
  ...statusIntents.flatMap((intent) => {
    const fg = `--color-${intent}-fg`;
    return [
      [`${fg} on its own tint`, fg, `--color-${intent}-bg`, TEXT_MIN] as const,
      ...surfaces.map((bg) => [`${fg} on ${bg}`, fg, bg, TEXT_MIN] as const),
    ];
  }),
  // A filled accent button must stay legible in both rest and hover states,
  // which is why --color-accent-hover is only slightly lighter.
  ["--color-text-on-accent on --color-accent", "--color-text-on-accent", "--color-accent", TEXT_MIN],
  [
    "--color-text-on-accent on --color-accent-hover",
    "--color-text-on-accent",
    "--color-accent-hover",
    TEXT_MIN,
  ],
  // A selected row / active tab takes ordinary body text on the muted fill.
  ["--color-text on --color-accent-muted", "--color-text", "--color-accent-muted", TEXT_MIN],
];

const nonTextPairs: ReadonlyArray<readonly [string, string, string, number]> = [
  // The boundary of an input or secondary button, on any surface.
  ...surfaces.map((bg) => [`--color-border-strong vs ${bg}`, "--color-border-strong", bg, NON_TEXT_MIN] as const),
  // The focus ring, on any surface.
  ...surfaces.map((bg) => [`--color-focus vs ${bg}`, "--color-focus", bg, NON_TEXT_MIN] as const),
  // A filled accent button carries no border, so its fill IS its boundary.
  ...surfaces.map((bg) => [`--color-accent vs ${bg}`, "--color-accent", bg, NON_TEXT_MIN] as const),
];

describe("token contrast", () => {
  // --color-border is absent from these tables on purpose: it is documented
  // as decorative separation only (table rules, hairlines), which SC 1.4.11
  // exempts. --color-border-strong is the token for anything that bounds a
  // control, and it is checked.
  it.each([...textPairs, ...nonTextPairs])(
    "%s meets %d:1",
    (_description, fg, bg, minimum) => {
      expect(contrast(hexToken(fg), hexToken(bg))).toBeGreaterThanOrEqual(minimum);
    },
  );

  // Not a WCAG requirement (a tint behind compliant text has no minimum of
  // its own), but a tint indistinguishable from the page defeats the point
  // of a pill. 1.25:1 is the floor these were tuned to.
  it.each(statusIntents.filter((intent) => intent !== "neutral"))(
    "%s tint is distinguishable from the page background",
    (intent) => {
      expect(
        contrast(hexToken(`--color-${intent}-bg`), hexToken("--color-bg")),
      ).toBeGreaterThanOrEqual(1.25);
    },
  );
});

describe("token references", () => {
  // `var(--typo)` is silent at build time and at runtime: the property just
  // resolves to its inherited or initial value and the component renders
  // subtly wrong. This is the check that turns that into a failure.
  it("resolves every var() used in a global stylesheet or a CSS Module", () => {
    const sources: ReadonlyArray<readonly [string, string]> = [
      ["reset.css", resetCss],
      ...moduleStylesheets(),
    ];
    const unknown = sources.flatMap(([file, css]) =>
      [...stripComments(css).matchAll(/var\(\s*(--[\w-]+)/g)]
        .map(([, name]) => name)
        .filter((name): name is string => name !== undefined && !tokens.has(name))
        .map((name) => `${file}: ${name}`),
    );
    expect(unknown).toEqual([]);
  });

  it("finds CSS Modules to check, so the check above cannot pass vacuously", () => {
    expect(moduleStylesheets().length).toBeGreaterThan(0);
  });
});

describe("palette discipline", () => {
  // The point of a token vocabulary is that a screen author never reaches
  // for a raw colour. Enforced rather than requested, because "we agreed to
  // use tokens" survives about two sprints on its own.
  it("hard-codes no colour in any CSS Module", () => {
    const literal = /#[0-9a-f]{3,8}\b|\b(?:rgba?|hsla?|color-mix|oklch|lab)\(/gi;
    const offences = moduleStylesheets().flatMap(([file, css]) =>
      [...stripComments(css).matchAll(literal)].map((match) => `${file}: ${match[0]}`),
    );
    expect(offences).toEqual([]);
  });
});

describe("dark-only theming", () => {
  // docs/web-frontend-spec.md -> Conventions: "Dark theme only -- a single
  // theme, no light mode or toggle". The tokens are declared unconditionally
  // on :root; a colour-scheme media query or a [data-theme] selector
  // appearing in a global stylesheet means someone has started a second
  // theme, which is a spec change, not a refactor.
  it.each([
    ["tokens.css", tokensCss],
    ["reset.css", resetCss],
  ])("declares no light-theme fork in %s", (_file, css) => {
    const body = stripComments(css);
    expect(body).not.toMatch(/prefers-color-scheme/);
    expect(body).not.toMatch(/\[data-theme/);
  });

  // Without this the page gets light scrollbars, a white overscroll canvas
  // and light native <select> popups on a dark app -- the one UA-level
  // signal that no amount of token discipline can replace.
  it("opts the user agent into dark chrome", () => {
    expect(tokens.get("color-scheme")).toBeUndefined();
    expect(stripComments(tokensCss)).toMatch(/:root\s*\{[\s\S]*?color-scheme:\s*dark;/);
  });
});
