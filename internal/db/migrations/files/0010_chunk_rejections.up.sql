-- The per-path rejection ledger (loam-qj21): the durable record of which
-- files the chunk store refused to write, so the next ingest can RETRY
-- them.
--
-- WHY A TABLE AND NOT A COUNTER. loam-2d44 already made the COUNT durable
-- (ingest_jobs.stats.files_rejected, queryable per job). What no counter
-- can carry is WHICH PATHS, and the paths are what the fix needs -- not
-- for reporting, but because the next incremental ingest plans from
-- `git diff ingested_ref..tip` and a rejected file DID NOT CHANGE between
-- those two refs. It appears in neither DropFiles nor ReparseFiles, so it
-- is never reparsed, never re-embedded, never retried. The only thing that
-- can put it back into a plan is a durable list of paths to union in, and
-- that list is this table.
--
-- TWO DIFFERENT FAILURE MODES LIVE IN chunks_state, and the distinction is
-- load-bearing rather than decorative:
--
--   * 'stale'  -- the file was already indexed, the rejection unwound to
--                 its per-file SAVEPOINT (loam-c94.24), and its PRIOR
--                 chunks survived whole. The file is still searchable; it
--                 just serves content from an older commit while
--                 repo_target_branches.ingested_ref claims a newer one.
--   * 'absent' -- there were no prior chunks to survive. Either this was
--                 the file's first ingest, or the ingest was a KindFull
--                 whose repo-scoped DropRepoBranch had already deleted
--                 every chunks row BEFORE the write phase and OUTSIDE the
--                 savepoints, so nothing unwound that delete. The file is
--                 not in RAG search at all.
--
-- 'absent' is the more urgent of the two, and a full rebuild converts
-- 'stale' into 'absent' for any file that rejects again during it. That is
-- not exotic: a Tree-sitter grammar or pipeline version bump escalates
-- every enrolled repo to KindFull. The writer does not branch on plan kind
-- to decide this column -- it counts the file's surviving chunks inside
-- the swap transaction, which observes the KindFull drop and reports
-- 'absent' by itself.
--
-- WHY (repo_id, target_branch, file) IS THE PRIMARY KEY. A rejection is a
-- fact about one path of one branch of one repo, and there is exactly one
-- current answer for it: the ledger records the CURRENT state of that
-- path, not a history of every time it failed. attempts/first_rejected_at
-- carry the history that matters in one row, so a permanently-bad file
-- costs one row rather than one per ingest forever.
--
-- WHY job_id CARRIES NO FOREIGN KEY. It is correlation, not ownership: it
-- names the ingest_jobs row whose stats.files_rejected counted this path,
-- which is the join that did not previously exist (the per-file ERROR log
-- lines carry no job id, so before this the COUNT was joinable to a job
-- and the FILENAMES were not). A real FK would make the ledger's lifetime
-- depend on the jobs table's, and would break every caller that runs the
-- orchestrator against a synthetic job id with no ingest_jobs row -- which
-- includes this repository's own orchestrator integration tests. The
-- column is nullable for the same reason: a ledger row must be writable
-- even when the caller has no job to name.
--
-- WHY THERE IS NO SEPARATE INDEX. Every read is
-- `WHERE repo_id = $1 AND target_branch = $2`, a prefix of the primary
-- key, so the PK's own index already serves it. The table is expected to
-- be empty on a healthy deployment and to hold a handful of rows on an
-- unhealthy one; nothing here is sized for scans.
--
-- THE GUARD IS THE FOREIGN KEY'S OWN CASCADE, not a CREATE ... IF NOT
-- EXISTS. An earlier draft used IF NOT EXISTS to pair with a no-op down
-- migration; that down turned out to be structurally impossible here (the
-- repos foreign key blocks 0001's own down -- see
-- 0010_chunk_rejections.down.sql), so the down drops this table and a
-- plain CREATE TABLE is the honest form. IF NOT EXISTS would now only
-- serve to swallow a genuine "this table already exists" collision, which
-- is a state an operator should hear about rather than have papered over.
CREATE TABLE chunk_rejections (
    repo_id           uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    target_branch     text NOT NULL,
    file              text NOT NULL,

    -- attempts counts how many times this path has been rejected,
    -- including the rejection that created the row. It is the bound on
    -- retrying a file no retry can fix: past a ceiling the writer stops
    -- unioning the path into the plan (see state).
    attempts          integer NOT NULL DEFAULT 1
                          CONSTRAINT chunk_rejections_attempts_check
                          CHECK (attempts > 0),

    -- state is the retry decision, computed from attempts by the writer:
    --   'pending'   -- union this path into the next ingest's plan.
    --   'exhausted' -- stop retrying it. The row DELIBERATELY STAYS: it is
    --                  the only durable record that this path's chunks are
    --                  stale or absent, and deleting it would restore the
    --                  silence this whole table exists to break. An
    --                  exhausted path is still retried whenever it is
    --                  named by a real diff (someone edited it) or by a
    --                  full rebuild, because both of those reach it
    --                  without the ledger's help.
    state             text NOT NULL DEFAULT 'pending'
                          CONSTRAINT chunk_rejections_state_check
                          CHECK (state IN ('pending', 'exhausted')),

    -- chunks_state is what a SEARCHER currently sees for this path; see
    -- the header for why 'stale' and 'absent' are not the same problem.
    chunks_state      text NOT NULL
                          CONSTRAINT chunk_rejections_chunks_state_check
                          CHECK (chunks_state IN ('stale', 'absent')),

    -- sqlstate is Postgres's own five-character code for the rejection
    -- when the error carried one. NULL when the failure was not a
    -- *pgconn.PgError at all, which is what a caller's own non-Postgres
    -- store produces.
    --
    -- The reference case, MEASURED against a real pgvector server rather
    -- than assumed, is a NaN vector coordinate: it raises '22000'
    -- (data_exception), NOT the '22P02' (invalid_text_representation)
    -- loam-qj21's briefing and loam-c94.24's notes both name. 22P02 is
    -- what pgvector's TEXT input function raises, and pgx sends vectors in
    -- the binary format, so that function never runs. See
    -- internal/ingest/vectors.sqlStateOf.
    sqlstate          text,
    error             text NOT NULL,

    job_id            uuid,

    -- rejected_ref is the commit the ingest was writing when this path was
    -- rejected -- i.e. the ref repo_target_branches.ingested_ref advanced
    -- to despite this path not landing. It is what makes the row
    -- self-describing after the fact: "this path's chunks are not the ones
    -- <ref> claims".
    rejected_ref      text NOT NULL,

    first_rejected_at timestamptz NOT NULL DEFAULT now(),
    last_rejected_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (repo_id, target_branch, file)
);
