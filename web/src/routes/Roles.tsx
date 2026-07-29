import { create } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import type { ChangeEvent, ReactElement } from "react";
import { useState } from "react";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { ErrorBanner } from "../components/ErrorBanner";
import { Field } from "../components/Field";
import { Form, FormActions } from "../components/Form";
import { Table, type TableColumn } from "../components/Table";
import { useMutationInvalidating } from "../data/invalidation";
import { mapConnectError } from "../data/mapConnectError";
import { RoleSchema, type Role } from "../gen/loam/admin/v1/role_pb";
import {
  createRole,
  deleteRole,
  listRoles,
  updateRole,
} from "../gen/loam/admin/v1/role-RoleService_connectquery";
import styles from "./Roles.module.css";

/**
 * The operation checkboxes need the fixed capability vocabulary
 * (docs/web-spec.md -> RoleService: work.start, work.set, work.request_review,
 * work.reply, work.verdict, work.read, git.clone, git.push, graph.query,
 * search). That vocabulary's single source of truth is
 * `internal/handler.AllCapabilities()` (loam-ofg.13 unified it there after it
 * had drifted across three hand-copied spots) -- but RoleService exposes no
 * RPC that hands it out, so there is nothing generated in `src/gen` to import
 * it from either. Restating the ten strings here would recreate exactly the
 * drift loam-ofg.13 just eliminated, one file later.
 *
 * Instead this derives the checkbox set from what ListRoles actually
 * returned: the union of every role's own `operations`. That is not a
 * workaround by coincidence -- 0001_init.up.sql seeds the two built-in roles
 * (author, reviewer), which cannot be deleted, with operations that between
 * them cover the whole vocabulary, so the union is complete in every real
 * deployment. It degrades honestly rather than silently if that ever stops
 * being true (e.g. a fresh vocabulary member added to AllCapabilities but not
 * yet granted to either built-in): the new operation simply cannot be granted
 * from this screen until some role carries it, which is a real gap -- see the
 * bead's report for the follow-up this should get (a ListCapabilities-shaped
 * RPC, or embedding the vocabulary in GetInstructions/ListRoles some other
 * way) -- not something to paper over with a second hand-copied list.
 */
export function operationVocabulary(roles: readonly Role[]): readonly string[] {
  const seen = new Set<string>();
  for (const role of roles) {
    for (const operation of role.operations) seen.add(operation);
  }
  return [...seen].sort();
}

interface RoleFormValues {
  readonly name: string;
  readonly operations: readonly string[];
  readonly instructions: string;
}

const emptyFormValues: RoleFormValues = { name: "", operations: [], instructions: "" };

/** A mapped Connect outcome reduced to one line of text, for any kind. */
function errorMessage(error: unknown): string {
  const outcome = mapConnectError(error);
  return outcome.kind === "auth-required"
    ? "Your session needs to be renewed. Refresh the page and sign in again."
    : outcome.message;
}

interface RoleFormFieldsProps {
  readonly values: RoleFormValues;
  readonly onChange: (values: RoleFormValues) => void;
  readonly nameDisabled: boolean;
  readonly vocabulary: readonly string[];
}

/** The Name / Operations / Instructions fields shared by the create and edit dialogs. */
function RoleFormFields({ values, onChange, nameDisabled, vocabulary }: RoleFormFieldsProps): ReactElement {
  const handleNameChange = (event: ChangeEvent<HTMLInputElement>): void => {
    onChange({ ...values, name: event.target.value });
  };
  const handleInstructionsChange = (event: ChangeEvent<HTMLTextAreaElement>): void => {
    onChange({ ...values, instructions: event.target.value });
  };
  const toggleOperation = (operation: string): void => {
    const next = values.operations.includes(operation)
      ? values.operations.filter((existing) => existing !== operation)
      : [...values.operations, operation];
    onChange({ ...values, operations: next });
  };
  return (
    <>
      <Field
        label="Name"
        required
        disabled={nameDisabled}
        value={values.name}
        onChange={handleNameChange}
        hint={
          nameDisabled
            ? "A role's name cannot be changed once it is created."
            : "Letters, digits, '-', '_', and '.' only -- it travels in a request header."
        }
      />
      <fieldset className={styles.operations}>
        <legend className={styles.operationsLegend}>Operations</legend>
        {vocabulary.length === 0 ? (
          <p className={styles.operationsEmpty}>
            No operations are known yet -- ListRoles returned none to derive them from.
          </p>
        ) : (
          vocabulary.map((operation) => (
            <label key={operation} className={styles.operationLabel}>
              <input
                type="checkbox"
                checked={values.operations.includes(operation)}
                onChange={() => toggleOperation(operation)}
              />
              <span className={styles.operationName}>{operation}</span>
            </label>
          ))
        )}
      </fieldset>
      <Field
        as="textarea"
        label="Instructions"
        rows={4}
        value={values.instructions}
        onChange={handleInstructionsChange}
        hint="Returned to an agent in this role by `loam instructions`."
      />
    </>
  );
}

const roleColumns = (
  onEdit: (role: Role) => void,
  onDelete: (role: Role) => void,
): readonly TableColumn<Role>[] => [
  { key: "name", header: "Name", cell: (role) => role.name, mono: true, rowHeader: true },
  {
    key: "operations",
    header: "Operations",
    cell: (role) => (role.operations.length > 0 ? role.operations.join(", ") : "None"),
    mono: true,
  },
  { key: "type", header: "Type", cell: (role) => (role.builtin ? "Built-in" : "Custom") },
  {
    key: "instructions",
    header: "Instructions",
    cell: (role) => (role.instructions.trim() === "" ? "—" : role.instructions),
  },
  {
    key: "actions",
    header: "Actions",
    cell: (role) => (
      <div className={styles.rowActions}>
        <Button size="sm" aria-label={`Edit ${role.name}`} onClick={() => onEdit(role)}>
          Edit
        </Button>
        <Button
          size="sm"
          variant="danger"
          aria-label={`Delete ${role.name}`}
          disabled={role.builtin}
          onClick={() => onDelete(role)}
        >
          Delete
        </Button>
      </div>
    ),
  },
];

/**
 * Roles (`/roles`) — the agent role editor (docs/web-frontend-spec.md ->
 * Routing & Screens). Lists every role from `RoleService.ListRoles` and lets
 * the admin create, edit and delete admin-defined roles; built-in roles
 * (author, reviewer) can be edited but never deleted (internal/handler/role.go
 * -> DeleteRole), so their Delete button is natively `disabled` -- genuinely
 * unavailable, not merely `pending` -- and stays out of the tab order.
 */
export function Roles(): ReactElement {
  const rolesQuery = useQuery(listRoles);
  const roles = rolesQuery.data?.roles ?? [];
  const vocabulary = operationVocabulary(roles);

  const [createOpen, setCreateOpen] = useState(false);
  const [createForm, setCreateForm] = useState<RoleFormValues>(emptyFormValues);
  const [editingRole, setEditingRole] = useState<Role | null>(null);
  const [editForm, setEditForm] = useState<RoleFormValues>(emptyFormValues);
  const [deletingRole, setDeletingRole] = useState<Role | null>(null);

  const closeCreateDialog = (): void => setCreateOpen(false);
  const closeEditDialog = (): void => setEditingRole(null);
  const closeDeleteDialog = (): void => setDeletingRole(null);

  const createRoleMutation = useMutationInvalidating(createRole, [{ schema: listRoles }], {
    onSuccess: closeCreateDialog,
  });
  const updateRoleMutation = useMutationInvalidating(updateRole, [{ schema: listRoles }], {
    onSuccess: closeEditDialog,
  });
  const deleteRoleMutation = useMutationInvalidating(deleteRole, [{ schema: listRoles }], {
    onSuccess: closeDeleteDialog,
  });

  const openCreateDialog = (): void => {
    createRoleMutation.reset();
    setCreateForm(emptyFormValues);
    setCreateOpen(true);
  };
  const openEditDialog = (role: Role): void => {
    updateRoleMutation.reset();
    setEditForm({ name: role.name, operations: [...role.operations], instructions: role.instructions });
    setEditingRole(role);
  };
  const openDeleteDialog = (role: Role): void => {
    deleteRoleMutation.reset();
    setDeletingRole(role);
  };

  const handleCreateSubmit = (): void => {
    createRoleMutation.mutate({
      role: create(RoleSchema, {
        name: createForm.name.trim(),
        operations: [...createForm.operations],
        instructions: createForm.instructions,
        builtin: false,
      }),
    });
  };
  const handleEditSubmit = (): void => {
    if (editingRole === null) return;
    updateRoleMutation.mutate({
      role: create(RoleSchema, {
        name: editingRole.name,
        operations: [...editForm.operations],
        instructions: editForm.instructions,
        builtin: false,
      }),
    });
  };
  const handleConfirmDelete = (): void => {
    if (deletingRole === null) return;
    deleteRoleMutation.mutate({ name: deletingRole.name });
  };

  const columns = roleColumns(openEditDialog, openDeleteDialog);

  return (
    <>
      <h1>Roles</h1>
      <div className={styles.toolbar}>
        <Button variant="primary" onClick={openCreateDialog}>
          New role
        </Button>
      </div>

      {rolesQuery.isError ? (
        <ErrorBanner title="Could not load roles" message={errorMessage(rolesQuery.error)}>
          <Button variant="secondary" size="sm" onClick={() => void rolesQuery.refetch()}>
            Retry
          </Button>
        </ErrorBanner>
      ) : rolesQuery.isPending ? (
        <p>Loading roles…</p>
      ) : (
        <Table
          caption="Roles"
          columns={columns}
          rows={roles}
          rowKey={(role) => role.name}
          emptyMessage="No roles configured."
        />
      )}

      <Dialog open={createOpen} title="New role" onClose={closeCreateDialog}>
        {createRoleMutation.isError ? (
          <ErrorBanner title="Could not create role" message={errorMessage(createRoleMutation.error)} />
        ) : null}
        <Form onSubmit={handleCreateSubmit}>
          <RoleFormFields
            values={createForm}
            onChange={setCreateForm}
            nameDisabled={false}
            vocabulary={vocabulary}
          />
          <FormActions>
            <Button type="button" variant="secondary" onClick={closeCreateDialog}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" pending={createRoleMutation.isPending}>
              Create role
            </Button>
          </FormActions>
        </Form>
      </Dialog>

      <Dialog
        open={editingRole !== null}
        title={editingRole === null ? "Edit role" : `Edit ${editingRole.name}`}
        onClose={closeEditDialog}
      >
        {updateRoleMutation.isError ? (
          <ErrorBanner title="Could not update role" message={errorMessage(updateRoleMutation.error)} />
        ) : null}
        <Form onSubmit={handleEditSubmit}>
          <RoleFormFields
            values={editForm}
            onChange={setEditForm}
            nameDisabled={true}
            vocabulary={vocabulary}
          />
          <FormActions>
            <Button type="button" variant="secondary" onClick={closeEditDialog}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" pending={updateRoleMutation.isPending}>
              Save changes
            </Button>
          </FormActions>
        </Form>
      </Dialog>

      <Dialog
        open={deletingRole !== null}
        title="Delete role"
        description={
          deletingRole === null
            ? undefined
            : `This removes "${deletingRole.name}". Any agent presenting it loses every operation it grants.`
        }
        onClose={closeDeleteDialog}
        footer={
          <>
            <Button variant="secondary" onClick={closeDeleteDialog}>
              Cancel
            </Button>
            <Button variant="danger" pending={deleteRoleMutation.isPending} onClick={handleConfirmDelete}>
              Delete role
            </Button>
          </>
        }
      >
        {deleteRoleMutation.isError ? (
          <ErrorBanner title="Could not delete role" message={errorMessage(deleteRoleMutation.error)} />
        ) : (
          <p>This cannot be undone.</p>
        )}
      </Dialog>
    </>
  );
}
