import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { AppProviders } from "../App";
import {
  ListReposResponseSchema,
  ProbeRepoResponseSchema,
  RepoAdminService,
  SyncState,
  type EnrollRepoRequest,
  type EnrolledRepo,
} from "../gen/loam/admin/v1/repo_admin_pb";
import { PageInfoSchema } from "../gen/loam/v1/common_pb";
import { enrolledRepoFixture as repoFixture, syncStatusFixture } from "../test/fixtures";
import { Repos } from "./Repos";

// Queried by role/label throughout: getByRole("link", { name }) only passes
// for a real row link, getByRole("row"/"rowheader") only for real table
// semantics, and toHaveFocus only for a genuinely focused element. None of
// this could be faked by a getByTestId or a snapshot.

interface RenderOptions {
  /** Seeds the fake service's repo list. Defaults to a single fixture repo. */
  readonly initialRepos?: readonly EnrolledRepo[];
  /** Overrides the default paginated-slice behaviour entirely, e.g. to simulate a failure. */
  readonly listRepos?: (page: { readonly limit: number; readonly offset: number }) => {
    repos: EnrolledRepo[];
    total: number;
  };
  /** Overrides the default append-and-return behaviour, e.g. to simulate a failure. */
  readonly enrollRepo?: (req: EnrollRepoRequest) => EnrolledRepo;
  readonly probeRepo?: (url: string) => { branches: string[]; head: string };
  /** Awaited before EnrollRepo resolves, so a test can observe the in-flight state. */
  readonly enrollRepoGate?: Promise<void>;
}

/**
 * Mounts Repos with a stubbed RepoAdminService (a real, if minimal, fake: it
 * keeps its own repo list, so `EnrollRepo` followed by `ListRepos`'s
 * invalidation-driven refetch actually round-trips through server state
 * instead of two independently-scripted responses) and a memory router.
 */
function renderRepos(options: RenderOptions = {}) {
  const enrollCalls: EnrollRepoRequest[] = [];
  const repos: EnrolledRepo[] = options.initialRepos !== undefined ? [...options.initialRepos] : [repoFixture()];
  const transport = createRouterTransport((router) => {
    router.service(RepoAdminService, {
      listRepos: async (req) => {
        const limit = req.page?.limit ?? 25;
        const offset = req.page?.offset ?? 0;
        const { repos: page, total } = options.listRepos?.({ limit, offset }) ?? {
          repos: repos.slice(offset, offset + limit),
          total: repos.length,
        };
        return create(ListReposResponseSchema, { repos: page, pageInfo: create(PageInfoSchema, { total }) });
      },
      enrollRepo: async (req) => {
        enrollCalls.push(req);
        if (options.enrollRepoGate !== undefined) await options.enrollRepoGate;
        if (options.enrollRepo !== undefined) {
          return { repo: options.enrollRepo(req) };
        }
        const repo = repoFixture({
          repo: "acme/new",
          upstreamUrl: req.upstreamUrl,
          targetBranches: req.targetBranches,
          indexedBranch: req.indexedBranch,
        });
        repos.push(repo);
        return { repo };
      },
      probeRepo: async (req) => {
        if (options.probeRepo === undefined) {
          throw new ConnectError("no upstream reachable", Code.Unavailable);
        }
        const result = options.probeRepo(req.upstreamUrl);
        return create(ProbeRepoResponseSchema, { branches: result.branches, head: result.head });
      },
    });
  });
  const location = memoryLocation({ path: "/", record: true });
  render(
    <AppProviders>
      <TransportProvider transport={transport}>
        <Router hook={location.hook}>
          <Repos />
        </Router>
      </TransportProvider>
    </AppProviders>,
  );
  return { location, enrollCalls };
}

/** Data rows only, keyed by their row-header (repo) text, excluding the header row. */
const rowHeaders = (): readonly string[] =>
  screen.getAllByRole("rowheader").map((cell) => cell.textContent ?? "");

describe("loading", () => {
  it("shows a loading row before the first response resolves", () => {
    renderRepos();
    expect(screen.getByRole("cell", { name: "Loading repos…" })).toBeInTheDocument();
  });
});

describe("listing repos", () => {
  it("renders a row per enrolled repo, linking the repo cell to its detail screen", async () => {
    renderRepos({ listRepos: () => ({ repos: [repoFixture()], total: 1 }) });
    const link = await screen.findByRole("link", { name: "acme/widgets" });
    expect(link).toHaveAttribute("href", "/repos/acme/widgets");
  });

  it("renders the corrected field set as columns, in order, under an accessible table name", async () => {
    renderRepos();
    await screen.findByRole("link", { name: "acme/widgets" });
    expect(screen.getByRole("table", { name: "Enrolled repos" })).toBeInTheDocument();
    expect(screen.getAllByRole("columnheader").map((cell) => cell.textContent)).toEqual([
      "Repo",
      "Upstream URL",
      "Target branches",
      "Indexed branch",
      "Sync",
      "Ingested ref",
    ]);
  });

  it("joins multiple target branches for display", async () => {
    renderRepos({
      listRepos: () => ({ repos: [repoFixture({ targetBranches: ["main", "release"] })], total: 1 }),
    });
    await screen.findByRole("link", { name: "acme/widgets" });
    expect(screen.getByRole("cell", { name: "main, release" })).toBeInTheDocument();
  });

  it("renders an em dash for a repo with no ingest yet, rather than a blank cell", async () => {
    renderRepos({ listRepos: () => ({ repos: [repoFixture({ ingestedRef: "" })], total: 1 }) });
    await screen.findByRole("link", { name: "acme/widgets" });
    expect(screen.getByRole("cell", { name: "—" })).toBeInTheDocument();
  });

  it("falls back to a neutral, unspecified pill for a repo with no sync status at all", async () => {
    // `EnrolledRepo.sync` is optional in the generated TS even though a real
    // server always sets it; this is the defensive fallback for that gap,
    // and it must read as "unspecified", not silently as an error.
    renderRepos({ listRepos: () => ({ repos: [repoFixture({ sync: undefined })], total: 1 }) });
    await screen.findByRole("link", { name: "acme/widgets" });
    expect(screen.getByText("Unspecified")).toBeInTheDocument();
    expect(screen.queryByText("Error")).not.toBeInTheDocument();
  });

  it("shows the sync error instead of the last-synced time when sync is failing", async () => {
    renderRepos({
      listRepos: () => ({
        repos: [
          repoFixture({
            sync: syncStatusFixture({
              state: SyncState.ERROR,
              lastSyncedAt: "",
              error: "mirror unreachable",
            }),
          }),
        ],
        total: 1,
      }),
    });
    await screen.findByRole("link", { name: "acme/widgets" });
    expect(screen.getByText("Error")).toBeInTheDocument();
    expect(screen.getByText("mirror unreachable")).toBeInTheDocument();
    expect(screen.queryByText("2026-07-20T10:00:00Z")).not.toBeInTheDocument();
  });

  it("shows the last-synced time, not an error, when sync is idle", async () => {
    renderRepos();
    await screen.findByRole("link", { name: "acme/widgets" });
    expect(screen.getByText("Idle")).toBeInTheDocument();
    expect(screen.getByText("2026-07-20T10:00:00Z")).toBeInTheDocument();
  });

  it("labels a currently-syncing repo distinctly from idle and error", async () => {
    renderRepos({
      listRepos: () => ({
        repos: [
          repoFixture({ sync: syncStatusFixture({ state: SyncState.SYNCING, lastSyncedAt: "", error: "" }) }),
        ],
        total: 1,
      }),
    });
    await screen.findByRole("link", { name: "acme/widgets" });
    expect(screen.getByText("Syncing")).toBeInTheDocument();
  });

  it("pages through results via Pager, refetching the next slice", async () => {
    const user = userEvent.setup();
    const allRepos = Array.from({ length: 30 }, (_, index) =>
      repoFixture({ repo: `acme/repo-${String(index).padStart(2, "0")}` }),
    );
    renderRepos({ initialRepos: allRepos });
    await screen.findByRole("link", { name: "acme/repo-00" });
    expect(rowHeaders()).toHaveLength(25);
    expect(screen.getByRole("status")).toHaveTextContent("Page 1 of 2");

    await user.click(screen.getByRole("button", { name: "Go to page 2" }));
    await screen.findByRole("link", { name: "acme/repo-25" });
    expect(rowHeaders()).toHaveLength(5);
    expect(screen.getByRole("status")).toHaveTextContent("Page 2 of 2");
    // The first page's rows are gone, not merely appended to.
    expect(screen.queryByRole("link", { name: "acme/repo-00" })).not.toBeInTheDocument();
  });
});

describe("empty state", () => {
  it("shows the empty message and keeps the Enroll action available", async () => {
    renderRepos({ listRepos: () => ({ repos: [], total: 0 }) });
    expect(await screen.findByRole("cell", { name: "No repos enrolled." })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Enroll repo" })).toBeInTheDocument();
  });
});

describe("error state", () => {
  it("renders an auth-required message on unauthenticated, not a raw error banner", async () => {
    renderRepos({
      listRepos: () => {
        throw new ConnectError("nope", Code.Unauthenticated);
      },
    });
    expect(await screen.findByText("Authentication required")).toBeInTheDocument();
    expect(screen.getByText("Refresh the page and sign in again.")).toBeInTheDocument();
  });

  it("renders a generic banner with the server's message for other failures", async () => {
    // Code.Internal (unlike Code.Unavailable) is not in queryClient.ts's
    // retryableCodes, so this reaches the error state on the first attempt
    // instead of after two retries' worth of backoff.
    renderRepos({
      listRepos: () => {
        throw new ConnectError("index corrupted", Code.Internal);
      },
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("index corrupted");
    expect(screen.getByText("Could not load repos")).toBeInTheDocument();
  });
});

describe("enroll dialog", () => {
  it("opens on the Enroll button and focuses the upstream URL field", async () => {
    const user = userEvent.setup();
    renderRepos();
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    expect(screen.getByRole("dialog", { name: "Enroll a repo" })).toBeInTheDocument();
    expect(screen.getByLabelText(/^Upstream URL/)).toHaveFocus();
  });

  it("keeps Enroll disabled until a URL, at least one branch, and an indexed branch are set", async () => {
    const user = userEvent.setup();
    renderRepos();
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    const submit = screen.getByRole("button", { name: "Enroll" });
    expect(submit).toBeDisabled();

    await user.type(screen.getByLabelText(/^Upstream URL/), "https://forge.example/acme/new");
    expect(submit).toBeDisabled();

    await user.type(screen.getByLabelText("Add target branch"), "main");
    await user.click(screen.getByRole("button", { name: "Add branch" }));
    expect(submit).toBeDisabled();

    await user.selectOptions(screen.getByLabelText(/^Indexed branch/), "main");
    expect(submit).toBeEnabled();
  });

  it("probes the upstream on blur, pre-filling target branches and the indexed branch from HEAD", async () => {
    const user = userEvent.setup();
    renderRepos({ probeRepo: () => ({ branches: ["main", "release"], head: "main" }) });
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    await user.type(screen.getByLabelText(/^Upstream URL/), "https://forge.example/acme/new");
    await user.tab();

    // "main" now appears twice (the chip and the indexed-branch <option>), so
    // the chip's own remove button -- not a text lookup -- is what proves it
    // landed in the target-branches list rather than only in the select.
    expect(await screen.findByRole("button", { name: "Remove main" })).toBeInTheDocument();
    expect(screen.getByLabelText(/^Indexed branch/)).toHaveValue("main");
  });

  it("adds and removes branches from the list manually", async () => {
    const user = userEvent.setup();
    renderRepos();
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    const branchInput = screen.getByLabelText("Add target branch");

    await user.type(branchInput, "main");
    await user.click(screen.getByRole("button", { name: "Add branch" }));
    expect(screen.getByRole("button", { name: "Remove main" })).toBeInTheDocument();
    expect(branchInput).toHaveValue("");

    await user.type(branchInput, "release{Enter}");
    expect(screen.getByRole("button", { name: "Remove release" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Remove main" }));
    expect(screen.queryByRole("button", { name: "Remove main" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove release" })).toBeInTheDocument();
  });

  it("does not add a second entry when the same branch is added twice", async () => {
    const user = userEvent.setup();
    renderRepos();
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    const branchInput = screen.getByLabelText("Add target branch");

    await user.type(branchInput, "main");
    await user.click(screen.getByRole("button", { name: "Add branch" }));
    await user.type(branchInput, "main");
    await user.click(screen.getByRole("button", { name: "Add branch" }));

    expect(screen.getAllByRole("button", { name: "Remove main" })).toHaveLength(1);
  });

  it("constrains the indexed-branch select to the branches actually entered", async () => {
    const user = userEvent.setup();
    renderRepos();
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    const select = screen.getByLabelText(/^Indexed branch/) as HTMLSelectElement;
    expect(select).toBeDisabled();
    expect(within(select).getAllByRole("option")).toHaveLength(1);

    await user.type(screen.getByLabelText("Add target branch"), "main");
    await user.click(screen.getByRole("button", { name: "Add branch" }));
    expect(select).toBeEnabled();
    expect(within(select).getAllByRole("option").map((option) => option.textContent)).toEqual([
      "Select a branch",
      "main",
    ]);
  });

  it("shows a fallback hint, without blocking manual entry, when the probe fails", async () => {
    const user = userEvent.setup();
    renderRepos();
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    await user.type(screen.getByLabelText(/^Upstream URL/), "https://forge.example/acme/new");
    await user.tab();

    expect(await screen.findByText("Could not probe the upstream. Enter target branches manually.")).toBeInTheDocument();
    await user.type(screen.getByLabelText("Add target branch"), "main");
    await user.click(screen.getByRole("button", { name: "Add branch" }));
    expect(screen.getByRole("button", { name: "Remove main" })).toBeInTheDocument();
  });

  it("submits EnrollRepo with the entered fields, closes, and refreshes the list", async () => {
    const user = userEvent.setup();
    const { enrollCalls } = renderRepos();
    await screen.findByRole("link", { name: "acme/widgets" });
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    await user.type(screen.getByLabelText(/^Upstream URL/), "https://forge.example/acme/new");
    await user.type(screen.getByLabelText("Add target branch"), "main");
    await user.click(screen.getByRole("button", { name: "Add branch" }));
    await user.selectOptions(screen.getByLabelText(/^Indexed branch/), "main");

    await user.click(screen.getByRole("button", { name: "Enroll" }));

    expect(await screen.findByRole("link", { name: "acme/new" })).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(enrollCalls).toEqual([
      {
        $typeName: "loam.admin.v1.EnrollRepoRequest",
        upstreamUrl: "https://forge.example/acme/new",
        targetBranches: ["main"],
        indexedBranch: "main",
      },
    ]);
  });

  it("marks the submit button busy while EnrollRepo is in flight, without dropping it from the tab order", async () => {
    const user = userEvent.setup();
    let resolveGate: () => void = () => {};
    const gate = new Promise<void>((resolve) => {
      resolveGate = resolve;
    });
    renderRepos({ enrollRepoGate: gate });
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    await user.type(screen.getByLabelText(/^Upstream URL/), "https://forge.example/acme/new");
    await user.type(screen.getByLabelText("Add target branch"), "main");
    await user.click(screen.getByRole("button", { name: "Add branch" }));
    await user.selectOptions(screen.getByLabelText(/^Indexed branch/), "main");

    const submit = screen.getByRole("button", { name: "Enroll" });
    await user.click(submit);

    // aria-busy/aria-disabled, not native disabled: a submit button that
    // leaves the tab order the moment it is pressed drops keyboard focus to
    // <body> (Button.tsx's own note on disabled-vs-pending).
    expect(submit).toHaveAttribute("aria-busy", "true");
    expect(submit).toHaveAttribute("aria-disabled", "true");
    expect(submit).not.toBeDisabled();

    resolveGate();
    expect(await screen.findByRole("link", { name: "acme/new" })).toBeInTheDocument();
  });

  it("renders an invalid_argument failure as an inline field error, not a page banner", async () => {
    const user = userEvent.setup();
    renderRepos({
      enrollRepo: () => {
        throw new ConnectError("upstream_url must be an absolute URL", Code.InvalidArgument);
      },
    });
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    await user.type(screen.getByLabelText(/^Upstream URL/), "not-a-url");
    await user.type(screen.getByLabelText("Add target branch"), "main");
    await user.click(screen.getByRole("button", { name: "Add branch" }));
    await user.selectOptions(screen.getByLabelText(/^Indexed branch/), "main");
    await user.click(screen.getByRole("button", { name: "Enroll" }));

    const urlField = await screen.findByLabelText(/^Upstream URL/);
    expect(urlField).toHaveAccessibleDescription(
      expect.stringContaining("upstream_url must be an absolute URL"),
    );
    expect(urlField).toHaveAttribute("aria-invalid", "true");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("renders a non-field failure (e.g. failed_precondition) as a page-level banner inside the dialog", async () => {
    const user = userEvent.setup();
    renderRepos({
      enrollRepo: () => {
        throw new ConnectError("repo already enrolled", Code.FailedPrecondition);
      },
    });
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    await user.type(screen.getByLabelText(/^Upstream URL/), "https://forge.example/acme/new");
    await user.type(screen.getByLabelText("Add target branch"), "main");
    await user.click(screen.getByRole("button", { name: "Add branch" }));
    await user.selectOptions(screen.getByLabelText(/^Indexed branch/), "main");
    await user.click(screen.getByRole("button", { name: "Enroll" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("repo already enrolled");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.getByLabelText(/^Upstream URL/)).not.toHaveAttribute("aria-invalid");
  });

  it("closes without submitting when Cancel is pressed", async () => {
    const user = userEvent.setup();
    const { enrollCalls } = renderRepos();
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    await user.type(screen.getByLabelText(/^Upstream URL/), "https://forge.example/acme/new");
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(enrollCalls).toEqual([]);
  });

  it("starts fresh on reopen, rather than keeping the previous session's fields", async () => {
    const user = userEvent.setup();
    renderRepos();
    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    await user.type(screen.getByLabelText(/^Upstream URL/), "https://forge.example/leftover");
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    await user.click(screen.getByRole("button", { name: "Enroll repo" }));
    expect(screen.getByLabelText(/^Upstream URL/)).toHaveValue("");
  });
});
