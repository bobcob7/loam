import { create } from "@bufbuild/protobuf";
import { useCallback, useRef, useState, type ReactElement } from "react";
import { useQuery } from "@connectrpc/connect-query";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { ErrorBanner } from "../components/ErrorBanner";
import { Field } from "../components/Field";
import { Form } from "../components/Form";
import { StatusBadge, type StatusIntent } from "../components/StatusBadge";
import { Table, type TableColumn } from "../components/Table";
import { type ErrorOutcome, mapConnectError } from "../data/mapConnectError";
import { useMutationInvalidating } from "../data/invalidation";
import {
  listCredentials,
  setUpstreamToken,
} from "../gen/loam/admin/v1/credential-CredentialService_connectquery";
import {
  type CredentialStatus,
  ListCredentialsRequestSchema,
  SetUpstreamTokenRequestSchema,
} from "../gen/loam/admin/v1/credential_pb";
import styles from "./Credentials.module.css";

/**
 * Credentials (`/credentials`) — per-forge-host token management
 * (docs/web-frontend-spec.md -> Routing & Screens; CredentialService,
 * proto/loam/admin/v1/credential.proto).
 *
 * TOKEN-ONLY, ON PURPOSE: proto/loam/admin/v1/credential.proto's own comment
 * says it plainly -- "One token per host covers both the REST calls that
 * open upstream PRs and git-over-HTTPS transport to the upstream; there is
 * no upstream SSH." `CredentialStatus` reserves field 3 by number only
 * ("formerly the SSH-key presence flag") to keep the retired identifier out
 * of the compiled descriptor. There is no `GenerateSSHKeyPair` RPC anywhere
 * in CredentialService, no `has_ssh_key`, and nothing here renders a public
 * key -- CopyField (built for read-only, non-secret values: a PR URL, an
 * upstream branch, a repo identifier) is correctly not used on this screen.
 *
 * WHAT THIS SCREEN EXPOSES OF A TOKEN: nothing, once it leaves the admin's
 * hands. `SetUpstreamTokenResponse` carries only a `CredentialStatus`
 * (host/has_token/validated) -- the wire contract itself makes the token
 * write-only, there is no response field it could echo back even by
 * accident. The token the admin types lives only in this component's local
 * `token` state, is sent once as `SetUpstreamTokenRequest.token`, and is
 * cleared immediately on both cancel and success -- it is never logged,
 * never rendered as text anywhere (the field itself is `type="password"`,
 * masked, with `autoComplete="new-password"` so a browser offers to save it
 * to a password manager rather than trying to fill it *from* one), and it
 * never enters the TanStack Query cache: the only cached data is
 * `ListCredentialsResponse`/`SetUpstreamTokenResponse`, and neither message
 * has a token field to cache.
 *
 * HOST FORMAT (loam-4kz): this screen sends exactly the `host` string the
 * admin types, verbatim, as `SetUpstreamTokenRequest.host` -- it never
 * reshapes, defaults, or otherwise papers over it. For the default case
 * (an https forge) a bare host ("github.com") is all that has ever been
 * needed. For a plaintext-HTTP, self-hosted forge, the SAME host used for
 * enrollment's Upstream URL must be typed here WITH its "http://" scheme,
 * matching what `RepoAdminService.EnrollRepo` derives from that URL
 * (`internal/handler/repoadmin/handler.go`'s `forgeHostOf`) -- a bare host
 * still validates here (the server retries once over plain HTTP on an
 * unambiguous scheme-mismatch signal, `internal/forge/forgejo.go`'s
 * `ValidateToken`), but it is stored under a different key than a
 * scheme-qualified enrollment would look up, so it would not be found
 * again at `EnrollRepo` time. The `Host` column below renders
 * `CredentialStatus.host` as returned by `ListCredentials`, unmodified.
 */
export function Credentials(): ReactElement {
  const listQuery = useQuery(listCredentials, create(ListCredentialsRequestSchema, {}));
  const [dialogOpen, setDialogOpen] = useState(false);
  const [host, setHost] = useState("");
  const [token, setToken] = useState("");
  const initialFocusRef = useRef<HTMLElement | null>(null);
  const hostFieldWrapperRef = useCallback((node: HTMLDivElement | null) => {
    initialFocusRef.current = node?.querySelector<HTMLElement>("input, textarea, select") ?? null;
  }, []);
  const openDialog = (prefillHost: string): void => {
    setHost(prefillHost);
    setToken("");
    setDialogOpen(true);
  };
  const closeDialog = (): void => {
    setDialogOpen(false);
    setHost("");
    setToken("");
  };
  const mutation = useMutationInvalidating(setUpstreamToken, [{ schema: listCredentials }], {
    onSuccess: closeDialog,
  });
  const handleSubmit = (): void => {
    mutation.mutate(create(SetUpstreamTokenRequestSchema, { host, token }));
  };
  const submitOutcome = mutation.error === null ? undefined : mapConnectError(mutation.error);
  const hostFieldError =
    submitOutcome?.kind === "invalid-argument" ? submitOutcome.message : undefined;
  const bannerMessage =
    submitOutcome !== undefined && submitOutcome.kind !== "invalid-argument"
      ? submitErrorMessage(submitOutcome)
      : undefined;
  const columns: readonly TableColumn<CredentialStatus>[] = [
    { key: "host", header: "Host", mono: true, rowHeader: true, cell: (row) => row.host },
    {
      key: "status",
      header: "Status",
      cell: (row) => {
        const content = credentialIntent(row);
        return <StatusBadge intent={content.intent}>{content.label}</StatusBadge>;
      },
    },
    {
      key: "actions",
      header: "Actions",
      cell: (row) => (
        <Button variant="secondary" size="sm" onClick={() => openDialog(row.host)}>
          {row.hasToken ? "Update token" : "Set token"}
        </Button>
      ),
    },
  ];
  return (
    <div className={styles.root}>
      <div className={styles.header}>
        <h1>Credentials</h1>
        <Button variant="primary" onClick={() => openDialog("")}>
          Set token
        </Button>
      </div>
      {listQuery.isLoading ? <p>Loading credentials…</p> : null}
      {listQuery.isError ? (
        <ErrorBanner
          title="Could not load credentials"
          message={submitErrorMessage(mapConnectError(listQuery.error))}
        >
          <Button variant="secondary" onClick={() => void listQuery.refetch()}>
            Retry
          </Button>
        </ErrorBanner>
      ) : null}
      {!listQuery.isLoading && !listQuery.isError ? (
        <Table
          caption="Credential status by forge host"
          columns={columns}
          rows={listQuery.data?.statuses ?? []}
          rowKey={(row) => row.host}
          emptyMessage="No credentials configured."
        />
      ) : null}
      <Dialog
        open={dialogOpen}
        title="Set upstream token"
        description="One token per forge host, used for both the upstream API and git-over-HTTPS."
        onClose={closeDialog}
        initialFocusRef={initialFocusRef}
      >
        {bannerMessage === undefined ? null : <ErrorBanner message={bannerMessage} />}
        <Form onSubmit={handleSubmit}>
          <div ref={hostFieldWrapperRef}>
            <Field
              as="input"
              label="Host"
              required
              value={host}
              onChange={(event) => setHost(event.target.value)}
              error={hostFieldError}
              hint='Forge host, e.g. "github.com". For a plaintext-HTTP forge, include the scheme: "http://forge.internal:3000".'
            />
          </div>
          <Field
            as="input"
            type="password"
            label="Token"
            required
            value={token}
            onChange={(event) => setToken(event.target.value)}
            autoComplete="new-password"
            hint="Forgejo token or GitHub PAT. Not shown again after saving."
          />
          <div className={styles.actions}>
            <Button variant="secondary" onClick={closeDialog}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" pending={mutation.isPending}>
              Save token
            </Button>
          </div>
        </Form>
      </Dialog>
    </div>
  );
}

/**
 * `has_token`/`validated` are plain booleans (proto/loam/admin/v1/credential.proto),
 * not one of the generated `as const` enums src/components/statusIntent.ts
 * exists for -- there is no "unknown value from a newer server" case to
 * guard against here, so this stays local rather than growing
 * statusIntent.ts a helper for an enum that does not exist.
 */
function credentialIntent(status: CredentialStatus): { intent: StatusIntent; label: string } {
  if (status.validated) return { intent: "success", label: "Validated" };
  if (status.hasToken) return { intent: "warning", label: "Token set, not validated" };
  return { intent: "neutral", label: "No token" };
}

/**
 * Every `ErrorOutcome` kind mapped to banner copy. Exhaustive without a
 * `default` -- unlike a generated proto enum, `ErrorOutcome` is a closed
 * union this codebase defines itself (data/mapConnectError.ts), so there is
 * no "value the frontend has never heard of" case to guard against.
 */
function submitErrorMessage(outcome: ErrorOutcome): string {
  switch (outcome.kind) {
    case "auth-required":
      return "You are signed out. Refresh the page and try again.";
    case "not-allowed":
    case "invalid-argument":
    case "failed-precondition":
    case "not-found":
    case "unexpected":
      return outcome.message;
  }
}
