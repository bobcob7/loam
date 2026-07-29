import { useMutation, useQuery } from "@connectrpc/connect-query";
import type { ChangeEvent, KeyboardEvent, ReactElement } from "react";
import { useCallback, useId, useRef, useState } from "react";
import { Link } from "wouter";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { ErrorBanner } from "../components/ErrorBanner";
import { Field } from "../components/Field";
import { Form, FormActions } from "../components/Form";
import { Pager } from "../components/Pager";
import { StatusBadge } from "../components/StatusBadge";
import { syncStateIntent } from "../components/statusIntent";
import { Table, type TableColumn } from "../components/Table";
import { useMutationInvalidating } from "../data/invalidation";
import { mapConnectError } from "../data/mapConnectError";
import { toPagerState, useOffsetPagination } from "../data/pagination";
import {
  enrollRepo,
  listRepos,
  probeRepo,
} from "../gen/loam/admin/v1/repo_admin-RepoAdminService_connectquery";
import { SyncState, type EnrolledRepo, type SyncStatus } from "../gen/loam/admin/v1/repo_admin_pb";
import { repoDetailPath } from "./paths";
import styles from "./Repos.module.css";

/**
 * Repos — the default screen (`/`): enrolled repos with sync status and the
 * enroll form (docs/web-frontend-spec.md -> Routing & Screens).
 */
export function Repos(): ReactElement {
  const pagination = useOffsetPagination();
  const reposQuery = useQuery(listRepos, { page: pagination.page });
  const [enrollOpen, setEnrollOpen] = useState(false);

  const columns: readonly TableColumn<EnrolledRepo>[] = [
    {
      key: "repo",
      header: "Repo",
      mono: true,
      rowHeader: true,
      cell: (row) => <Link href={repoDetailPath(row.repo)}>{row.repo}</Link>,
    },
    {
      key: "upstreamUrl",
      header: "Upstream URL",
      cell: (row) => row.upstreamUrl,
    },
    {
      key: "targetBranches",
      header: "Target branches",
      mono: true,
      cell: (row) => row.targetBranches.join(", "),
    },
    {
      key: "indexedBranch",
      header: "Indexed branch",
      mono: true,
      cell: (row) => row.indexedBranch,
    },
    {
      key: "sync",
      header: "Sync",
      cell: (row) => <SyncCell sync={row.sync} />,
    },
    {
      key: "ingestedRef",
      header: "Ingested ref",
      mono: true,
      // Empty until the first ingest completes (repo_admin_pb.ts -> EnrolledRepo.ingested_ref).
      cell: (row) => (row.ingestedRef === "" ? "—" : row.ingestedRef),
    },
  ];

  return (
    <>
      <div className={styles.header}>
        <h1>Repos</h1>
        <Button variant="primary" onClick={() => setEnrollOpen(true)}>
          Enroll repo
        </Button>
      </div>
      {reposQuery.isPending ? (
        <Table
          caption="Enrolled repos"
          columns={columns}
          rows={[]}
          rowKey={(row) => row.repo}
          emptyMessage="Loading repos…"
        />
      ) : reposQuery.isError ? (
        <ListReposError error={reposQuery.error} />
      ) : (
        <>
          <Table
            caption="Enrolled repos"
            columns={columns}
            rows={reposQuery.data.repos}
            rowKey={(row) => row.repo}
            emptyMessage="No repos enrolled."
          />
          {reposQuery.data.pageInfo !== undefined && (
            <Pager
              {...toPagerState(pagination.page, reposQuery.data.pageInfo)}
              onOffsetChange={pagination.setOffset}
              itemNoun="repos"
            />
          )}
        </>
      )}
      {enrollOpen && <EnrollDialog onClose={() => setEnrollOpen(false)} />}
    </>
  );
}

/** The `ListRepos` failure surface: auth gets its own message, everything else a generic banner. */
function ListReposError({ error }: { readonly error: unknown }): ReactElement {
  const outcome = mapConnectError(error);
  if (outcome.kind === "auth-required") {
    return <ErrorBanner title="Authentication required" message="Refresh the page and sign in again." />;
  }
  return <ErrorBanner title="Could not load repos" message={outcome.message} />;
}

/** One repo's sync pill plus its supporting detail: the error when failing, else the last sync time. */
function SyncCell({ sync }: { readonly sync: SyncStatus | undefined }): ReactElement {
  const state = sync?.state ?? SyncState.UNSPECIFIED;
  const { intent, label } = syncStateIntent(state);
  const detail = state === SyncState.ERROR ? sync?.error : sync?.lastSyncedAt;
  return (
    <span className={styles.syncCell}>
      <StatusBadge intent={intent}>{label}</StatusBadge>
      {detail !== undefined && detail !== "" && <span className={styles.syncDetail}>{detail}</span>}
    </span>
  );
}

interface EnrollDialogProps {
  readonly onClose: () => void;
}

/**
 * The enroll form: upstream URL, then `ProbeRepo` on the URL loads a branch
 * picker and pre-fills the indexed branch from upstream HEAD — with manual
 * entry as the fallback if the probe fails (docs/web-frontend-spec.md ->
 * Routing & Screens; loam-nvb.8 NOTES, "the pre-fill gap is resolved").
 *
 * Mounted only while open (`{enrollOpen && <EnrollDialog .../>}` in `Repos`),
 * matching Dialog's own "mounting IS opening" contract: every field here
 * starts fresh because the whole subtree — including this local state —
 * unmounts on close rather than being reset by hand.
 */
function EnrollDialog({ onClose }: EnrollDialogProps): ReactElement {
  const [upstreamUrl, setUpstreamUrl] = useState("");
  const [targetBranches, setTargetBranches] = useState<readonly string[]>([]);
  const [branchInput, setBranchInput] = useState("");
  const [indexedBranch, setIndexedBranch] = useState("");
  const [probedBranches, setProbedBranches] = useState<readonly string[] | null>(null);
  const [probeFailed, setProbeFailed] = useState(false);
  const branchGroupLabelId = useId();
  const branchDatalistId = useId();
  const initialFocusRef = useRef<HTMLElement | null>(null);

  const probeMutation = useMutation(probeRepo);
  const enrollMutation = useMutationInvalidating(enrollRepo, [{ schema: listRepos }], {
    onSuccess: onClose,
  });

  // Field (loam-nvb.5) does not forward a ref to the control it renders, so
  // there is no way to hand Dialog's initialFocusRef the actual <input> by
  // passing it through Field's props. This wrapper queries for it instead,
  // via a ref callback (which fires during commit, before Dialog's own
  // focus effect runs) rather than an effect in this component (which would
  // run after -- see Dialog.tsx's own note on effect ordering).
  const attachInitialFocus = useCallback((node: HTMLDivElement | null): void => {
    if (node === null) return;
    const control = node.querySelector<HTMLElement>("input, select, textarea");
    if (control !== null) initialFocusRef.current = control;
  }, []);

  const handleProbe = (): void => {
    const url = upstreamUrl.trim();
    if (url === "") return;
    probeMutation.mutate(
      { upstreamUrl: url },
      {
        onSuccess: (response) => {
          setProbedBranches(response.branches);
          setProbeFailed(false);
          if (response.head === "") return;
          setIndexedBranch(response.head);
          setTargetBranches((current) =>
            current.includes(response.head) ? current : [...current, response.head],
          );
        },
        onError: () => {
          setProbedBranches(null);
          setProbeFailed(true);
        },
      },
    );
  };

  const addBranch = (): void => {
    const branch = branchInput.trim();
    if (branch === "" || targetBranches.includes(branch)) return;
    setTargetBranches((current) => [...current, branch]);
    setBranchInput("");
  };

  const removeBranch = (branch: string): void => {
    setTargetBranches((current) => current.filter((entry) => entry !== branch));
    setIndexedBranch((current) => (current === branch ? "" : current));
  };

  const handleBranchInputKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key !== "Enter") return;
    event.preventDefault();
    addBranch();
  };

  // There is no typed field-violation detail anywhere in proto/ (mapConnectError.ts),
  // so an invalid_argument from EnrollRepo is rendered next to the form -- on the
  // upstream URL field, the most likely offender -- rather than in a page-level
  // ErrorBanner. Every other failure kind gets the banner instead.
  const enrollOutcome = enrollMutation.error === null ? undefined : mapConnectError(enrollMutation.error);
  const upstreamUrlError = enrollOutcome?.kind === "invalid-argument" ? enrollOutcome.message : undefined;
  const generalErrorMessage =
    enrollOutcome !== undefined && enrollOutcome.kind !== "invalid-argument"
      ? enrollOutcome.kind === "auth-required"
        ? "Refresh the page and sign in again."
        : enrollOutcome.message
      : undefined;

  // No separate targetBranches.length check: the <select> below only ever
  // offers values drawn from targetBranches, and removeBranch clears
  // indexedBranch whenever the branch it names is removed, so
  // indexedBranch !== "" already implies at least one target branch exists.
  const canSubmit = upstreamUrl.trim() !== "" && indexedBranch !== "";

  const handleSubmit = (): void => {
    if (!canSubmit) return;
    enrollMutation.mutate({
      upstreamUrl: upstreamUrl.trim(),
      targetBranches: [...targetBranches],
      indexedBranch,
    });
  };

  return (
    <Dialog open title="Enroll a repo" onClose={onClose} initialFocusRef={initialFocusRef}>
      {generalErrorMessage !== undefined && (
        <ErrorBanner title="Could not enroll repo" message={generalErrorMessage} />
      )}
      <Form aria-label="Enroll a repo" onSubmit={handleSubmit}>
        <div ref={attachInitialFocus}>
          <Field
            label="Upstream URL"
            required
            placeholder="https://forge.example/acme/widgets"
            value={upstreamUrl}
            error={upstreamUrlError}
            hint="Probed automatically when you leave this field, to suggest branches."
            onChange={(event: ChangeEvent<HTMLInputElement>) => setUpstreamUrl(event.target.value)}
            onBlur={handleProbe}
          />
        </div>
        <div className={styles.branchGroup}>
          <span id={branchGroupLabelId} className={styles.branchGroupLabel}>
            Target branches
            <span aria-hidden="true" className={styles.branchGroupRequired}>
              *
            </span>
          </span>
          <div className={styles.branchAddRow}>
            <Field
              label="Add target branch"
              hint="Press Enter or Add branch to include it."
              value={branchInput}
              list={probedBranches !== null ? branchDatalistId : undefined}
              onChange={(event: ChangeEvent<HTMLInputElement>) => setBranchInput(event.target.value)}
              onKeyDown={handleBranchInputKeyDown}
            />
            <Button type="button" onClick={addBranch} disabled={branchInput.trim() === ""}>
              Add branch
            </Button>
          </div>
          {probedBranches !== null && (
            <datalist id={branchDatalistId}>
              {probedBranches.map((branch) => (
                <option key={branch} value={branch} />
              ))}
            </datalist>
          )}
          {probeFailed && (
            <p className={styles.branchHint}>
              Could not probe the upstream. Enter target branches manually.
            </p>
          )}
          <ul aria-labelledby={branchGroupLabelId} className={styles.branchList}>
            {targetBranches.map((branch) => (
              <li key={branch} className={styles.branchChip}>
                <span className={styles.branchChipText}>{branch}</span>
                <button
                  type="button"
                  className={styles.branchChipRemove}
                  aria-label={`Remove ${branch}`}
                  onClick={() => removeBranch(branch)}
                >
                  <span aria-hidden="true">&times;</span>
                </button>
              </li>
            ))}
          </ul>
        </div>
        <Field
          as="select"
          label="Indexed branch"
          required
          disabled={targetBranches.length === 0}
          value={indexedBranch}
          onChange={(event: ChangeEvent<HTMLSelectElement>) => setIndexedBranch(event.target.value)}
        >
          <option value="">Select a branch</option>
          {targetBranches.map((branch) => (
            <option key={branch} value={branch}>
              {branch}
            </option>
          ))}
        </Field>
        <FormActions>
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" pending={enrollMutation.isPending} disabled={!canSubmit}>
            Enroll
          </Button>
        </FormActions>
      </Form>
    </Dialog>
  );
}
