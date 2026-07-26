-- Metadata schema (loam-54o.3), per docs/persistence-spec.md "Metadata".
--
-- This replaces the loam-54o.2 bootstrap no-op ("SELECT 1;"). That bootstrap
-- never applied to any live database (there is no live Postgres anywhere in
-- this project's history yet), so there is no schema_migrations row on earth
-- to preserve; replacing version 1 in place, rather than adding 0002_, avoids
-- a permanent dead history slot. This is the last time 0001 may be replaced:
-- once a real database applies this migration (Demo M1), it is a normal,
-- append-only migration like any other.
--
-- Covers the metadata tables that are the source of truth for repos,
-- credentials, roles, work branches, review rounds/verdicts, threads,
-- comments, and ingest jobs. Derived code-intelligence tables and the
-- pgvector extension belong to loam-54o.4, not here.

CREATE TABLE repos (
    id              uuid PRIMARY KEY,
    name            text NOT NULL,
    upstream_url    text NOT NULL,
    forge_host      text NOT NULL,
    indexed_branch  text NOT NULL,
    sync_state      text NOT NULL DEFAULT 'idle'
                        CONSTRAINT repos_sync_state_check
                        CHECK (sync_state IN ('idle', 'syncing', 'error')),
    last_synced_at  timestamptz,
    sync_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT repos_name_key UNIQUE (name)
);

CREATE TABLE repo_target_branches (
    repo_id             uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    branch              text NOT NULL,
    ingested_ref        text,
    ingested_at         timestamptz,
    ingested_versions   jsonb,
    PRIMARY KEY (repo_id, branch)
);

CREATE TABLE credentials (
    id                  uuid PRIMARY KEY,
    host                text NOT NULL,
    token_ciphertext    bytea,
    validated           boolean NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT credentials_host_key UNIQUE (host)
);

CREATE TABLE roles (
    id              uuid PRIMARY KEY,
    name            text NOT NULL,
    instructions    text NOT NULL DEFAULT '',
    builtin         boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT roles_name_key UNIQUE (name)
);

CREATE TABLE role_operations (
    role_id     uuid NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    operation   text NOT NULL
                    CONSTRAINT role_operations_operation_check
                    CHECK (operation IN (
                        'work.start', 'work.set', 'work.request_review',
                        'work.reply', 'work.verdict', 'work.read',
                        'git.clone', 'git.push', 'graph.query', 'search'
                    )),
    PRIMARY KEY (role_id, operation)
);

CREATE TABLE work_branches (
    id                  uuid PRIMARY KEY,
    repo_id             uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name                text NOT NULL,
    target              text NOT NULL,
    title               text,
    description         text,
    state               text NOT NULL DEFAULT 'draft'
                            CONSTRAINT work_branches_state_check
                            CHECK (state IN ('draft', 'reviewable', 'reviewed', 'complete', 'closed')),
    author              text NOT NULL,
    upstream_pr_url     text,
    upstream_pr_number  integer,
    conflict            text NOT NULL DEFAULT 'none'
                            CONSTRAINT work_branches_conflict_check
                            CHECK (conflict IN ('none', 'flagged', 'reset')),
    close_reason        text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT work_branches_repo_id_name_key UNIQUE (repo_id, name)
);

CREATE TABLE review_rounds (
    id              uuid PRIMARY KEY,
    work_branch_id  uuid NOT NULL REFERENCES work_branches (id) ON DELETE CASCADE,
    number          integer NOT NULL,
    requested_by    text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT review_rounds_work_branch_id_number_key UNIQUE (work_branch_id, number)
);

CREATE TABLE verdicts (
    id          uuid PRIMARY KEY,
    round_id    uuid NOT NULL REFERENCES review_rounds (id) ON DELETE CASCADE,
    reviewer    text NOT NULL,
    outcome     text NOT NULL
                    CONSTRAINT verdicts_outcome_check
                    CHECK (outcome IN ('approve', 'disapprove', 'neutral')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT verdicts_round_id_reviewer_key UNIQUE (round_id, reviewer)
);

CREATE TABLE threads (
    id              uuid PRIMARY KEY,
    work_branch_id  uuid NOT NULL REFERENCES work_branches (id) ON DELETE CASCADE,
    round_id        uuid NOT NULL REFERENCES review_rounds (id) ON DELETE CASCADE,
    author          text NOT NULL,
    file            text,
    line            integer,
    resolved        boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX threads_work_branch_id_idx ON threads (work_branch_id);
CREATE INDEX threads_round_id_idx ON threads (round_id);

CREATE TABLE comments (
    id          uuid PRIMARY KEY,
    thread_id   uuid NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
    round_id    uuid NOT NULL REFERENCES review_rounds (id) ON DELETE CASCADE,
    author      text NOT NULL,
    body        text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX comments_thread_id_idx ON comments (thread_id);
CREATE INDEX comments_round_id_idx ON comments (round_id);

CREATE TABLE ingest_jobs (
    id              uuid PRIMARY KEY,
    repo_id         uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    target_branch   text NOT NULL,
    kind            text NOT NULL
                        CONSTRAINT ingest_jobs_kind_check
                        CHECK (kind IN ('incremental', 'full')),
    status          text NOT NULL DEFAULT 'queued'
                        CONSTRAINT ingest_jobs_status_check
                        CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    attempts        integer NOT NULL DEFAULT 0,
    error           text,
    stats           jsonb,
    queued_at       timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    finished_at     timestamptz
);

CREATE INDEX ingest_jobs_repo_id_idx ON ingest_jobs (repo_id);

-- Built-in roles (docs/web-spec.md -> RoleService), seeded here so they exist
-- from the first migration and cannot be deleted (enforced at the handler
-- layer via roles.builtin, not by the schema).
WITH author_role AS (
    INSERT INTO roles (id, name, instructions, builtin)
    VALUES (gen_random_uuid(), 'author', '', true)
    RETURNING id
), reviewer_role AS (
    INSERT INTO roles (id, name, instructions, builtin)
    VALUES (gen_random_uuid(), 'reviewer', '', true)
    RETURNING id
)
INSERT INTO role_operations (role_id, operation)
SELECT author_role.id, op
FROM author_role, unnest(ARRAY[
    'work.start', 'work.set', 'work.request_review', 'work.reply',
    'git.clone', 'git.push', 'work.read', 'graph.query', 'search'
]) AS op
UNION ALL
SELECT reviewer_role.id, op
FROM reviewer_role, unnest(ARRAY[
    'work.read', 'work.reply', 'work.verdict', 'git.clone', 'graph.query', 'search'
]) AS op;
