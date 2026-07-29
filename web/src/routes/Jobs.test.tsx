import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createRouterTransport, type Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import {
  IngestJobSchema,
  IngestKind,
  IngestStatus,
  RepoAdminService,
} from "../gen/loam/admin/v1/repo_admin_pb";
import { defaultPageLimit } from "../data/pagination";
import { createQueryClient } from "../queryClient";
import {
  formatTimestamp,
  ingestKindLabel,
  Jobs,
  jobRowKey,
  jobsPollIntervalMs,
  jobsRefetchInterval,
} from "./Jobs";

/**
 * Mounts Jobs the way docs/web-frontend-spec.md's DESIGN section asks for:
 * QueryClientProvider + a wouter in-memory Router, over a stubbed transport
 * (`createRouterTransport`, @connectrpc/connect's in-process test double --
 * no real HTTP, no mocked `fetch`). Each call gets its own QueryClient (the
 * same reasoning as src/App.tsx's `AppProviders`: no cache leaking between
 * tests) built from the real `createQueryClient`, so these tests exercise
 * the actual retry/staleTime policy rather than a hand-rolled stand-in.
 */
function renderJobs(transport: Transport): void {
  const queryClient = createQueryClient();
  const location = memoryLocation({ path: "/jobs", record: true });
  render(
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>
        <Router hook={location.hook}>
          <Jobs />
        </Router>
      </QueryClientProvider>
    </TransportProvider>,
  );
}

const emptyMessage = "No ingest jobs match these filters.";

/**
 * Waits for the jobs table itself, rather than for any status word, because
 * every one of "Queued"/"Running"/"Succeeded"/"Failed" is ALSO the visible
 * text of an `<option>` in the always-present Status filter select
 * (rendered regardless of query state) -- `findByText("Running")` resolves
 * against that option instantly, before the real data has loaded, which is
 * exactly the kind of accidentally-vacuous wait this suite has to avoid. The
 * table only renders once the query leaves both the loading and the
 * no-data-error state, so finding it by its caption is an unambiguous signal
 * that real row data is on screen.
 */
const findJobsTable = (): Promise<HTMLElement> => screen.findByRole("table", { name: "Ingest jobs" });

afterEach(() => {
  // Several cases below switch to fake timers to drive the polling gate;
  // restoring here keeps that local to its own test instead of leaking into
  // whichever test runs next in this file.
  vi.useRealTimers();
});

describe("jobsRefetchInterval", () => {
  // Messages are built via create(Schema, {…}), not object literals
  // (docs/web-frontend-spec.md); a plain object satisfies IngestJob's own
  // fields but not the Message base type ($typeName etc.), so these fixtures
  // go through create() like the rest of the app's request/response data.
  const jobWithStatus = (status: IngestStatus) =>
    create(IngestJobSchema, {
      repo: "acme/widgets",
      targetBranch: "main",
      kind: IngestKind.FULL,
      status,
      attempts: 1,
      error: "",
      queuedAt: "2026-07-19T09:00:00Z",
      startedAt: "2026-07-19T09:00:02Z",
      finishedAt: "2026-07-19T09:01:00Z",
    });

  it("polls while a job is queued", () => {
    expect(jobsRefetchInterval([jobWithStatus(IngestStatus.QUEUED)])).toBe(jobsPollIntervalMs);
  });

  it("polls while a job is running", () => {
    expect(jobsRefetchInterval([jobWithStatus(IngestStatus.RUNNING)])).toBe(jobsPollIntervalMs);
  });

  it("stops once every job is terminal", () => {
    expect(
      jobsRefetchInterval([jobWithStatus(IngestStatus.SUCCEEDED), jobWithStatus(IngestStatus.FAILED)]),
    ).toBe(false);
  });

  it("stops on an empty page", () => {
    expect(jobsRefetchInterval([])).toBe(false);
  });

  it("treats the unset zero value as terminal, not as something to keep polling for", () => {
    expect(jobsRefetchInterval([jobWithStatus(IngestStatus.UNSPECIFIED)])).toBe(false);
  });

  it("treats a status this client has never seen as terminal, so an idle tab does not poll forever", () => {
    // Double assertion, not `!` (CLAUDE.md): 999 is a plain number, and no
    // number literal type is assignable to the branded IngestStatus union.
    expect(jobsRefetchInterval([jobWithStatus(999 as unknown as IngestStatus)])).toBe(false);
  });
});

describe("jobRowKey", () => {
  // A render-based "are there two <tr>s" assertion does NOT catch a row key
  // that drops a field: React renders every array element it is given
  // regardless of key collisions on a single pass (it only warns to the
  // console), so two siblings sharing a key still produce two <tr>s here --
  // key collisions only bite on a later *reconciliation*, which a
  // once-rendered test never exercises. The only direct way to prove the
  // key is actually unique is to call it and compare the strings, which is
  // what this does.
  it("differs for two jobs that share everything except queuedAt", () => {
    const base = {
      repo: "acme/widgets",
      targetBranch: "main",
      kind: IngestKind.FULL,
      status: IngestStatus.RUNNING,
      attempts: 1,
      error: "",
      startedAt: "",
      finishedAt: "",
    };
    const first = create(IngestJobSchema, { ...base, queuedAt: "2026-07-20T10:00:00Z" });
    const second = create(IngestJobSchema, { ...base, queuedAt: "2026-07-19T09:00:00Z" });
    expect(jobRowKey(first)).not.toBe(jobRowKey(second));
  });
});

describe("ingestKindLabel", () => {
  // Same source-discovery guard as ./statusIntent.test.ts: IngestKind is an
  // open `as const` object (web/src/gen), so this checks every *named*
  // member the generated file currently declares rather than a hand-picked
  // subset -- a proto regen adding a member fails this test instead of
  // silently falling through `default`.
  const known: ReadonlyArray<readonly [IngestKind, string]> = [
    [IngestKind.INCREMENTAL, "Incremental"],
    [IngestKind.FULL, "Full"],
  ];

  it("covers every named IngestKind member besides UNSPECIFIED", () => {
    const namedValues = Object.entries(IngestKind)
      .filter(([name]) => name !== "UNSPECIFIED")
      .map(([, value]) => value);
    expect(known.map(([value]) => value).sort()).toEqual(namedValues.sort());
  });

  it.each(known)("labels %s as %s", (kind, label) => {
    expect(ingestKindLabel(kind)).toBe(label);
  });

  it("labels the unset zero value distinctly from an unrecognised one", () => {
    expect(ingestKindLabel(IngestKind.UNSPECIFIED)).toBe("Unspecified");
  });

  it("falls back to Unknown for a value outside the generated union", () => {
    expect(ingestKindLabel(999 as unknown as IngestKind)).toBe("Unknown");
  });
});

describe("formatTimestamp", () => {
  it("renders an em dash for a timestamp that has not happened yet", () => {
    expect(formatTimestamp("")).toBe("—");
  });

  it("formats a real RFC 3339 timestamp into something other than the raw wire value", () => {
    const formatted = formatTimestamp("2026-07-19T09:01:00Z");
    expect(formatted).not.toBe("—");
    expect(formatted).not.toBe("2026-07-19T09:01:00Z");
  });

  it("falls back to the raw string for something that does not parse as a date", () => {
    expect(formatTimestamp("not-a-timestamp")).toBe("not-a-timestamp");
  });
});

describe("Jobs", () => {
  it("shows a loading state before the first response arrives", () => {
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        // Never resolves within this test -- the point is only to catch the
        // screen mid-flight, before any response lands.
        listIngestJobs: () => new Promise(() => {}),
      });
    });
    renderJobs(transport);
    expect(screen.getByText("Loading jobs…")).toBeInTheDocument();
  });

  it("renders the empty state when there are no jobs", async () => {
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => ({ jobs: [], pageInfo: { total: 0 } }),
      });
    });
    renderJobs(transport);
    expect(await screen.findByText(emptyMessage)).toBeInTheDocument();
  });

  it("renders two jobs sharing a repo, branch and kind as two distinct, correctly-labelled rows", async () => {
    // repo/targetBranch/kind are identical on purpose -- only queuedAt
    // differs (IngestJob carries no id on the wire; see jobRowKey's
    // comment). This is a rendering-layer companion to the direct
    // `jobRowKey` uniqueness test above, not a substitute for it: a broken
    // row key does NOT cost this test a `<tr>` (React still renders every
    // array element on a single pass, key collisions or not -- it only
    // warns), so what this actually guards is that two same-repo jobs stay
    // independently, correctly labelled next to each other, which a
    // fixture pair that already differed by kind or repo would not
    // meaningfully exercise.
    const shared = {
      repo: "acme/widgets",
      targetBranch: "main",
      kind: IngestKind.FULL,
      attempts: 1,
      error: "",
      startedAt: "",
      finishedAt: "",
    };
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => ({
          jobs: [
            { ...shared, status: IngestStatus.RUNNING, queuedAt: "2026-07-20T10:00:00Z" },
            { ...shared, status: IngestStatus.SUCCEEDED, queuedAt: "2026-07-19T09:00:00Z" },
          ],
          pageInfo: { total: 2 },
        }),
      });
    });
    renderJobs(transport);
    const table = await findJobsTable();
    // getAllByRole("row") includes the header row.
    expect(within(table).getAllByRole("row")).toHaveLength(3);
    expect(within(table).getAllByText("Running")).toHaveLength(1);
    expect(within(table).getAllByText("Succeeded")).toHaveLength(1);
  });

  it("renders the repo as a link to that repo's detail page", async () => {
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => ({
          jobs: [
            {
              repo: "acme/widgets",
              targetBranch: "main",
              kind: IngestKind.FULL,
              status: IngestStatus.RUNNING,
              attempts: 1,
              error: "",
              queuedAt: "2026-07-20T10:00:00Z",
              startedAt: "2026-07-20T10:00:05Z",
              finishedAt: "",
            },
          ],
          pageInfo: { total: 1 },
        }),
      });
    });
    renderJobs(transport);
    const link = await screen.findByRole("link", { name: "acme/widgets" });
    expect(link).toHaveAttribute("href", "/repos/acme/widgets");
  });

  it("renders attempts, an unset timestamp and an error message", async () => {
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => ({
          jobs: [
            {
              repo: "beta/service",
              targetBranch: "develop",
              kind: IngestKind.FULL,
              status: IngestStatus.FAILED,
              attempts: 3,
              error: "clone failed: timeout",
              queuedAt: "2026-07-18T08:00:00Z",
              startedAt: "2026-07-18T08:00:05Z",
              finishedAt: "2026-07-18T08:05:00Z",
            },
          ],
          pageInfo: { total: 1 },
        }),
      });
    });
    renderJobs(transport);
    const table = await findJobsTable();
    expect(within(table).getByText("Failed")).toBeInTheDocument();
    expect(within(table).getByText("3")).toBeInTheDocument();
    expect(within(table).getByText("clone failed: timeout")).toBeInTheDocument();
  });

  it("shows an ErrorBanner with a working Retry when the initial load fails", async () => {
    let calls = 0;
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => {
          calls += 1;
          throw new ConnectError("ingest store unreachable", Code.Internal);
        },
      });
    });
    renderJobs(transport);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("ingest store unreachable");
    expect(calls).toBe(1);
    const user = userEvent.setup();
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(calls).toBe(2));
  });

  it("scopes the query to the submitted repo filter and returns to the first page", async () => {
    const seenRepos: Array<string | undefined> = [];
    const seenOffsets: number[] = [];
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: (req) => {
          seenRepos.push(req.repo);
          seenOffsets.push(req.page?.offset ?? 0);
          return { jobs: [], pageInfo: { total: 0 } };
        },
      });
    });
    renderJobs(transport);
    await screen.findByText(emptyMessage);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Repo"), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Apply filters" }));
    await waitFor(() => expect(seenRepos.at(-1)).toBe("acme/widgets"));
    expect(seenOffsets.at(-1)).toBe(0);
  });

  it("scopes the query to the selected status filter immediately, without Apply", async () => {
    const seenStatuses: Array<IngestStatus | undefined> = [];
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: (req) => {
          seenStatuses.push(req.status);
          return { jobs: [], pageInfo: { total: 0 } };
        },
      });
    });
    renderJobs(transport);
    await screen.findByText(emptyMessage);
    const user = userEvent.setup();
    await user.selectOptions(screen.getByLabelText("Status"), "Failed");
    await waitFor(() => expect(seenStatuses.at(-1)).toBe(IngestStatus.FAILED));
  });

  it("clears both filters back to unscoped", async () => {
    // Deliberately NOT asserted via another captured request: clearing back
    // to the exact (repo: undefined, status: undefined) query the screen
    // already fetched on mount lands inside the client's 5s staleTime
    // (src/queryClient.ts), so TanStack correctly serves it from cache
    // instead of firing a third network call -- asserting a network
    // round-trip here would be asserting an implementation detail the
    // caching layer is entitled to skip. What "Clear filters" promises is
    // that the controls themselves go back to empty/unscoped.
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => ({ jobs: [], pageInfo: { total: 0 } }),
      });
    });
    renderJobs(transport);
    await screen.findByText(emptyMessage);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Repo"), "acme/widgets");
    await user.selectOptions(screen.getByLabelText("Status"), "Failed");
    await user.click(screen.getByRole("button", { name: "Apply filters" }));
    expect(screen.getByLabelText("Repo")).toHaveValue("acme/widgets");
    expect(screen.getByLabelText("Status")).toHaveValue("failed");
    await user.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(screen.getByLabelText("Repo")).toHaveValue("");
    expect(screen.getByLabelText("Status")).toHaveValue("");
  });

  it("disables Reindex repo until a repo is entered", async () => {
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => ({ jobs: [], pageInfo: { total: 0 } }),
      });
    });
    renderJobs(transport);
    await screen.findByText(emptyMessage);
    expect(screen.getByRole("button", { name: "Reindex repo" })).toBeDisabled();
  });

  it("calls ReindexRepo with the typed repo and shows the newly queued job once the list refreshes", async () => {
    // listIngestJobs answers from mutable state that ReindexRepo appends
    // to, so "the queued job shows up" can only be true if
    // useMutationInvalidating actually triggered a refetch after the
    // mutation succeeded -- a call-count assertion would also have to
    // account for the *filter* change Reindex makes (it applies the typed
    // repo as the list filter too, which is its own, separate refetch), so
    // asserting on rendered content is the more direct claim of the two.
    let reindexedRepo: string | undefined;
    let jobs: Array<Record<string, unknown>> = [];
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => ({ jobs, pageInfo: { total: jobs.length } }),
        reindexRepo: (req) => {
          reindexedRepo = req.repo;
          const queued = {
            repo: req.repo,
            targetBranch: "main",
            kind: IngestKind.FULL,
            status: IngestStatus.QUEUED,
            attempts: 0,
            error: "",
            queuedAt: "2026-07-27T00:00:00Z",
            startedAt: "",
            finishedAt: "",
          };
          jobs = [queued];
          return { job: queued };
        },
      });
    });
    renderJobs(transport);
    await screen.findByText(emptyMessage);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Repo"), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Reindex repo" }));
    await waitFor(() => expect(reindexedRepo).toBe("acme/widgets"));
    const table = await findJobsTable();
    // A table "cell" (the status badge), not a "columnheader" -- the
    // "Queued" column header (queued_at) and the QUEUED status badge are
    // both literally the word "Queued", so a plain text query is ambiguous
    // here even scoped to the table.
    expect(within(table).getByRole("cell", { name: "Queued" })).toBeInTheDocument();
  });

  it("shows a dismissible ErrorBanner when ReindexRepo fails", async () => {
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => ({ jobs: [], pageInfo: { total: 0 } }),
        reindexRepo: () => {
          throw new ConnectError("repo not enrolled", Code.NotFound);
        },
      });
    });
    renderJobs(transport);
    await screen.findByText(emptyMessage);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Repo"), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Reindex repo" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("repo not enrolled");
    await user.click(within(alert).getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("pages through results via Pager, requesting the next offset", async () => {
    const seenOffsets: number[] = [];
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: (req) => {
          seenOffsets.push(req.page?.offset ?? 0);
          // More than one page at the default limit, so Pager renders.
          return { jobs: [], pageInfo: { total: defaultPageLimit * 2 + 1 } };
        },
      });
    });
    renderJobs(transport);
    await screen.findByRole("navigation", { name: "Pagination" });
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Go to page 2" }));
    await waitFor(() => expect(seenOffsets.at(-1)).toBe(defaultPageLimit));
  });

  // Fake timers are enabled BEFORE render in both cases below, not after
  // the first response lands: TanStack schedules the interval's
  // `setTimeout` the moment that first response resolves, and a timer
  // created under real timers is invisible to a fake clock started later --
  // advancing fake time would then never fire it, which is a false pass for
  // the wrong reason (nothing to do with the gate itself). Starting fake
  // from before `render` keeps every timer this query ever schedules on the
  // one clock this test controls.  The stub's own response resolves via a
  // plain settled Promise, not a real timer, so
  // `vi.advanceTimersByTimeAsync(0)` -- which also drains the microtask
  // queue -- is enough to observe it without a real wall-clock wait.
  it("polls again after the interval while the loaded page has a running job", async () => {
    let calls = 0;
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => {
          calls += 1;
          return {
            jobs: [
              {
                repo: "acme/widgets",
                targetBranch: "main",
                kind: IngestKind.FULL,
                status: IngestStatus.RUNNING,
                attempts: 1,
                error: "",
                queuedAt: "2026-07-20T10:00:00Z",
                startedAt: "2026-07-20T10:00:05Z",
                finishedAt: "",
              },
            ],
            pageInfo: { total: 1 },
          };
        },
      });
    });
    vi.useFakeTimers();
    renderJobs(transport);
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByRole("table", { name: "Ingest jobs" })).toBeInTheDocument();
    expect(calls).toBe(1);
    await act(() => vi.advanceTimersByTimeAsync(jobsPollIntervalMs));
    expect(calls).toBe(2);
  });

  it("does not poll again once every job on the loaded page is terminal", async () => {
    let calls = 0;
    const transport = createRouterTransport((router) => {
      router.service(RepoAdminService, {
        listIngestJobs: () => {
          calls += 1;
          return {
            jobs: [
              {
                repo: "acme/widgets",
                targetBranch: "main",
                kind: IngestKind.INCREMENTAL,
                status: IngestStatus.SUCCEEDED,
                attempts: 1,
                error: "",
                queuedAt: "2026-07-19T09:00:00Z",
                startedAt: "2026-07-19T09:00:02Z",
                finishedAt: "2026-07-19T09:01:00Z",
              },
            ],
            pageInfo: { total: 1 },
          };
        },
      });
    });
    vi.useFakeTimers();
    renderJobs(transport);
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByRole("table", { name: "Ingest jobs" })).toBeInTheDocument();
    expect(calls).toBe(1);
    // Several intervals' worth of elapsed time -- a single surviving call
    // count rules out an off-by-one as easily as it rules out "never gates".
    await act(() => vi.advanceTimersByTimeAsync(jobsPollIntervalMs * 4));
    expect(calls).toBe(1);
  });
});
