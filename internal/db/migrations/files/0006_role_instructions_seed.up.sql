-- Fills roles.instructions for the built-in 'author' and 'reviewer' roles
-- (loam-0pj.17) with real role policy, replacing the empty string
-- 0001_init.up.sql seeded them with.
--
-- WHY THIS EXISTS: the column, the query (UpdateRoleInstructions,
-- queries/roles.sql), and the RPC (loam.v1.MetaService.GetInstructions,
-- returned as role_instructions) were already fully implemented -- nothing
-- was missing from the mechanism. What was missing was CONTENT: an empty
-- built-in instructions field reads as a broken feature, not an unset one,
-- and three agents independently filed it as a bug for exactly that
-- reason. This migration ships default text so a freshly migrated database
-- never starts from that empty, ambiguous state.
--
-- The wording below is drawn from docs/cli-spec.md's "Work Branches"
-- section (state gates, review rounds, staged comments, verdicts) rather
-- than invented here, so there is one description of the workflow and not
-- two. It intentionally does NOT restate what internal/handler/meta's
-- usageText already covers in every GetInstructions response regardless of
-- role -- positional arguments, plain git with no loam commit/push, exit
-- codes -- since role_instructions is merged with that general guide, not
-- read alone; repeating it here would just be drift waiting to happen.
--
-- THE GUARD, and the reason this is not a plain UPDATE: the live
-- deployment already carries non-empty, human-typed text on 'author' (92
-- characters of filler, per this bead's own investigation) that predates
-- this migration. An unconditional UPDATE would destroy it. Both
-- statements below therefore fill in ONLY where the existing value reads
-- as empty (`coalesce(instructions, '') = ''`), so:
--   * a fresh database (both roles seeded '' by 0001_init) gets this text
--     for both roles.
--   * the live deployment's already-non-empty 'author' row is left exactly
--     as it is -- this migration does not, and cannot, distinguish "real
--     policy someone wrote" from "filler someone typed"; see this bead's
--     report for the follow-up admin action that filler text still needs.
--   * 'reviewer' on that same deployment (seeded '', never touched) DOES
--     get filled, since it satisfies the guard.
--
-- A custom, operator-created role is untouched by either statement: both
-- are scoped to `builtin`, and CreateRole (queries/roles.sql) never sets
-- that flag, so no non-builtin row can match `AND builtin` in the first
-- place regardless of its instructions value.
UPDATE roles
SET instructions = $instructions$An author starts work branches (work start), sets a title and description (work set) -- both required before request-review -- and requests review when ready, opening a numbered review round; request-review is rejected while the branch is still reviewable and awaiting a verdict (exit 2), and once it is reviewed, request review again to open a fresh round -- that marks the prior round's verdicts stale. Between rounds, reply to threads with work reply. A push lands only on your own, non-terminal work branch. Submitting a verdict belongs to the reviewer role, not this one.$instructions$
WHERE name = 'author' AND builtin AND coalesce(instructions, '') = '';

UPDATE roles
SET instructions = $instructions$A reviewer reads work branches, replies to threads, and submits verdicts -- never starts a work branch or pushes to one. Open new threads with work comment, which stages comments locally and keeps them invisible to everyone else until work verdict publishes them together with an outcome (approve/disapprove/neutral) and clears the staging area. Replies (work reply) are the exception: they post immediately, unstaged.$instructions$
WHERE name = 'reviewer' AND builtin AND coalesce(instructions, '') = '';
