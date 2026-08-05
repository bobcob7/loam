-- Enforces "at most one running job per repo" (docs/persistence-spec.md
-- "ingest_jobs"; loam-54o.13's DESIGN) as an actual database constraint,
-- not a property internal/ingestjobs.Store.Claim has to reason its way to
-- correctly under snapshot isolation. A partial unique index on
-- (repo_id) WHERE status = 'running' rejects a second row entering
-- 'running' for a repo that already has one, unconditionally, regardless
-- of what any concurrent transaction's own read snapshot showed at the
-- time it decided to claim -- the "a guard that EXECUTES a rule cannot
-- drift the way one that RESTATES it can" property this project has
-- already paid to learn (bd remember
-- guard-design-ask-dont-model-2026-08).
--
-- This exists because a single-statement, CTE-computed claim could NOT
-- hold the invariant on its own, proven directly by this bead's own
-- concurrency integration tests running under repetition
-- (TestClaim_ConcurrentClaims_SameRepo_OnlyOneWins at -count=200+): two
-- concurrent claims against the SAME repo's two different queued jobs can
-- each compute their own candidate from a snapshot that has not yet
-- observed the other's not-yet-committed claim -- and once each has
-- picked a DIFFERENT row, no single-row recheck on the row actually being
-- written (`AND status = 'queued'`, ClaimIngestJob's own outer UPDATE)
-- defends an invariant that spans two DIFFERENT rows of the same repo.
-- Only a constraint that Postgres itself enforces at write time, checked
-- against the committed state rather than any transaction's snapshot, can
-- close that window. See ClaimIngestJob (internal/db/queries/
-- ingest_jobs.sql) for the retry Store.Claim performs when this index
-- rejects a write.
CREATE UNIQUE INDEX ingest_jobs_one_running_per_repo
    ON ingest_jobs (repo_id)
    WHERE status = 'running';
