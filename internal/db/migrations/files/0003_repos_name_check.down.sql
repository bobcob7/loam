-- Reverses 0003_repos_name_check.up.sql: drops the CHECK constraint,
-- leaving repos.name unconstrained at the database layer again (application-
-- level validation in internal/handler/repoadmin's validRepoName is
-- untouched by this migration either way).

ALTER TABLE repos DROP CONSTRAINT IF EXISTS repos_name_check;
