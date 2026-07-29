import { create } from "@bufbuild/protobuf";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { AppProviders } from "../App";
import { RoleSchema, type Role } from "../gen/loam/admin/v1/role_pb";
import { operationVocabulary, Roles } from "./Roles";

/**
 * These mount the real screen against a real `QueryClient` and the real
 * connect-web `transport` (`AppProviders`, per the bead's DESIGN note), with
 * only `fetch` stubbed -- the same level `transport.test.ts` and
 * `invalidation.test.tsx` stub at. The wouter memory Router is included even
 * though Roles reads no route params, matching the bead's own test recipe
 * (QueryClientProvider + a wouter in-memory Router + a stubbed transport)
 * rather than depending on `Roles` never growing a `<Link>`.
 */

interface FakeRole {
  name: string;
  operations: string[];
  instructions: string;
  builtin: boolean;
}

interface RecordedCall {
  readonly method: "ListRoles" | "CreateRole" | "UpdateRole" | "DeleteRole";
  readonly body: Record<string, unknown>;
}

const jsonResponse = (body: unknown, status = 200): Promise<Response> =>
  Promise.resolve(
    new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } }),
  );

/**
 * connect-web's JSON codec serialises a unary request body as a `Uint8Array`
 * of UTF-8 bytes, not a string -- `String(uint8Array)` gives a comma-joined
 * byte list ("123,125"), which is not JSON and makes `JSON.parse` throw
 * synchronously inside the `fetch` mock, before it even reaches the URL
 * dispatch below. That throw doesn't surface as a query error either: it
 * aborts the mock outside of any promise chain react-query is watching, so
 * the query is left permanently pending with zero recorded calls -- the
 * "stuck on the loading state forever" failure this decode step exists to
 * avoid.
 */
const decodeBody = (body: BodyInit | null | undefined): Record<string, unknown> => {
  if (body === null || body === undefined) return {};
  if (typeof body === "string") return body === "" ? {} : (JSON.parse(body) as Record<string, unknown>);
  // `ArrayBuffer.isView`, not `instanceof Uint8Array`: connect-web's binary
  // body can be constructed in a different realm than this test's jsdom
  // globals (a known jsdom cross-realm quirk), which makes `instanceof`
  // unreliable here even though the value genuinely is a typed array.
  // `isView` checks the engine-level internal slot instead of the prototype
  // chain, so it agrees across realms.
  if (ArrayBuffer.isView(body)) {
    const text = new TextDecoder().decode(body as Uint8Array);
    return text === "" ? {} : (JSON.parse(text) as Record<string, unknown>);
  }
  throw new Error(`Roles.test.tsx decodeBody: unsupported body type ${Object.prototype.toString.call(body)}`);
};

/**
 * A minimal in-memory RoleService, keyed on the same URLs the real transport
 * posts to (`transport.test.ts`: `/loam.admin.v1.<Service>/<Method>`).
 *
 * Deliberately does NOT enforce "a builtin role cannot be deleted" the way
 * the real server does (internal/handler/role.go's DeleteRole). That guard
 * belongs to the UI here -- Roles.tsx makes a builtin role's Delete button
 * natively `disabled` -- and this fake must not double up on it, or a test
 * that breaks the frontend guard (e.g. flips the `disabled={role.builtin}`
 * condition) would still pass by accident: the fake would silently refuse
 * the delete the broken UI allowed through, masking exactly the regression
 * the guard test below exists to catch.
 */
function fakeRoleServer(initial: readonly FakeRole[]) {
  let roles: FakeRole[] = initial.map((role) => ({ ...role }));
  const calls: RecordedCall[] = [];
  const handle = (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = String(input);
    const body = decodeBody(init?.body);
    if (url.endsWith(".RoleService/ListRoles")) {
      calls.push({ method: "ListRoles", body });
      return jsonResponse({ roles });
    }
    if (url.endsWith(".RoleService/CreateRole")) {
      calls.push({ method: "CreateRole", body });
      const requested = body["role"] as Partial<FakeRole>;
      if (roles.some((role) => role.name === requested.name)) {
        return jsonResponse({ code: "already_exists", message: `role ${requested.name} already exists` }, 409);
      }
      const created: FakeRole = {
        name: requested.name ?? "",
        operations: requested.operations ?? [],
        instructions: requested.instructions ?? "",
        builtin: false,
      };
      roles = [...roles, created];
      return jsonResponse({ role: created });
    }
    if (url.endsWith(".RoleService/UpdateRole")) {
      calls.push({ method: "UpdateRole", body });
      const requested = body["role"] as Partial<FakeRole>;
      const existing = roles.find((role) => role.name === requested.name);
      if (existing === undefined) {
        return jsonResponse({ code: "not_found", message: `role ${requested.name ?? ""}` }, 404);
      }
      const updated: FakeRole = {
        ...existing,
        operations: requested.operations ?? [],
        instructions: requested.instructions ?? "",
      };
      roles = roles.map((role) => (role.name === updated.name ? updated : role));
      return jsonResponse({ role: updated });
    }
    if (url.endsWith(".RoleService/DeleteRole")) {
      calls.push({ method: "DeleteRole", body });
      roles = roles.filter((role) => role.name !== body["name"]);
      return jsonResponse({});
    }
    throw new Error(`unhandled fetch in Roles.test.tsx: ${url}`);
  };
  return { handle, calls, getRoles: (): readonly FakeRole[] => roles };
}

const author: FakeRole = {
  name: "author",
  operations: ["git.push", "work.start"],
  instructions: "",
  builtin: true,
};
const triage: FakeRole = {
  name: "triage",
  operations: ["search"],
  instructions: "Read-only recon.",
  builtin: false,
};

const renderRoles = (): void => {
  const location = memoryLocation({ path: "/roles", record: true });
  render(
    <AppProviders>
      <Router hook={location.hook}>
        <Roles />
      </Router>
    </AppProviders>,
  );
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("operationVocabulary", () => {
  it("unions every role's operations, de-duplicated and sorted", () => {
    const roles: Role[] = [
      create(RoleSchema, { name: "a", operations: ["work.start", "search"], instructions: "", builtin: true }),
      create(RoleSchema, { name: "b", operations: ["search", "git.push"], instructions: "", builtin: false }),
    ];
    expect(operationVocabulary(roles)).toEqual(["git.push", "search", "work.start"]);
  });

  it("is empty when there are no roles to derive it from", () => {
    expect(operationVocabulary([])).toEqual([]);
  });
});

describe("Roles", () => {
  it("shows a loading state before ListRoles resolves", () => {
    vi.stubGlobal("fetch", () => new Promise<Response>(() => undefined));
    renderRoles();
    expect(screen.getByText("Loading roles…")).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("shows the empty message when ListRoles returns no roles", async () => {
    vi.stubGlobal("fetch", fakeRoleServer([]).handle);
    renderRoles();
    expect(await screen.findByRole("cell", { name: "No roles configured." })).toBeInTheDocument();
  });

  it("shows an ErrorBanner carrying the server's message when ListRoles fails", async () => {
    vi.stubGlobal("fetch", () => jsonResponse({ code: "internal", message: "db down" }, 500));
    renderRoles();
    expect(await screen.findByRole("alert")).toHaveTextContent("db down");
  });

  it("falls back to a generic message when the server sends none", async () => {
    vi.stubGlobal("fetch", () => jsonResponse({ code: "internal" }, 500));
    renderRoles();
    expect(await screen.findByRole("alert")).toHaveTextContent("Something went wrong. Please try again.");
  });

  it("renders each role's name, operations and builtin flag", async () => {
    vi.stubGlobal("fetch", fakeRoleServer([author, triage]).handle);
    renderRoles();
    const authorRow = (await screen.findByRole("rowheader", { name: "author" })).closest("tr");
    const triageRow = screen.getByRole("rowheader", { name: "triage" }).closest("tr");
    expect(authorRow).not.toBeNull();
    expect(triageRow).not.toBeNull();
    expect(within(authorRow as HTMLElement).getByText("git.push, work.start")).toBeInTheDocument();
    expect(within(authorRow as HTMLElement).getByText("Built-in")).toBeInTheDocument();
    expect(within(triageRow as HTMLElement).getByText("search")).toBeInTheDocument();
    expect(within(triageRow as HTMLElement).getByText("Custom")).toBeInTheDocument();
  });

  describe("the builtin delete guard", () => {
    it("disables Delete for the builtin role but leaves the custom role's Delete enabled", async () => {
      vi.stubGlobal("fetch", fakeRoleServer([author, triage]).handle);
      renderRoles();
      await screen.findByRole("rowheader", { name: "author" });
      // Both assertions matter: a mutant that hardcodes `disabled` to true or
      // false, or one that inverts `role.builtin`, fails exactly one of them.
      expect(screen.getByRole("button", { name: "Delete author" })).toBeDisabled();
      expect(screen.getByRole("button", { name: "Delete triage" })).toBeEnabled();
    });

    it("does not open the delete confirmation when a disabled Delete button is clicked", async () => {
      const server = fakeRoleServer([author, triage]);
      vi.stubGlobal("fetch", server.handle);
      renderRoles();
      await screen.findByRole("rowheader", { name: "author" });
      fireEvent.click(screen.getByRole("button", { name: "Delete author" }));
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(server.calls.some((call) => call.method === "DeleteRole")).toBe(false);
    });
  });

  it("deletes a non-builtin role after confirming, and the table drops it", async () => {
    const server = fakeRoleServer([author, triage]);
    vi.stubGlobal("fetch", server.handle);
    const user = userEvent.setup();
    renderRoles();
    await screen.findByRole("rowheader", { name: "triage" });
    await user.click(screen.getByRole("button", { name: "Delete triage" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete role" });
    await user.click(within(dialog).getByRole("button", { name: "Delete role" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.queryByRole("rowheader", { name: "triage" })).not.toBeInTheDocument());
    expect(screen.getByRole("rowheader", { name: "author" })).toBeInTheDocument();
    expect(server.getRoles().map((role) => role.name)).toEqual(["author"]);
  });

  it("cancelling the delete confirmation keeps the role and calls no mutation", async () => {
    const server = fakeRoleServer([author, triage]);
    vi.stubGlobal("fetch", server.handle);
    const user = userEvent.setup();
    renderRoles();
    await screen.findByRole("rowheader", { name: "triage" });
    await user.click(screen.getByRole("button", { name: "Delete triage" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete role" });
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByRole("rowheader", { name: "triage" })).toBeInTheDocument();
    expect(server.calls.some((call) => call.method === "DeleteRole")).toBe(false);
  });

  it("creates a role with the checked operations and refreshes the list", async () => {
    const server = fakeRoleServer([author, triage]);
    vi.stubGlobal("fetch", server.handle);
    const user = userEvent.setup();
    renderRoles();
    await screen.findByRole("rowheader", { name: "author" });
    await user.click(screen.getByRole("button", { name: "New role" }));
    const dialog = await screen.findByRole("dialog", { name: "New role" });
    await user.type(within(dialog).getByLabelText("Name", { exact: false }), "scout");
    // Both boxes checked below come from the union of the two seeded roles'
    // operations (git.push, search, work.start) -- the vocabulary this
    // screen can offer at all, given no ListCapabilities-shaped RPC exists.
    await user.click(within(dialog).getByRole("checkbox", { name: "git.push" }));
    await user.click(within(dialog).getByRole("checkbox", { name: "search" }));
    await user.type(within(dialog).getByLabelText("Instructions"), "Recon only.");
    await user.click(within(dialog).getByRole("button", { name: "Create role" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(await screen.findByRole("rowheader", { name: "scout" })).toBeInTheDocument();
    const createCall = server.calls.find((call) => call.method === "CreateRole");
    expect(createCall).toBeDefined();
    const sentRole = createCall?.body["role"] as Partial<FakeRole> | undefined;
    expect(sentRole?.name).toBe("scout");
    expect(sentRole?.builtin ?? false).toBe(false);
    expect([...(sentRole?.operations ?? [])].sort()).toEqual(["git.push", "search"]);
  });

  it("shows the server's rejection inline in the dialog, without closing it", async () => {
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith(".RoleService/ListRoles")) return jsonResponse({ roles: [author] });
      if (url.endsWith(".RoleService/CreateRole")) {
        return jsonResponse(
          { code: "invalid_argument", message: "role name \"bad name\" contains \" \"" },
          400,
        );
      }
      throw new Error(`unhandled fetch: ${url} ${String(init?.body)}`);
    });
    const user = userEvent.setup();
    renderRoles();
    await screen.findByRole("rowheader", { name: "author" });
    await user.click(screen.getByRole("button", { name: "New role" }));
    const dialog = await screen.findByRole("dialog", { name: "New role" });
    await user.type(within(dialog).getByLabelText("Name", { exact: false }), "bad name");
    await user.click(within(dialog).getByRole("button", { name: "Create role" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent('role name "bad name" contains " "');
    expect(screen.getByRole("dialog", { name: "New role" })).toBeInTheDocument();
  });

  it("edits a role's operations and instructions, with its name fixed and read-only", async () => {
    const server = fakeRoleServer([author, triage]);
    vi.stubGlobal("fetch", server.handle);
    const user = userEvent.setup();
    renderRoles();
    await screen.findByRole("rowheader", { name: "triage" });
    await user.click(screen.getByRole("button", { name: "Edit triage" }));
    const dialog = await screen.findByRole("dialog", { name: "Edit triage" });
    const nameField = within(dialog).getByLabelText("Name", { exact: false });
    expect(nameField).toBeDisabled();
    expect(nameField).toHaveValue("triage");
    // triage starts with only "search"; toggle it off and "git.push" on.
    await user.click(within(dialog).getByRole("checkbox", { name: "search" }));
    await user.click(within(dialog).getByRole("checkbox", { name: "git.push" }));
    await user.click(within(dialog).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    const updateCall = [...server.calls].reverse().find((call) => call.method === "UpdateRole");
    expect(updateCall).toBeDefined();
    const sentRole = updateCall?.body["role"] as Partial<FakeRole> | undefined;
    expect(sentRole?.name).toBe("triage");
    expect([...(sentRole?.operations ?? [])]).toEqual(["git.push"]);
    const updatedRow = (await screen.findByRole("rowheader", { name: "triage" })).closest("tr");
    expect(within(updatedRow as HTMLElement).getByText("git.push")).toBeInTheDocument();
  });
});
