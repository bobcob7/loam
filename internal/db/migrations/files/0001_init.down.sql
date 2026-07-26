-- Reverses 0001_init.up.sql: drops the metadata schema group in dependency
-- order (children before parents). The built-in role seed rows are removed
-- along with the roles/role_operations tables themselves.

DROP TABLE IF EXISTS ingest_jobs;
DROP TABLE IF EXISTS comments;
DROP TABLE IF EXISTS threads;
DROP TABLE IF EXISTS verdicts;
DROP TABLE IF EXISTS review_rounds;
DROP TABLE IF EXISTS work_branches;
DROP TABLE IF EXISTS role_operations;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS credentials;
DROP TABLE IF EXISTS repo_target_branches;
DROP TABLE IF EXISTS repos;
