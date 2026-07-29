import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createRouterTransport, type ConnectRouter } from "@connectrpc/connect";
import { createConnectQueryKey, TransportProvider } from "@connectrpc/connect-query";
import { QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { vi } from "vitest";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import {
  CredentialService,
  CredentialStatusSchema,
  GetCredentialStatusResponseSchema,
} from "../gen/loam/admin/v1/credential_pb";
import { getRepo, listRepos } from "../gen/loam/admin/v1/repo_admin-RepoAdminService_connectquery";
import {
  EnrolledRepoSchema,
  GetRepoResponseSchema,
  RemovalBlockedSchema,
  RemoveRepoResponseSchema,
  RepoAdminService,
  SetTargetBranchesResponseSchema,
  SyncState,
  SyncStatusSchema,
  type EnrolledRepo,
} from "../gen/loam/admin/v1/repo_admin_pb";
import { WorkBranchState } from "../gen/loam/v1/common_pb";
import { createQueryClient } from "../queryClient";
import { RepoDetail } from "./RepoDetail";

/**
 * These render the real screen against `createRouterTransport`
 * (@connectrpc/connect) rather than a hand-rolled fetch stub: `RemoveRepo`'s
 * `RemovalBlocked` detail has to survive a real wire encode/decode for the
 * `findDetails` assertions below to mean anything, and a fetch stub returning
 * canned JSON per test would either fake that round trip or skip it --
 * exactly the kind of shortcut that lets a dropped `findDetails` call pass
 * unnoticed. `createQueryClient()` is used as-is (not a bare `new
 * QueryClient()`) so the real retry/staleTime/mutation policy is exercised,
 * per this bead's own instructions not to re-specify retry per query.
 */

const defaultRepo = "acme/widgets";

const enrolledRepo = (overrides: Partial<EnrolledRepo> = {}): EnrolledRepo =>
  create(EnrolledRepoSchema, {
    repo: defaultRepo,
    upstreamUrl: "https://forge.example.com/acme/widgets.git",
    targetBranches: ["main", "develop"],
    indexedBranch: "main",
    ingestedRef: "abc123",
    sync: create(SyncStatusSchema, {
      state: SyncState.IDLE,
      lastSyncedAt: "2026-07-20T00:00:00Z",
      error: "",
    }),
    ...overrides,
  });

const credentialStatusRoute =
  (overrides: { hasToken?: boolean; validated?: boolean } = {}) =>
  (router: ConnectRouter): void => {
    router.service(CredentialService, {
      getCredentialStatus: async (req) =>
        create(GetCredentialStatusResponseSchema, {
          status: create(CredentialStatusSchema, {
            host: req.host,
            hasToken: overrides.hasToken ?? true,
            validated: overrides.validated ?? true,
          }),
        }),
    });
  };

function renderRepoDetail(
  routes: (router: ConnectRouter) => void,
  options: { readonly repo?: string } = {},
) {
  const repo = options.repo ?? defaultRepo;
  const transport = createRouterTransport(routes);
  const queryClient = createQueryClient();
  const location = memoryLocation({ path: `/repos/${repo}`, record: true });
  render(
    <Router hook={location.hook}>
      <TransportProvider transport={transport}>
        <QueryClientProvider client={queryClient}>
          <RepoDetail repo={repo} />
        </QueryClientProvider>
      </TransportProvider>
    </Router>,
  );
  return { queryClient, location, repo, transport };
}

/** Waits for the repo's data to have landed, using the indexed-branch radio as the signal. */
const waitForLoaded = (): Promise<HTMLElement> =>
  screen.findByRole("radio", { name: "Set main as the indexed branch" });

describe("RepoDetail — loading and error states", () => {
  it("shows the repo heading and a loading state before GetRepo resolves", () => {
    renderRepoDetail((router) => {
      router.service(RepoAdminService, { getRepo: () => new Promise(() => {}) });
    });
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(defaultRepo);
    expect(screen.getByText("Loading…")).toBeInTheDocument();
  });

  it("renders a not-found state with a link back to Repos, not the ErrorBanner", async () => {
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: () => {
          throw new ConnectError("repo acme/widgets is not enrolled", Code.NotFound);
        },
      });
    });
    expect(await screen.findByText("repo acme/widgets is not enrolled")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Go to Repos" })).toHaveAttribute("href", "/");
  });

  it("renders a non-not-found GetRepo failure as the page ErrorBanner", async () => {
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: () => {
          throw new ConnectError("boom", Code.Internal);
        },
      });
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not load repo");
    expect(alert).toHaveTextContent("boom");
    expect(screen.queryByRole("link", { name: "Go to Repos" })).not.toBeInTheDocument();
  });

  it("renders a fixed auth-required message for Unauthenticated, not the server's raw text", async () => {
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: () => {
          throw new ConnectError("invalid credentials", Code.Unauthenticated);
        },
      });
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Authentication is required. Refresh the page to sign in again.");
    expect(alert).not.toHaveTextContent("invalid credentials");
  });
});

describe("RepoDetail — repo, sync, and credential status", () => {
  it("renders upstream URL, ingested ref, sync status, and derives the credential host from upstream_url", async () => {
    let capturedHost: string | undefined;
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () =>
          create(GetRepoResponseSchema, {
            repo: enrolledRepo({ upstreamUrl: "https://git.example.org:8443/acme/widgets.git" }),
          }),
      });
      router.service(CredentialService, {
        getCredentialStatus: async (req) => {
          capturedHost = req.host;
          return create(GetCredentialStatusResponseSchema, {
            status: create(CredentialStatusSchema, { host: req.host, hasToken: true, validated: false }),
          });
        },
      });
    });

    expect(
      await screen.findByDisplayValue("https://git.example.org:8443/acme/widgets.git"),
    ).toBeInTheDocument();
    expect(screen.getByDisplayValue("abc123")).toBeInTheDocument();
    expect(screen.getByText("Idle")).toBeInTheDocument();
    expect(screen.getByText("Last synced 2026-07-20T00:00:00Z")).toBeInTheDocument();
    // host is the scheme+authority only, not the whole URL, a path, or the hostname alone
    // (which would silently drop a non-default port).
    await waitFor(() => expect(capturedHost).toBe("git.example.org:8443"));
    expect(await screen.findByText("Present")).toBeInTheDocument();
    expect(screen.getByText("No")).toBeInTheDocument();
  });

  it("shows the sync error text when SyncState is ERROR", async () => {
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () =>
          create(GetRepoResponseSchema, {
            repo: enrolledRepo({
              sync: create(SyncStatusSchema, {
                state: SyncState.ERROR,
                lastSyncedAt: "",
                error: "clone failed: authentication required",
              }),
            }),
          }),
      });
      credentialStatusRoute()(router);
    });
    expect(await screen.findByText("Error")).toBeInTheDocument();
    expect(screen.getByText("clone failed: authentication required")).toBeInTheDocument();
    expect(screen.getByText("Never synced.")).toBeInTheDocument();
  });

  it("skips GetCredentialStatus entirely when upstream_url has no derivable host", async () => {
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo({ upstreamUrl: "not-a-url" }) }),
      });
      // CredentialService is deliberately NOT registered: if RepoDetail
      // queried it anyway despite the empty host, that request would 404
      // instead of leaving this fallback message in place.
    });
    expect(await screen.findByText("No upstream host to check credentials for.")).toBeInTheDocument();
  });

  it("renders a GetCredentialStatus failure inline without hiding the rest of the screen", async () => {
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }),
      });
      router.service(CredentialService, {
        getCredentialStatus: () => {
          throw new ConnectError("credential store unavailable", Code.Internal);
        },
      });
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not load credential status");
    expect(alert).toHaveTextContent("credential store unavailable");
    expect(screen.getByRole("table", { name: "Target branches" })).toBeInTheDocument();
  });
});

describe("RepoDetail — SetTargetBranches", () => {
  it("saves the edited branch list and the newly selected indexed branch, then re-seeds from the response", async () => {
    const user = userEvent.setup();
    let getRepoCalls = 0;
    let captured: { repo: string; targetBranches: string[]; indexedBranch: string } | undefined;
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => {
          getRepoCalls += 1;
          return create(GetRepoResponseSchema, { repo: enrolledRepo() });
        },
        setTargetBranches: async (req) => {
          captured = {
            repo: req.repo,
            targetBranches: [...req.targetBranches],
            indexedBranch: req.indexedBranch,
          };
          return create(SetTargetBranchesResponseSchema, {
            repo: enrolledRepo({ targetBranches: [...req.targetBranches], indexedBranch: req.indexedBranch }),
          });
        },
      });
      credentialStatusRoute()(router);
    });

    await waitForLoaded();
    await user.click(screen.getByRole("radio", { name: "Set develop as the indexed branch" }));
    await user.click(screen.getByRole("button", { name: "Save target branches" }));

    await waitFor(() => expect(captured).toBeDefined());
    expect(captured).toEqual({
      repo: defaultRepo,
      targetBranches: ["main", "develop"],
      indexedBranch: "develop",
    });
    // GetRepo was invalidated by the mutation's own useMutationInvalidating
    // wiring and refetched -- more than the one call the initial render made.
    await waitFor(() => expect(getRepoCalls).toBeGreaterThanOrEqual(2));
  });

  it("adds a branch and moves focus to it via Enter, without submitting the surrounding Form", async () => {
    const user = userEvent.setup();
    let submitCount = 0;
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }),
        setTargetBranches: async (req) => {
          submitCount += 1;
          return create(SetTargetBranchesResponseSchema, {
            repo: enrolledRepo({ targetBranches: [...req.targetBranches], indexedBranch: req.indexedBranch }),
          });
        },
      });
      credentialStatusRoute()(router);
    });
    await waitForLoaded();
    await user.type(screen.getByLabelText("New branch name"), "release{Enter}");
    const table = screen.getByRole("table", { name: "Target branches" });
    expect(within(table).getByText("release")).toBeInTheDocument();
    expect(submitCount).toBe(0);
  });

  it("does not duplicate a row when the added branch name is already in the list", async () => {
    const user = userEvent.setup();
    renderRepoDetail((router) => {
      router.service(RepoAdminService, { getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }) });
      credentialStatusRoute()(router);
    });
    await waitForLoaded();
    await user.type(screen.getByLabelText("New branch name"), "main{Enter}");
    const table = screen.getByRole("table", { name: "Target branches" });
    expect(within(table).getAllByText("main")).toHaveLength(1);
  });

  it("removes exactly the targeted branch and reassigns the indexed branch when it was removed", async () => {
    const user = userEvent.setup();
    renderRepoDetail((router) => {
      router.service(RepoAdminService, { getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }) });
      credentialStatusRoute()(router);
    });
    await waitForLoaded();
    await user.click(screen.getByRole("button", { name: "Remove main" }));
    const table = screen.getByRole("table", { name: "Target branches" });
    expect(within(table).queryByText("main")).not.toBeInTheDocument();
    expect(within(table).getByText("develop")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Set develop as the indexed branch" })).toBeChecked();
  });

  it("disables Save (native disabled, not merely pending) once every branch is removed", async () => {
    const user = userEvent.setup();
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo({ targetBranches: ["main"], indexedBranch: "main" }) }),
      });
      credentialStatusRoute()(router);
    });
    await waitForLoaded();
    await user.click(screen.getByRole("button", { name: "Remove main" }));
    expect(screen.getByRole("button", { name: "Save target branches" })).toBeDisabled();
  });

  it("marks Save pending (aria-busy, still focusable) while in flight, distinct from native disabled", async () => {
    const user = userEvent.setup();
    let resolveMutation: (() => void) | undefined;
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }),
        setTargetBranches: () =>
          new Promise((resolve) => {
            resolveMutation = () =>
              resolve(create(SetTargetBranchesResponseSchema, { repo: enrolledRepo() }));
          }),
      });
      credentialStatusRoute()(router);
    });
    await waitForLoaded();
    await user.click(screen.getByRole("button", { name: "Save target branches" }));
    const save = screen.getByRole("button", { name: "Save target branches" });
    await waitFor(() => expect(save).toHaveAttribute("aria-busy", "true"));
    expect(save).not.toBeDisabled();
    resolveMutation?.();
    await waitFor(() => expect(save).not.toHaveAttribute("aria-busy"));
  });

  it("renders an invalid_argument SetTargetBranches failure inline, not as the page ErrorBanner", async () => {
    const user = userEvent.setup();
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }),
        setTargetBranches: () => {
          throw new ConnectError("indexed_branch must be a target branch", Code.InvalidArgument);
        },
      });
      credentialStatusRoute()(router);
    });
    await waitForLoaded();
    await user.click(screen.getByRole("button", { name: "Save target branches" }));
    expect(await screen.findByText("indexed_branch must be a target branch")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders a non-invalid_argument SetTargetBranches failure as the page ErrorBanner", async () => {
    const user = userEvent.setup();
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }),
        setTargetBranches: () => {
          throw new ConnectError("forge unreachable", Code.Unavailable);
        },
      });
      credentialStatusRoute()(router);
    });
    await waitForLoaded();
    await user.click(screen.getByRole("button", { name: "Save target branches" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update target branches");
    expect(alert).toHaveTextContent("forge unreachable");
  });
});

describe("RepoDetail — RemoveRepo", () => {
  it("removes the repo, navigates back to Repos, and invalidates GetRepo and ListRepos", async () => {
    const user = userEvent.setup();
    let captured: string | undefined;
    const { queryClient, location } = renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }),
        removeRepo: async (req) => {
          captured = req.repo;
          return create(RemoveRepoResponseSchema, {});
        },
      });
      credentialStatusRoute()(router);
    });

    // GetRepo is a live, mounted query here: `isInvalidated` is a transient
    // flag that an active query's own auto-refetch clears again once it
    // resolves (and navigating away on success unmounts it in the same
    // tick), so asserting on that flag after the fact would race the
    // refetch. Spying on invalidateQueries instead asserts the one thing
    // that is not racy: which schemas useMutationInvalidating told the
    // QueryClient to invalidate.
    const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");

    await waitForLoaded();
    await user.click(screen.getByRole("button", { name: "Remove repo" }));
    const dialog = await screen.findByRole("dialog", { name: "Remove repo" });
    await user.click(within(dialog).getByRole("button", { name: "Remove repo" }));

    await waitFor(() => expect(location.history.at(-1)).toBe("/"));
    expect(captured).toBe(defaultRepo);
    const invalidatedKeys = invalidateSpy.mock.calls.map(([filters]) => filters?.queryKey);
    expect(invalidatedKeys).toContainEqual(createConnectQueryKey({ schema: getRepo, cardinality: undefined }));
    expect(invalidatedKeys).toContainEqual(createConnectQueryKey({ schema: listRepos, cardinality: undefined }));
  });

  it("renders the structured blocking-work-branches panel from the RemovalBlocked detail, not a generic message", async () => {
    const user = userEvent.setup();
    const { location } = renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }),
        removeRepo: () => {
          throw new ConnectError("2 open work branches block removal", Code.FailedPrecondition, undefined, [
            {
              desc: RemovalBlockedSchema,
              value: {
                blockers: [
                  { name: "wb-aaa111", title: "Add retry logic", state: WorkBranchState.REVIEWABLE },
                  { name: "wb-bbb222", title: "Fix flaky test", state: WorkBranchState.DRAFT },
                ],
              },
            },
          ]);
        },
      });
      credentialStatusRoute()(router);
    });

    await waitForLoaded();
    await user.click(screen.getByRole("button", { name: "Remove repo" }));
    const dialog = await screen.findByRole("dialog", { name: "Remove repo" });
    await user.click(within(dialog).getByRole("button", { name: "Remove repo" }));

    // Both blockers render individually -- not just the first, and not
    // collapsed into the raw error message.
    expect(await within(dialog).findByText("wb-aaa111")).toBeInTheDocument();
    expect(within(dialog).getByText("wb-bbb222")).toBeInTheDocument();
    expect(within(dialog).getByText("Add retry logic")).toBeInTheDocument();
    expect(within(dialog).getByText("Fix flaky test")).toBeInTheDocument();
    expect(within(dialog).getByText("Reviewable")).toBeInTheDocument();
    expect(within(dialog).getByText("Draft")).toBeInTheDocument();
    // Distinct from ErrorBanner (docs/web-frontend-spec.md -> Error mapping).
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "Remove repo" })).toBeInTheDocument();
    expect(location.history.at(-1)).not.toBe("/");
  });

  it("falls back to the raw failed_precondition message when no RemovalBlocked detail is attached", async () => {
    const user = userEvent.setup();
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }),
        removeRepo: () => {
          throw new ConnectError("blocked by 2 open work branches", Code.FailedPrecondition);
        },
      });
      credentialStatusRoute()(router);
    });
    await waitForLoaded();
    await user.click(screen.getByRole("button", { name: "Remove repo" }));
    const dialog = await screen.findByRole("dialog", { name: "Remove repo" });
    await user.click(within(dialog).getByRole("button", { name: "Remove repo" }));

    expect(await within(dialog).findByText("blocked by 2 open work branches")).toBeInTheDocument();
    expect(within(dialog).queryByRole("table")).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders a non-failed_precondition RemoveRepo failure as an ErrorBanner inside the dialog", async () => {
    const user = userEvent.setup();
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }),
        removeRepo: () => {
          throw new ConnectError("not an admin", Code.PermissionDenied);
        },
      });
      credentialStatusRoute()(router);
    });
    await waitForLoaded();
    await user.click(screen.getByRole("button", { name: "Remove repo" }));
    const dialog = await screen.findByRole("dialog", { name: "Remove repo" });
    await user.click(within(dialog).getByRole("button", { name: "Remove repo" }));

    const alert = await within(dialog).findByRole("alert");
    expect(alert).toHaveTextContent("Could not remove repo");
    expect(alert).toHaveTextContent("not an admin");
  });

  it("clears a previous RemoveRepo failure when the dialog is cancelled and reopened", async () => {
    const user = userEvent.setup();
    renderRepoDetail((router) => {
      router.service(RepoAdminService, {
        getRepo: async () => create(GetRepoResponseSchema, { repo: enrolledRepo() }),
        removeRepo: () => {
          throw new ConnectError("blocked", Code.FailedPrecondition);
        },
      });
      credentialStatusRoute()(router);
    });
    await waitForLoaded();
    await user.click(screen.getByRole("button", { name: "Remove repo" }));
    let dialog = await screen.findByRole("dialog", { name: "Remove repo" });
    await user.click(within(dialog).getByRole("button", { name: "Remove repo" }));
    await within(dialog).findByText("blocked");

    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Remove repo" }));
    dialog = await screen.findByRole("dialog", { name: "Remove repo" });
    expect(within(dialog).queryByText("blocked")).not.toBeInTheDocument();
  });
});
