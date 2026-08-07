import {
  create,
  createFileRegistry,
  type DescMessage,
  type Message,
  type MessageInitShape,
} from "@bufbuild/protobuf";
import {
  DescriptorProtoSchema,
  FieldDescriptorProto_Label,
  FieldDescriptorProto_Type,
  FieldDescriptorProtoSchema,
  FileDescriptorSetSchema,
} from "@bufbuild/protobuf/wkt";

type FieldInit = MessageInitShape<typeof FieldDescriptorProtoSchema>;
type MessageDescInit = MessageInitShape<typeof DescriptorProtoSchema>;
import { describe, expect, it } from "vitest";
import {
  enumHasUnspecifiedZero,
  expectNoUnspecifiedEnums,
  findUnspecifiedEnums,
  generatedEnums,
  generatedFiles,
  messagesDeclaringEnumFields,
} from "./enumGuard";

/**
 * The walker's own tests, against a descriptor built here rather than against
 * `src/gen`.
 *
 * This is not a convenience. `loam/*.proto` today has no `repeated` enum, no
 * map-valued enum, no enum inside a `oneof`, and no enum whose zero is a real
 * value -- so every one of those branches would ship untested if the walker
 * were only ever pointed at the real schema. Those are exactly the shapes
 * loam-mvso's sweep got wrong (`fieldKind === "list"` filtered a repeated enum
 * out; `not.toBe(0)` passed on an `optional` one), and they are the shapes
 * that will arrive without anyone remembering this file exists.
 *
 * `createFileRegistry` over a hand-built `FileDescriptorSet` gives real
 * descriptors with no `.proto` file, no codegen step, and nothing outside
 * `web/**`.
 */

const enumField = (
  name: string,
  number: number,
  typeName: string,
  extra: FieldInit = {},
): FieldInit => ({
  name,
  number,
  type: FieldDescriptorProto_Type.ENUM,
  label: FieldDescriptorProto_Label.OPTIONAL,
  typeName,
  jsonName: name,
  ...extra,
});

/**
 * A synthetic proto3 file carrying one enum per shape the walker must handle.
 * Field numbering and `oneofIndex` wiring mirror what protoc emits, including
 * the synthetic `_singular_optional` oneof a proto3 `optional` desugars into.
 */
const testFile = () => {
  const mapEntry: MessageDescInit = {
    name: "FlagsEntry",
    options: { mapEntry: true },
    field: [
      {
        name: "key",
        number: 1,
        type: FieldDescriptorProto_Type.STRING,
        label: FieldDescriptorProto_Label.OPTIONAL,
        jsonName: "key",
      },
      enumField("value", 2, ".test.v1.Flag"),
    ],
  };
  const set = create(FileDescriptorSetSchema, {
    file: [
      {
        name: "test/v1/test.proto",
        package: "test.v1",
        syntax: "proto3",
        enumType: [
          {
            name: "Flag",
            value: [
              { name: "FLAG_UNSPECIFIED", number: 0 },
              { name: "FLAG_NONE", number: 1 },
              { name: "FLAG_RAISED", number: 2 },
            ],
          },
          {
            // Zero is a REAL value here, so an omission is not the
            // impossible-value defect and the walker must stay quiet.
            name: "Mode",
            value: [
              { name: "MODE_FAST", number: 0 },
              { name: "MODE_SLOW", number: 1 },
            ],
          },
        ],
        messageType: [
          {
            name: "Inner",
            field: [enumField("inner_flag", 1, ".test.v1.Flag")],
          },
          {
            name: "Outer",
            nestedType: [mapEntry],
            oneofDecl: [{ name: "_singular_optional" }, { name: "choice" }],
            field: [
              enumField("singular", 1, ".test.v1.Flag"),
              enumField("singular_optional", 2, ".test.v1.Flag", {
                proto3Optional: true,
                oneofIndex: 0,
              }),
              enumField("repeated_flag", 3, ".test.v1.Flag", {
                label: FieldDescriptorProto_Label.REPEATED,
              }),
              {
                name: "flags",
                number: 4,
                type: FieldDescriptorProto_Type.MESSAGE,
                label: FieldDescriptorProto_Label.REPEATED,
                typeName: ".test.v1.Outer.FlagsEntry",
                jsonName: "flags",
              },
              {
                name: "inner",
                number: 5,
                type: FieldDescriptorProto_Type.MESSAGE,
                label: FieldDescriptorProto_Label.OPTIONAL,
                typeName: ".test.v1.Inner",
                jsonName: "inner",
              },
              {
                name: "inners",
                number: 6,
                type: FieldDescriptorProto_Type.MESSAGE,
                label: FieldDescriptorProto_Label.REPEATED,
                typeName: ".test.v1.Inner",
                jsonName: "inners",
              },
              enumField("mode", 7, ".test.v1.Mode"),
              enumField("choice_flag", 8, ".test.v1.Flag", { oneofIndex: 1 }),
            ],
          },
        ],
      },
    ],
  });
  return createFileRegistry(set);
};

const registry = testFile();

const schemaFor = (typeName: string): DescMessage => {
  const desc = registry.getMessage(typeName);
  if (desc === undefined) throw new Error(`no descriptor for ${typeName}`);
  return desc;
};

const Outer = schemaFor("test.v1.Outer");
const Inner = schemaFor("test.v1.Inner");

/** Paths only, since that is what a failure message shows a reader. */
const paths = (schema: DescMessage, message: Message): string[] =>
  findUnspecifiedEnums(schema, message).map((entry) => entry.path);

/** A fully faithful Outer, so each test can break exactly one thing. */
const faithfulOuter = (overrides: Record<string, unknown> = {}): Message =>
  create(Outer, {
    singular: 1,
    singularOptional: 1,
    repeatedFlag: [1],
    flags: { a: 1 },
    inner: { innerFlag: 1 },
    inners: [{ innerFlag: 1 }],
    choice: { case: "choiceFlag", value: 1 },
    ...overrides,
  });

describe("findUnspecifiedEnums", () => {
  it("is silent on a fixture that sets every enum", () => {
    expect(paths(Outer, faithfulOuter())).toEqual([]);
  });

  it("catches a plain proto3 enum left at its zero", () => {
    expect(paths(Outer, faithfulOuter({ singular: 0 }))).toEqual(["singular"]);
  });

  it("catches an OPTIONAL enum that was never set, which reads undefined and not 0", () => {
    // The hole loam-mvso's `not.toBe(0)` passed on: this field is `undefined`,
    // so `!== 0` is TRUE and the old assertion said nothing.
    const message = faithfulOuter({ singularOptional: undefined });
    expect((message as unknown as Record<string, unknown>)["singularOptional"]).toBeUndefined();
    expect(paths(Outer, message)).toEqual(["singularOptional"]);
  });

  it("catches an OPTIONAL enum deliberately set to the zero, which is a different fact from unset", () => {
    expect(paths(Outer, faithfulOuter({ singularOptional: 0 }))).toEqual(["singularOptional"]);
  });

  it("catches an empty REPEATED enum, which the old fieldKind filter dropped entirely", () => {
    expect(paths(Outer, faithfulOuter({ repeatedFlag: [] }))).toEqual(["repeatedFlag"]);
  });

  it("catches an UNSPECIFIED sitting inside a populated repeated enum", () => {
    expect(paths(Outer, faithfulOuter({ repeatedFlag: [1, 0, 2] }))).toEqual(["repeatedFlag[1]"]);
  });

  it("catches an empty map-valued enum", () => {
    expect(paths(Outer, faithfulOuter({ flags: {} }))).toEqual(["flags"]);
  });

  it("catches an UNSPECIFIED map value, naming the key", () => {
    expect(paths(Outer, faithfulOuter({ flags: { a: 1, b: 0 } }))).toEqual(["flags[b]"]);
  });

  it("recurses into a nested message field", () => {
    expect(paths(Outer, faithfulOuter({ inner: { innerFlag: 0 } }))).toEqual(["inner.innerFlag"]);
  });

  it("recurses into a repeated message field, naming the index", () => {
    const message = faithfulOuter({ inners: [{ innerFlag: 1 }, { innerFlag: 0 }] });
    expect(paths(Outer, message)).toEqual(["inners[1].innerFlag"]);
  });

  it("does not descend into an UNSET message field, which is genuinely absent rather than zero", () => {
    expect(paths(Outer, faithfulOuter({ inner: undefined }))).toEqual([]);
  });

  it("ignores an enum whose zero is a real value a server does send", () => {
    // `mode` is never set by faithfulOuter and sits at MODE_FAST = 0
    // throughout. Reporting it would make the guard cry wolf on a legitimate
    // default, which is how a guard gets worked around.
    expect(paths(Outer, faithfulOuter())).toEqual([]);
    expect(paths(Outer, faithfulOuter({ mode: 1 }))).toEqual([]);
  });

  it("reports a oneof carrying an enum ONCE, by group, when no case is selected", () => {
    // Naming each member would name fields that CANNOT all be set.
    expect(paths(Outer, faithfulOuter({ choice: { case: undefined } }))).toEqual(["choice"]);
  });

  it("checks the selected oneof member's value", () => {
    expect(paths(Outer, faithfulOuter({ choice: { case: "choiceFlag", value: 0 } }))).toEqual([
      "choiceFlag",
    ]);
  });

  it("reports every offending field, not just the first", () => {
    const message = faithfulOuter({ singular: 0, repeatedFlag: [], inner: { innerFlag: 0 } });
    expect(paths(Outer, message)).toEqual(["singular", "repeatedFlag", "inner.innerFlag"]);
  });

  it("records why each field was reported, so a failure distinguishes unset from a bad element", () => {
    expect(findUnspecifiedEnums(Outer, faithfulOuter({ repeatedFlag: [0] }))).toEqual([
      { path: "repeatedFlag[0]", enumType: "test.v1.Flag", why: "element" },
    ]);
    expect(findUnspecifiedEnums(Inner, create(Inner))).toEqual([
      { path: "innerFlag", enumType: "test.v1.Flag", why: "unset" },
    ]);
  });

  it("terminates on a message that holds itself, rather than recursing forever", () => {
    const message = create(Outer, { inner: { innerFlag: 1 } }) as unknown as Record<string, unknown>;
    message["inners"] = [message];
    expect(() => findUnspecifiedEnums(Outer, message as unknown as Message)).not.toThrow();
  });
});

describe("expectNoUnspecifiedEnums", () => {
  it("passes when an allowance names the exact path that is unset", () => {
    expectNoUnspecifiedEnums(Inner, create(Inner), {
      innerFlag: "the older-server case: this field is what the test is about",
    });
  });

  it("fails when an allowance names a path that is no longer unset, so it cannot rot", () => {
    expect(() =>
      expectNoUnspecifiedEnums(Inner, create(Inner, { innerFlag: 1 }), {
        innerFlag: "stale",
      }),
    ).toThrow(/no longer UNSPECIFIED/);
  });

  it("fails on an unset field the allowances do not name", () => {
    expect(() => expectNoUnspecifiedEnums(Inner, create(Inner))).toThrow(
      /leaves enum fields at UNSPECIFIED/,
    );
  });

  it("names the field, its enum and the reason in the failure, so the diff is readable", () => {
    expect(() => expectNoUnspecifiedEnums(Inner, create(Inner))).toThrow(
      /innerFlag \(test\.v1\.Flag, unset\)/,
    );
  });
});

describe("generated schema assumptions", () => {
  const files = generatedFiles();

  it("globs every generated file, both proto packages included", () => {
    const names = files.map((file) => file.name);
    expect(names).toContain("loam/v1/common");
    expect(names).toContain("loam/admin/v1/repo_admin");
    expect(names.length).toBeGreaterThanOrEqual(10);
  });

  it("finds every enum in the schema, and every one of them has an UNSPECIFIED zero", () => {
    // The guard SKIPS enums whose zero is a real value, so this is the
    // assumption that makes "unset == a value no server sends" hold across
    // the whole schema. `buf lint`'s STANDARD group enforces the suffix
    // server-side; this is the web side noticing if that ever stops being
    // true, because the guard would go quiet rather than fail.
    const enums = generatedEnums(files);
    expect(enums.length).toBeGreaterThan(0);
    expect(enums.filter((desc) => !enumHasUnspecifiedZero(desc)).map((desc) => desc.typeName)).toEqual(
      [],
    );
  });

  it("reports messages that DECLARE an enum field, not every message that reaches one", () => {
    const declaring = messagesDeclaringEnumFields(files).map((message) => message.typeName);
    expect(declaring).toContain("loam.v1.WorkBranch");
    // Proposal only EMBEDS WorkBranch, so it is covered by the recursion in
    // findUnspecifiedEnums rather than by needing its own entry -- otherwise
    // every response wrapper in the schema would demand a builder.
    expect(declaring).not.toContain("loam.admin.v1.Proposal");
    expect(declaring).not.toContain("loam.admin.v1.ListProposalsResponse");
  });
});
