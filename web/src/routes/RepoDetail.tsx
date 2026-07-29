import { useQuery } from "@connectrpc/connect-query";
import type { ChangeEvent, KeyboardEvent, ReactElement } from "react";
import { useEffect, useState } from "react";
import { Link, useLocation } from "wouter";
import { Button } from "../components/Button";
import { CopyField } from "../components/CopyField";
import { Dialog } from "../components/Dialog";
import { ErrorBanner } from "../components/ErrorBanner";
import { Field } from "../components/Field";
import { Form, FormActions } from "../components/Form";
import { StatusBadge } from "../components/StatusBadge";
import { syncStateIntent, workBranchStateIntent } from "../components/statusIntent";
import { Table } from "../components/Table";
import type { TableColumn } from "../components/Table";
import { type ErrorOutcome, mapConnectError } from "../data/mapConnectError";
import { useMutationInvalidating } from "../data/invalidation";
import { getCredentialStatus } from "../gen/loam/admin/v1/credential-CredentialService_connectquery";
import {
  getRepo,
  listRepos,
  removeRepo,
  setTargetBranches,
} from "../gen/loam/admin/v1/repo_admin-RepoAdminService_connectquery";
import { RemovalBlockedSchema, type BlockedWorkBranch } from "../gen/loam/admin/v1/repo_admin_pb";
import styles from "./RepoDetail.module.css";
import { routePatterns } from "./paths";

export interface RepoDetailProps {
  /** The enrolled repo identifier in its wire form, `<group>/<repo_name>`. */
  readonly repo: string;
}

/** The host half of an upstream URL, for CredentialService.GetCredentialStatus. */
function upstreamHost(upstreamUrl: string): string {
  try {
    return new URL(upstreamUrl).host;
  } catch {
    // An unparseable upstream_url is a server-data problem, not a reason to
    // crash the screen: the credential section falls back to "no host".
    return "";
  }
}

/** A generic outcome's message, with the one kind that carries none filled in. */
function outcomeMessage(outcome: ErrorOutcome): string {
  return outcome.kind === "auth-required"
    ? "Authentication is required. Refresh the page to sign in again."
    : outcome.message;
}

const blockerColumns: readonly TableColumn<BlockedWorkBranch>[] = [
  { key: "name", header: "Work branch", mono: true, rowHeader: true, cell: (blocker) => blocker.name },
  { key: "title", header: "Title", cell: (blocker) => blocker.title },
  {
    key: "state",
    header: "State",
    cell: (blocker) => {
      const { intent, label } = workBranchStateIntent(blocker.state);
      return <StatusBadge intent={intent}>{label}</StatusBadge>;
    },
  },
];

/**
 * Repo detail (`/repos/:group/:name`) — target branches, the indexed branch,
 * sync status, and credential status, for one enrolled repo.
 *
 * The identifier arrives as a prop, already rejoined from its two URL
 * segments by the route table (see ./paths.ts). Screens in this directory
 * take plain props rather than calling `useParams` so a screen bead can test
 * one without standing up a router.
 *
 * SetDescriptionSchema does not exist on this screen: the bead's own DESIGN
 * note resolves a conflict between docs/web-spec.md's original RemoveRepo
 * text and this bead's correction in favour of dropping it entirely, along
 * with `has_description_schema` and `has_ssh_key` — credential status here is
 * `has_token`/`validated` only.
 */
export function RepoDetail({ repo }: RepoDetailProps): ReactElement {
  const [, navigate] = useLocation();
  const repoQuery = useQuery(getRepo, { repo });
  const enrolledRepo = repoQuery.data?.repo;
  const host = enrolledRepo === undefined ? "" : upstreamHost(enrolledRepo.upstreamUrl);
  const credentialQuery = useQuery(getCredentialStatus, { host }, { enabled: host !== "" });

  // Undefined means "not yet seeded from the server" -- distinct from `[]`,
  // an admin who has deliberately cleared every branch pending an Add.
  const [branches, setBranches] = useState<string[] | undefined>(undefined);
  const [indexedBranch, setIndexedBranch] = useState<string | undefined>(undefined);
  const [newBranchName, setNewBranchName] = useState("");
  const [removeDialogOpen, setRemoveDialogOpen] = useState(false);

  useEffect(() => {
    if (enrolledRepo === undefined) return;
    setBranches((prev) => prev ?? [...enrolledRepo.targetBranches]);
    setIndexedBranch((prev) => prev ?? (enrolledRepo.indexedBranch === "" ? undefined : enrolledRepo.indexedBranch));
  }, [enrolledRepo]);

  const setTargetBranchesMutation = useMutationInvalidating(setTargetBranches, [{ schema: getRepo }], {
    onSuccess: (data) => {
      const saved = data.repo;
      if (saved === undefined) return;
      setBranches([...saved.targetBranches]);
      setIndexedBranch(saved.indexedBranch === "" ? undefined : saved.indexedBranch);
    },
  });

  const removeRepoMutation = useMutationInvalidating(
    removeRepo,
    [{ schema: getRepo }, { schema: listRepos }],
    {
      onSuccess: () => {
        navigate(routePatterns.repos);
      },
    },
  );

  const addBranch = (): void => {
    const trimmed = newBranchName.trim();
    if (trimmed === "") return;
    setBranches((prev) => {
      const current = prev ?? [];
      return current.includes(trimmed) ? current : [...current, trimmed];
    });
    setIndexedBranch((prev) => prev ?? trimmed);
    setNewBranchName("");
  };

  const removeBranch = (branch: string): void => {
    const next = (branches ?? []).filter((candidate) => candidate !== branch);
    setBranches(next);
    setIndexedBranch((prev) => (prev === branch ? next[0] : prev));
  };

  const handleNewBranchKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    // A tag-input convention: Enter here adds the branch instead of
    // submitting the surrounding Form, which would otherwise fire
    // SetTargetBranches on every keystroke's Enter rather than on Save.
    if (event.key !== "Enter") return;
    event.preventDefault();
    addBranch();
  };

  const canSubmitBranches = (branches?.length ?? 0) > 0 && indexedBranch !== undefined;

  const handleSubmitBranches = (): void => {
    if (!canSubmitBranches || branches === undefined || indexedBranch === undefined) return;
    setTargetBranchesMutation.mutate({ repo, targetBranches: branches, indexedBranch });
  };

  const branchColumns: readonly TableColumn<string>[] = [
    { key: "branch", header: "Branch", mono: true, rowHeader: true, cell: (branch) => branch },
    {
      key: "indexed",
      header: "Indexed",
      cell: (branch) => (
        <input
          type="radio"
          name="indexed-branch"
          aria-label={`Set ${branch} as the indexed branch`}
          checked={branch === indexedBranch}
          onChange={() => setIndexedBranch(branch)}
        />
      ),
    },
    {
      key: "actions",
      header: "Actions",
      cell: (branch) => (
        <Button variant="secondary" size="sm" onClick={() => removeBranch(branch)}>
          Remove {branch}
        </Button>
      ),
    },
  ];

  const branchesMutationOutcome = setTargetBranchesMutation.isError
    ? mapConnectError(setTargetBranchesMutation.error)
    : undefined;

  const removeMutationOutcome = removeRepoMutation.isError
    ? mapConnectError(removeRepoMutation.error)
    : undefined;
  const blockers =
    removeMutationOutcome?.kind === "failed-precondition"
      ? (removeMutationOutcome.cause.findDetails(RemovalBlockedSchema)[0]?.blockers ?? [])
      : [];

  const closeRemoveDialog = (): void => {
    setRemoveDialogOpen(false);
    removeRepoMutation.reset();
  };

  if (repoQuery.isPending) {
    return (
      <>
        <h1>{repo}</h1>
        <p className={styles.muted}>Loading…</p>
      </>
    );
  }

  if (repoQuery.isError) {
    const outcome = mapConnectError(repoQuery.error);
    if (outcome.kind === "not-found") {
      return (
        <>
          <h1>{repo}</h1>
          <p>{outcome.message}</p>
          <Link href={routePatterns.repos}>Go to Repos</Link>
        </>
      );
    }
    return (
      <>
        <h1>{repo}</h1>
        <ErrorBanner title="Could not load repo" message={outcomeMessage(outcome)} />
      </>
    );
  }

  if (enrolledRepo === undefined) {
    return (
      <>
        <h1>{repo}</h1>
        <p>Repo not found.</p>
        <Link href={routePatterns.repos}>Go to Repos</Link>
      </>
    );
  }

  const sync = enrolledRepo.sync;
  const syncIntent = sync === undefined ? undefined : syncStateIntent(sync.state);

  const credentialSection = ((): ReactElement => {
    if (host === "") {
      return <p className={styles.muted}>No upstream host to check credentials for.</p>;
    }
    if (credentialQuery.isPending) {
      return <p className={styles.muted}>Loading credential status…</p>;
    }
    if (credentialQuery.isError) {
      const outcome = mapConnectError(credentialQuery.error);
      return <ErrorBanner title="Could not load credential status" message={outcomeMessage(outcome)} />;
    }
    const status = credentialQuery.data.status;
    if (status === undefined) {
      return <p className={styles.muted}>No credential status returned.</p>;
    }
    return (
      <dl className={styles.statusList}>
        <div>
          <dt>Host</dt>
          <dd className={styles.mono}>{status.host}</dd>
        </div>
        <div>
          <dt>Token</dt>
          <dd>
            <StatusBadge intent={status.hasToken ? "success" : "warning"}>
              {status.hasToken ? "Present" : "Missing"}
            </StatusBadge>
          </dd>
        </div>
        <div>
          <dt>Validated</dt>
          <dd>
            <StatusBadge intent={status.validated ? "success" : "neutral"}>
              {status.validated ? "Yes" : "No"}
            </StatusBadge>
          </dd>
        </div>
      </dl>
    );
  })();

  return (
    <>
      <h1>{repo}</h1>

      <section className={styles.section} aria-labelledby="repo-heading">
        <h2 id="repo-heading" className={styles.sectionTitle}>
          Repo
        </h2>
        <CopyField label="Repo identifier" value={repo} />
        <CopyField label="Upstream URL" value={enrolledRepo.upstreamUrl} />
        {enrolledRepo.ingestedRef !== "" && <CopyField label="Ingested ref" value={enrolledRepo.ingestedRef} />}
        <div className={styles.statusRow}>
          {syncIntent === undefined || sync === undefined ? (
            <p className={styles.muted}>No sync status yet.</p>
          ) : (
            <>
              <StatusBadge intent={syncIntent.intent}>{syncIntent.label}</StatusBadge>
              <p className={styles.muted}>
                {sync.lastSyncedAt === "" ? "Never synced." : `Last synced ${sync.lastSyncedAt}`}
              </p>
              {sync.error !== "" && <p className={styles.errorText}>{sync.error}</p>}
            </>
          )}
        </div>
      </section>

      <section className={styles.section} aria-labelledby="credential-heading">
        <h2 id="credential-heading" className={styles.sectionTitle}>
          Credential status
        </h2>
        {credentialSection}
      </section>

      <section className={styles.section} aria-labelledby="branches-heading">
        <h2 id="branches-heading" className={styles.sectionTitle}>
          Target branches
        </h2>
        <Form onSubmit={handleSubmitBranches}>
          <Table
            caption="Target branches"
            columns={branchColumns}
            rows={branches ?? []}
            rowKey={(branch) => branch}
            emptyMessage="No target branches yet. Add one below."
          />
          <Field
            label="New branch name"
            value={newBranchName}
            onChange={(event: ChangeEvent<HTMLInputElement>) => setNewBranchName(event.target.value)}
            onKeyDown={handleNewBranchKeyDown}
          />
          <FormActions>
            <Button type="button" variant="secondary" onClick={addBranch}>
              Add branch
            </Button>
            <Button type="submit" variant="primary" pending={setTargetBranchesMutation.isPending} disabled={!canSubmitBranches}>
              Save target branches
            </Button>
          </FormActions>
        </Form>
        {branchesMutationOutcome !== undefined && branchesMutationOutcome.kind === "invalid-argument" && (
          <p className={styles.inlineError}>{branchesMutationOutcome.message}</p>
        )}
        {branchesMutationOutcome !== undefined && branchesMutationOutcome.kind !== "invalid-argument" && (
          <ErrorBanner title="Could not update target branches" message={outcomeMessage(branchesMutationOutcome)} />
        )}
      </section>

      <section className={styles.section} aria-labelledby="remove-heading">
        <h2 id="remove-heading" className={styles.sectionTitle}>
          Remove repo
        </h2>
        <p className={styles.muted}>
          Removes the local mirror, graph, and vector data for this repo. Blocked while open work
          branches exist.
        </p>
        <Button variant="danger" onClick={() => setRemoveDialogOpen(true)}>
          Remove repo
        </Button>
      </section>

      <Dialog
        open={removeDialogOpen}
        title="Remove repo"
        description={`This permanently drops the local mirror, graph, and vector data for ${repo}.`}
        onClose={closeRemoveDialog}
        footer={
          <>
            <Button variant="secondary" onClick={closeRemoveDialog}>
              Cancel
            </Button>
            <Button
              variant="danger"
              pending={removeRepoMutation.isPending}
              onClick={() => removeRepoMutation.mutate({ repo })}
            >
              Remove repo
            </Button>
          </>
        }
      >
        {removeMutationOutcome === undefined ? null : removeMutationOutcome.kind === "failed-precondition" ? (
          <div className={styles.blockingPanel}>
            <p className={styles.blockingTitle}>Removal blocked</p>
            {blockers.length > 0 ? (
              <Table
                caption="Work branches blocking removal"
                columns={blockerColumns}
                rows={blockers}
                rowKey={(blocker) => blocker.name}
              />
            ) : (
              <p>{removeMutationOutcome.message}</p>
            )}
          </div>
        ) : (
          <ErrorBanner title="Could not remove repo" message={outcomeMessage(removeMutationOutcome)} />
        )}
      </Dialog>
    </>
  );
}
