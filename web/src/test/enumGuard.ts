import type { DescEnum, DescField, DescFile, DescMessage, Message } from "@bufbuild/protobuf";
import {
  isReflectList,
  isReflectMap,
  isReflectMessage,
  reflect,
  type ReflectMessage,
} from "@bufbuild/protobuf/reflect";
import { expect } from "vitest";

/**
 * The schema-driven half of loam-mvso's fixture guard, generalised (loam-yhcz).
 *
 * WHY THIS EXISTS AT ALL. protobuf-es's `MessageInitShape` makes every field
 * optional -- correct for a partial-init type, and the reason a hand-built
 * fixture that OMITS a proto3 enum is accepted by `tsc --noEmit` without a
 * word, decodes as `UNSPECIFIED = 0`, and is indistinguishable in review from
 * a deliberate unset. The type checker structurally cannot catch that class,
 * and what a reviewer sees in the diff is an absence. So it is asserted.
 *
 * WHY IT DISCOVERS RATHER THAN ENUMERATES. `fixtures.test.ts` used to pin
 * `["conflict", "state", "upstreamDrift"]` by hand, and that list -- not the
 * sweep beside it -- was what actually closed the holes. A hand-written list
 * covers the fields someone remembered on the day they wrote it; the next enum
 * field added to the message slips straight through, with a test file standing
 * next to it claiming coverage. That is worse than no guard. Everything below
 * is derived from the generated descriptors instead, so a new enum field on a
 * covered message fails without anyone editing this file.
 *
 * WHAT COUNTS AS "UNSPECIFIED". A field is reported when it is NOT SET, which
 * is deliberately stronger than `value !== 0`:
 *
 *  - implicit presence (a plain proto3 `Enum e = 1`): unset == the zero ==
 *    `UNSPECIFIED`, the value a real server never sends because it sets a
 *    positive `NONE`/named member on every healthy record.
 *  - explicit presence (`optional Enum e = 1`): unset is `undefined`, NOT 0 --
 *    so the old `not.toBe(0)` assertion PASSED on exactly the omission it
 *    existed to catch. Presence, not the number, is the right question.
 *  - `repeated`/`map` enums: unset is the empty list/map, which the old sweep
 *    skipped entirely via its `fieldKind === "enum"` filter. Reported when
 *    empty, and every element is additionally checked for a zero -- an
 *    `UNSPECIFIED` sitting INSIDE a populated list is the same defect.
 *  - enum members of a `oneof`: only one member can be set, so the GROUP is
 *    reported when no case is selected, and the selected member is checked.
 *
 * Requiring presence on a `repeated` enum is the one place this is stricter
 * than "a value the server never sends" -- an empty list is something a server
 * sends routinely. It is still the right default here, for the reason above:
 * an omitted repeated field and a deliberately empty one are the same bytes,
 * so the only way to tell them apart is to make the deliberate one say so.
 * {@link expectNoUnspecifiedEnums} is where it says so.
 *
 * Enums whose zero is NOT `UNSPECIFIED` are skipped: their zero is a real
 * value a server does send, so an omission there is not this defect. `buf
 * lint`'s STANDARD group (ENUM_ZERO_VALUE_SUFFIX) means there are none today,
 * and `enumGuard.test.ts` re-checks that rather than assuming it.
 */

/** One enum field that a fixture left at a value a real server does not send. */
export interface UnspecifiedEnum {
  /** Dotted path from the root message, e.g. `workBranch.upstreamDrift` or `verdicts[0].outcome`. */
  readonly path: string;
  /** Fully qualified name of the enum, e.g. `loam.v1.UpstreamDrift`. */
  readonly enumType: string;
  /** `unset` -- the field was never given a value; `element` -- a populated list/map holds a zero. */
  readonly why: "unset" | "element";
}

/**
 * True when the enum's zero value is the proto3 `UNSPECIFIED` placeholder, and
 * therefore a value no healthy record carries. `localName` is `UNSPECIFIED`
 * whenever protoc-gen-es strips a shared prefix; the `name` check covers an
 * enum that has no shared prefix to strip.
 */
const hasUnspecifiedZero = (desc: DescEnum): boolean =>
  desc.values.some(
    (value) => value.number === 0 && (value.localName === "UNSPECIFIED" || value.name.endsWith("_UNSPECIFIED")),
  );

/** The enum a field carries, whether singular, `repeated` or a map value; `undefined` if it carries none. */
const enumOfField = (field: DescField): DescEnum | undefined => {
  if (field.fieldKind === "enum") return field.enum;
  if (field.fieldKind === "list" && field.listKind === "enum") return field.enum;
  if (field.fieldKind === "map" && field.mapKind === "enum") return field.enum;
  return undefined;
};

/** Every message declared in the file, including messages nested inside others. */
const messagesIn = (file: DescFile): DescMessage[] => {
  const out: DescMessage[] = [];
  const visit = (message: DescMessage): void => {
    out.push(message);
    message.nestedMessages.forEach(visit);
  };
  file.messages.forEach(visit);
  return out;
};

/** Every enum declared in the file, including enums nested inside messages. */
const enumsIn = (file: DescFile): DescEnum[] => {
  const out: DescEnum[] = [...file.enums];
  messagesIn(file).forEach((message) => out.push(...message.nestedEnums));
  return out;
};

const walk = (message: ReflectMessage, prefix: string, seen: Set<Message>, out: UnspecifiedEnum[]): void => {
  if (seen.has(message.message)) return;
  seen.add(message.message);
  const reportedOneofs = new Set<string>();
  for (const field of message.fields) {
    const path = `${prefix}${field.localName}`;
    const enumType = enumOfField(field);
    if (enumType !== undefined) {
      if (!hasUnspecifiedZero(enumType)) continue;
      if (field.oneof !== undefined) {
        const selected = message.oneofCase(field.oneof);
        if (selected === undefined) {
          // Report the group once: only one member can ever be set, so
          // reporting each member would name fields that CANNOT be set.
          if (!reportedOneofs.has(field.oneof.localName)) {
            reportedOneofs.add(field.oneof.localName);
            out.push({ path: `${prefix}${field.oneof.localName}`, enumType: enumType.typeName, why: "unset" });
          }
          continue;
        }
        if (selected !== field) continue;
      } else if (!message.isSet(field)) {
        out.push({ path, enumType: enumType.typeName, why: "unset" });
        continue;
      }
      const value = message.get(field);
      if (isReflectList(value)) {
        for (const [index, element] of value.entries()) {
          if (element === 0) out.push({ path: `${path}[${index}]`, enumType: enumType.typeName, why: "element" });
        }
      } else if (isReflectMap(value)) {
        for (const [key, element] of value) {
          if (element === 0) {
            out.push({ path: `${path}[${String(key)}]`, enumType: enumType.typeName, why: "element" });
          }
        }
      } else if (value === 0) {
        // Only reachable for an explicit-presence or oneof member that was set
        // to UNSPECIFIED on purpose; `isSet` is false for the implicit case.
        out.push({ path, enumType: enumType.typeName, why: "unset" });
      }
      continue;
    }
    // Not an enum field, so recurse: an omission inside a nested message is
    // the same defect one level down, and a fixture for a message that embeds
    // WorkBranch is exactly where it hides.
    if (!message.isSet(field)) continue;
    const value = message.get(field);
    if (isReflectMessage(value)) {
      walk(value, `${path}.`, seen, out);
    } else if (isReflectList(value)) {
      for (const [index, element] of value.entries()) {
        if (isReflectMessage(element)) walk(element, `${path}[${index}].`, seen, out);
      }
    } else if (isReflectMap(value)) {
      for (const [key, element] of value) {
        if (isReflectMessage(element)) walk(element, `${path}[${String(key)}].`, seen, out);
      }
    }
  }
};

/**
 * Every enum field, at any depth, that `message` left at a value a real server
 * does not send. Empty means the fixture is faithful.
 *
 * Unset MESSAGE fields are not descended into: an absent sub-message is
 * genuinely absent on the wire, not a zero standing in for one.
 */
export const findUnspecifiedEnums = (schema: DescMessage, message: Message): UnspecifiedEnum[] => {
  const out: UnspecifiedEnum[] = [];
  walk(reflect(schema, message), "", new Set(), out);
  return out;
};

/**
 * Asserts `message` leaves no enum field at an `UNSPECIFIED` it cannot have
 * come by honestly.
 *
 * THE DELIBERATE ZERO. "No enum may be zero" is not obviously right: a test
 * for how the console renders a field a NEWER server did not send is a real
 * test and it needs a zero. `allowUnspecified` is how that is said -- keyed by
 * the exact dotted path, valued by the reason, at the call site. Two
 * properties make it loud rather than a silencer:
 *
 *  - the path is spelled out, so the diff shows a decision instead of an
 *    absence, which is the entire failure mode being designed against;
 *  - an allowance that no longer fires FAILS. A path that has since been
 *    filled in cannot sit here rotting, quietly re-opening the hole for
 *    whatever field takes its place.
 *
 * The cheaper opt-out, and the one most tests should reach for, is not this
 * argument at all: pass the zero through the fixture builder's overrides
 * (`workBranchFixture({ conflict: WorkBranchConflict.UNSPECIFIED })`). That is
 * already explicit at the call site -- you type the word UNSPECIFIED -- and
 * needs nothing here. `allowUnspecified` is for asserting over a message whose
 * zero you did not choose field by field.
 */
export const expectNoUnspecifiedEnums = (
  schema: DescMessage,
  message: Message,
  allowUnspecified: Readonly<Record<string, string>> = {},
): void => {
  const found = findUnspecifiedEnums(schema, message);
  const unexpected = found
    .filter((entry) => allowUnspecified[entry.path] === undefined)
    .map((entry) => `${entry.path} (${entry.enumType}, ${entry.why})`);
  expect(unexpected, `${schema.typeName} fixture leaves enum fields at UNSPECIFIED`).toEqual([]);
  const stale = Object.keys(allowUnspecified).filter((path) => !found.some((entry) => entry.path === path));
  expect(stale, `${schema.typeName}: allowUnspecified names paths that are no longer UNSPECIFIED`).toEqual([]);
};

/**
 * Every generated `DescFile`, found by globbing `src/gen` rather than by an
 * import list.
 *
 * The import list is the same self-staling defect one layer up: a new
 * `.proto` file would land in `src/gen`, carry new enums, and be invisible to
 * a guard that only knows the files someone typed out. `src/gen` is committed
 * (see `codegen.test.ts`), so the glob resolves at build time with no
 * generation step.
 */
export const generatedFiles = (): DescFile[] => {
  const modules = import.meta.glob<Record<string, unknown>>("../gen/**/*.ts", { eager: true });
  const files: DescFile[] = [];
  for (const module of Object.values(modules)) {
    for (const exported of Object.values(module)) {
      if (typeof exported === "object" && exported !== null && "kind" in exported && exported.kind === "file") {
        files.push(exported as DescFile);
      }
    }
  }
  return files;
};

/**
 * Every message in `files` that DECLARES at least one enum field whose enum has
 * an `UNSPECIFIED` zero -- the messages a fixture can silently get wrong.
 *
 * Declared, not reached: a message that merely embeds one of these is covered
 * by {@link findUnspecifiedEnums}'s recursion through whatever fixture builds
 * it, so listing it here would demand a builder for every response wrapper in
 * the schema.
 */
export const messagesDeclaringEnumFields = (files: readonly DescFile[]): DescMessage[] =>
  files
    .flatMap(messagesIn)
    .filter((message) =>
      message.fields.some((field) => {
        const enumType = enumOfField(field);
        return enumType !== undefined && hasUnspecifiedZero(enumType);
      }),
    );

/** Every enum declared across `files`, including enums nested inside messages. */
export const generatedEnums = (files: readonly DescFile[]): DescEnum[] => files.flatMap(enumsIn);

/** Exposed so `enumGuard.test.ts` can pin the `UNSPECIFIED`-zero assumption the guard is scoped by. */
export const enumHasUnspecifiedZero = hasUnspecifiedZero;
