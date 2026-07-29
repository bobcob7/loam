import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createRouterTransport, type Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { AppProviders } from "../App";
import {
  CredentialService,
  CredentialStatusSchema,
  ListCredentialsResponseSchema,
  SetUpstreamTokenResponseSchema,
} from "../gen/loam/admin/v1/credential_pb";
import { Credentials } from "./Credentials";

/**
 * Every case below drives Credentials through a fake `CredentialService`
 * built with `createRouterTransport` (real connect-web wire encoding, no
 * network) rather than `AppProviders`' real, hardcoded transport
 * (src/App.tsx). `AppProviders` is still used for the QueryClient it
 * configures (retry policy, mutation `retry: false`) -- only the transport
 * is overridden, by nesting a second `TransportProvider` inside it, which
 * React context resolves to the nearer one.
 */

const aStatus = (host: string, hasToken: boolean, validated: boolean) =>
  create(CredentialStatusSchema, { host, hasToken, validated });

function renderCredentials(transport: Transport) {
  return render(
    <AppProviders>
      <TransportProvider transport={transport}>
        <Credentials />
      </TransportProvider>
    </AppProviders>,
  );
}

/** A fake CredentialService whose ListCredentials answer can change after a
 * successful SetUpstreamToken, so invalidation can be observed. */
function fakeCredentialTransport(initial: ReturnType<typeof aStatus>[]): Transport {
  let statuses = initial;
  return createRouterTransport((router) => {
    router.service(CredentialService, {
      listCredentials: () => create(ListCredentialsResponseSchema, { statuses }),
      setUpstreamToken: (req) => {
        const next = create(CredentialStatusSchema, { host: req.host, hasToken: true, validated: true });
        statuses = [...statuses.filter((existing) => existing.host !== req.host), next];
        return create(SetUpstreamTokenResponseSchema, { status: next });
      },
    });
  });
}

/**
 * `Code.Internal` -- not `Code.Unavailable` -- so this settles into an error
 * state within the default `waitFor` window. `queryClient.ts`'s
 * `shouldRetryQuery` treats `Unavailable` as transient and retries it twice
 * with backoff (up to ~3s) before giving up; `Internal` is not in that set,
 * so the query fails on the first attempt.
 */
function fakeListErrorTransport(code: Code): Transport {
  return createRouterTransport((router) => {
    router.service(CredentialService, {
      listCredentials: () => {
        throw new ConnectError("server exploded", code);
      },
    });
  });
}

const heading = () => screen.getByRole("heading", { level: 1 });
const openDialogButton = () => screen.getByRole("button", { name: "Set token" });
// Field appends a visually-hidden "*" to a required label's own text node
// (Field.tsx), so the accessible name is "Host*"/"Token*" -- an exact-string
// query would never match either field.
const hostField = () => screen.getByLabelText(/^Host/);
const tokenField = () => screen.getByLabelText(/^Token/);
const saveButton = () => screen.getByRole("button", { name: "Save token" });

describe("Credentials — loading and empty states", () => {
  it("shows a loading message before the first response resolves", () => {
    renderCredentials(fakeCredentialTransport([]));
    expect(heading()).toHaveTextContent("Credentials");
    expect(screen.getByText("Loading credentials…")).toBeInTheDocument();
  });

  it("shows the empty state once ListCredentials resolves with no hosts", async () => {
    renderCredentials(fakeCredentialTransport([]));
    await waitFor(() => expect(screen.getByText("No credentials configured.")).toBeInTheDocument());
  });
});

describe("Credentials — populated table", () => {
  it("renders one row per host, each with its own status label", async () => {
    renderCredentials(
      fakeCredentialTransport([
        aStatus("github.com", true, true),
        aStatus("forgejo.example.com", true, false),
        aStatus("git.internal", false, false),
      ]),
    );
    await waitFor(() => expect(screen.getByText("github.com")).toBeInTheDocument());
    const githubRow = screen.getByRole("row", { name: /github\.com/ });
    expect(within(githubRow).getByText("Validated")).toBeInTheDocument();
    const forgejoRow = screen.getByRole("row", { name: /forgejo\.example\.com/ });
    expect(within(forgejoRow).getByText("Token set, not validated")).toBeInTheDocument();
    const internalRow = screen.getByRole("row", { name: /git\.internal/ });
    expect(within(internalRow).getByText("No token")).toBeInTheDocument();
  });

  it("labels the per-row action by whether a token is already set", async () => {
    renderCredentials(
      fakeCredentialTransport([aStatus("github.com", true, true), aStatus("git.internal", false, false)]),
    );
    await waitFor(() => expect(screen.getByText("github.com")).toBeInTheDocument());
    expect(within(screen.getByRole("row", { name: /github\.com/ })).getByRole("button")).toHaveTextContent(
      "Update token",
    );
    expect(
      within(screen.getByRole("row", { name: /git\.internal/ })).getByRole("button"),
    ).toHaveTextContent("Set token");
  });
});

describe("Credentials — error state", () => {
  it("renders the server's own message in an ErrorBanner when ListCredentials fails", async () => {
    // mapConnectError prefers the server's rawMessage over the generic
    // fallback whenever the server sent one (data/mapConnectError.ts), so a
    // non-empty ConnectError message is what should reach the banner here.
    renderCredentials(fakeListErrorTransport(Code.Internal));
    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    expect(screen.getByRole("alert")).toHaveTextContent("server exploded");
  });

  it("does not render the table alongside the error banner", async () => {
    renderCredentials(fakeListErrorTransport(Code.Internal));
    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });
});

describe("Credentials — Set token dialog", () => {
  it("opens with the host field focused, both fields empty", async () => {
    renderCredentials(fakeCredentialTransport([]));
    fireEvent.click(openDialogButton());
    expect(hostField()).toHaveFocus();
    expect(hostField()).toHaveValue("");
    expect(tokenField()).toHaveValue("");
  });

  it("masks the token field and marks it as a new, not stored, secret", async () => {
    renderCredentials(fakeCredentialTransport([]));
    fireEvent.click(openDialogButton());
    expect(tokenField()).toHaveAttribute("type", "password");
    expect(tokenField()).toHaveAttribute("autocomplete", "new-password");
  });

  it("pre-fills the host from the row's own Update-token action, still editable", async () => {
    renderCredentials(fakeCredentialTransport([aStatus("github.com", true, true)]));
    await waitFor(() => expect(screen.getByText("github.com")).toBeInTheDocument());
    fireEvent.click(within(screen.getByRole("row", { name: /github\.com/ })).getByRole("button"));
    expect(hostField()).toHaveValue("github.com");
    expect(hostField()).not.toHaveAttribute("readonly");
  });

  it("never resurfaces a stale value after Cancel, even across reopening", async () => {
    // Honest note on what this can and cannot prove: `openDialog` and
    // `closeDialog` each independently reset `host`/`token` (Credentials.tsx).
    // Dialog unmounts its children while closed, so the *only* way to
    // observe either reset from the DOM is to reopen -- and reopening's own
    // reset would mask a broken `closeDialog` on its own. This test therefore
    // proves the end-to-end, user-visible guarantee (no stale value ever
    // resurfaces), not that `closeDialog` specifically does the clearing --
    // a mutation deleting just `closeDialog`'s reset passes this test, since
    // `openDialog` still clears on the next open. That reset is kept anyway,
    // as a second line of defence for the token's lifetime in memory while
    // the dialog sits closed, even though no DOM assertion can attribute
    // clearing to it in isolation.
    renderCredentials(fakeCredentialTransport([]));
    fireEvent.click(openDialogButton());
    fireEvent.change(hostField(), { target: { value: "forgejo.example.com" } });
    fireEvent.change(tokenField(), { target: { value: "shhh-secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(openDialogButton());
    expect(hostField()).toHaveValue("");
    expect(tokenField()).toHaveValue("");
  });

  it("submits SetUpstreamToken, closes the dialog, and shows the refreshed status", async () => {
    renderCredentials(fakeCredentialTransport([]));
    fireEvent.click(openDialogButton());
    fireEvent.change(hostField(), { target: { value: "forgejo.example.com" } });
    fireEvent.change(tokenField(), { target: { value: "a-real-token-value" } });
    fireEvent.click(saveButton());
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByText("forgejo.example.com")).toBeInTheDocument());
    expect(within(screen.getByRole("row", { name: /forgejo\.example\.com/ })).getByText("Validated")).toBeInTheDocument();
  });

  it("never renders the submitted token value as text anywhere, before or after saving", async () => {
    // The mutation-that-would-leak-the-token check. Two checkpoints on
    // purpose: `textContent` never includes an <input>'s own `value` (that
    // is an attribute, not a text node), so checking only after the dialog
    // closes -- once `closeDialog` has already wiped `token` back to "" --
    // would miss a leak that only existed transiently while the dialog was
    // still open (e.g. a hint or preview that echoed the typed value as
    // text). Checking mid-session, right after typing, catches that class of
    // bug; checking again after success catches the token surviving into the
    // refreshed table/status pill.
    renderCredentials(fakeCredentialTransport([]));
    fireEvent.click(openDialogButton());
    fireEvent.change(hostField(), { target: { value: "forgejo.example.com" } });
    const secret = "correct-horse-battery-staple-token";
    fireEvent.change(tokenField(), { target: { value: secret } });
    expect(document.body.textContent).not.toContain(secret);
    fireEvent.click(saveButton());
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByText("forgejo.example.com")).toBeInTheDocument());
    expect(document.body.textContent).not.toContain(secret);
    expect(document.body.innerHTML).not.toContain(secret);
  });
});

describe("Credentials — SetUpstreamToken failure", () => {
  it("routes invalid_argument to the Host field, not the ErrorBanner", async () => {
    const transport = createRouterTransport((router) => {
      router.service(CredentialService, {
        listCredentials: () => create(ListCredentialsResponseSchema, { statuses: [] }),
        setUpstreamToken: () => {
          throw new ConnectError("malformed host", Code.InvalidArgument);
        },
      });
    });
    renderCredentials(transport);
    fireEvent.click(openDialogButton());
    fireEvent.change(hostField(), { target: { value: "not a host" } });
    fireEvent.change(tokenField(), { target: { value: "some-token" } });
    fireEvent.click(saveButton());
    await waitFor(() => expect(hostField()).toHaveAttribute("aria-invalid", "true"));
    expect(hostField()).toHaveAccessibleDescription(expect.stringContaining("malformed host"));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("routes every other failure kind to the dialog's ErrorBanner, not the Host field", async () => {
    // Code.Unavailable, deliberately: mutations never retry regardless of
    // code (createQueryClient's `mutations: { retry: false }`), so this
    // exercises a *retryable-for-queries* code specifically to prove the
    // mutation path itself never retries.
    const transport = createRouterTransport((router) => {
      router.service(CredentialService, {
        listCredentials: () => create(ListCredentialsResponseSchema, { statuses: [] }),
        setUpstreamToken: () => {
          throw new ConnectError("down for maintenance", Code.Unavailable);
        },
      });
    });
    renderCredentials(transport);
    fireEvent.click(openDialogButton());
    fireEvent.change(hostField(), { target: { value: "github.com" } });
    fireEvent.change(tokenField(), { target: { value: "some-token" } });
    fireEvent.click(saveButton());
    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    expect(screen.getByRole("alert")).toHaveTextContent("down for maintenance");
    expect(hostField()).not.toHaveAttribute("aria-invalid");
    // The dialog stays open on failure -- the admin's host/token are not lost.
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });
});
