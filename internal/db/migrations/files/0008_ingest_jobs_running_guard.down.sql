-- Reverses 0008_ingest_jobs_running_guard.up.sql: drops the partial
-- unique index, putting the single-running-per-repo invariant back to
-- being enforced only by internal/ingestjobs.Store.Claim's own guarded
-- statement (docs/persistence-spec.md "ingest_jobs").
DROP INDEX IF EXISTS ingest_jobs_one_running_per_repo;
