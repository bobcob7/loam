-- Reverses 0004_work_branches_accepted_tip.up.sql: drops accepted_tip,
-- putting ListProposals back to over-including every reviewed, approved,
-- unconflicted branch regardless of its recorded PR (loam-ofg.14's original
-- behavior).

ALTER TABLE work_branches DROP COLUMN IF EXISTS accepted_tip;
