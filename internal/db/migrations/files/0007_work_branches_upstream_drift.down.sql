-- Reverses 0007_work_branches_upstream_drift.up.sql: drops upstream_drift,
-- putting AcceptProposal back to refusing on `conflict` alone and leaving
-- an upstream branch someone rewrote behind Loam's back undetectable again
-- (docs/sync-spec.md "Upstream Drift on `loam/<work-branch>`").
--
-- Nothing else has to be undone with it: a fast-forward adoption writes
-- accepted_tip and a review_rounds row, neither of which this column is
-- part of, and both of which stay correct without it.

ALTER TABLE work_branches DROP COLUMN IF EXISTS upstream_drift;
