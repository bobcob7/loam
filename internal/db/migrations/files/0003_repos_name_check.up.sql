-- Adds a DB-level CHECK constraint on repos.name mirroring
-- internal/handler/repoadmin's validRepoName (loam-ofg.12), which validates
-- this exact shape at the EnrollRepo write path but leaves the column itself
-- unconstrained: a future write path that bypasses EnrollRepo (a direct
-- INSERT, a migration backfill, a different bead) could otherwise still
-- write an invalid name straight into the table. This closes the
-- loam-ofg.16-review gap ("..%2f traversal reaching a filesystem path
-- because repos.name has NO CHECK constraint") at the database layer too.
--
-- A repo identifier is "<group>/<name>" (docs/persistence-spec.md): exactly
-- two '/'-delimited segments, each starting with an alphanumeric and
-- containing only alphanumerics, '.', '_', or '-'. The regex below is that
-- same allowlist applied twice around exactly one '/' -- neither segment's
-- character class includes '/', so this also rejects an extra slash
-- ("acme//evil", "acme/sub/widgets") by construction, matching
-- validRepoName's two-segment-only behavior.
--
-- No existing migration inserts into repos (0001_init.up.sql's only seed
-- data is the built-in roles/role_operations rows), so this is a plain
-- CHECK -- no NOT VALID / VALIDATE CONSTRAINT staging is needed against a
-- freshly migrated, pre-1.0 database.
ALTER TABLE repos
    ADD CONSTRAINT repos_name_check
    CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$');
