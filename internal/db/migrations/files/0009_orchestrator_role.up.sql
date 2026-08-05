-- Seeds the third built-in role, 'orchestrator' (loam-hi5o.31): the
-- supervisor that reads a task, dispatches author and reviewer agents,
-- carries findings between them, and records outcomes. Until now that role
-- existed in practice and nowhere in the product, so every orchestrator
-- reconstructed it from scratch.
--
-- WHY A ROLE ROW AND NOT A CONSTANT IN THE BINARY: operators tailor how
-- their agents work, and the instruction text is exactly what differs
-- between deployments. Roles are already the configurable, web-editable
-- home for such text (loam.admin.v1.RoleService, plus the console's role
-- screens -- web/src/routes/Roles.tsx edits instructions and operations for
-- built-in roles too, only deletion is refused). A constant compiled into
-- the server would make every deployment's orchestration policy identical
-- and un-editable.
--
-- WHAT IT GRANTS, AND WHY EXACTLY THAT: 'graph.query' and 'search', and
-- nothing else. The loop's first step is verifying a task's factual claims
-- about the code BEFORE an agent is dispatched against them, and those two
-- capabilities are what make that possible without a clone. It holds no
-- work-branch capability -- not work.start, work.set, work.request_review,
-- work.reply, work.verdict, work.read, and not git.clone or git.push: the
-- orchestrator supervises, the agents act. internal/db/migrations'
-- TestOrchestratorRoleSeedMigration_GrantsExactlyGraphQueryAndSearch pins
-- that set exactly, so a later edit here cannot quietly widen it.
--
-- Because it grants something real it is an ORDINARY role: the console's
-- existing capability editing applies unchanged, and an operator who wants
-- to add a capability may -- the same freedom every other role has.
--
-- THE UUID is a hand-written v7-shaped literal continuing 0001_init's own
-- sequence (author ...-7952-..., reviewer ...-7953-..., orchestrator
-- ...-7954-...), for the reason 0001_init gives: Postgres has no built-in
-- uuidv7(), so literals are the only route, and deterministic ids make
-- these rows easy fixtures.
--
-- THE GUARDS, all three of them, and 0006's lesson. This migration must be
-- safe to apply to a deployment that already has SOMETHING named
-- 'orchestrator', because roles_name_key is UNIQUE and an unguarded INSERT
-- would abort the whole migration:
--
--   * ON CONFLICT (name) DO NOTHING on the role row. A pre-existing row --
--     including an operator-created, non-builtin one -- survives untouched,
--     and the CLI's well-known identity then resolves to THAT role, which
--     is the right answer: the operator got there first.
--   * The operations insert is scoped to `WHERE name = 'orchestrator' AND
--     builtin`, so it cannot silently grant capabilities to an operator's
--     own same-named custom role, and ON CONFLICT DO NOTHING so a rerun
--     against a row that already carries them is a no-op rather than a
--     primary-key violation.
--   * The instructions fill is guarded on `coalesce(instructions, '') = ''`
--     exactly as 0006_role_instructions_seed's is, and for the same reason.
--     0006's own history is the lesson: because that guard is honest, the
--     live deployment's 'author' row -- which already carried 92 characters
--     of human-typed filler -- was correctly left alone, while 'reviewer'
--     (still '') got the good seeded text. The same trap applies here on
--     any redeploy or restore: once this row's instructions are non-empty,
--     no rerun of this migration will ever replace them, and changing the
--     text below is therefore NOT how you update a deployment that has
--     already run it. That is an admin RoleService.UpdateRole call.
--
-- The row is inserted with instructions = '' and filled by the third
-- statement rather than carrying its text inline, so the fresh-database
-- path and the already-exists path both flow through the SAME guarded
-- UPDATE and there is only one copy of the text in this file.
--
-- THE TEXT ITSELF DELIBERATELY NAMES NO ISSUE TRACKER. docs/orchestration.md
-- may reference this repository's own choice (CLAUDE.md mandates bd) because
-- it is this repository's document; this string ships to EVERY deployment on
-- first migration, and most of them will not use that tracker or any other.
-- It is written against what loam actually provides -- a work branch
-- carrying a title and description, set with `loam work set`, which is the
-- specification both agents read -- and phrased so it is true whether the
-- task arrived from a tracker, a one-off instruction, or a paragraph the
-- operator wrote. loam has no tracker integration and grows none here;
-- where the work came from is the operator's business, and an operator who
-- wants theirs named edits the role, which is precisely why this is a role
-- and not a constant.
--
-- It also does not restate what internal/handler/meta's usageText already
-- returns in every GetInstructions response regardless of role (positional
-- arguments, plain git with no `loam commit`/`loam push`, exit codes), since
-- role_instructions is merged with that general guide, not read alone.
INSERT INTO roles (id, name, instructions, builtin)
VALUES ('019f9c4b-0474-7954-9c1f-3d2b8e6a70f1', 'orchestrator', '', true)
ON CONFLICT (name) DO NOTHING;

INSERT INTO role_operations (role_id, operation)
SELECT roles.id, op
FROM roles, unnest(ARRAY['graph.query', 'search']) AS op
WHERE roles.name = 'orchestrator' AND roles.builtin
ON CONFLICT (role_id, operation) DO NOTHING;

UPDATE roles
SET instructions = $instructions$An orchestrator supervises work it does not perform. It holds graph.query and search and no work-branch capability: it can read the code but cannot start a branch, push, comment, or cast a verdict. The long form of everything below, with the full hazard table, is docs/orchestration.md in the loam repository.

The loop. First, verify the task's factual claims about the code before dispatching anyone against them -- whatever its source, a task written from memory is often wrong in a way that costs a whole cycle, and search and graph queries answer "does this already exist" and "where does it actually live" without a clone. Then dispatch an author with its own identity, exclusive file territory, and every decision the task leaves open named as a decision it must make and record. Then dispatch a reviewer under a DIFFERENT identity. Carry each finding to the commit that answers it: nothing in loam records that mapping for you, and it is the least automatable part of the job. Finally record the outcome and its reasoning, including the arguments you rejected -- the rejected argument is the one someone re-proposes later.

The specification is the work branch. Its title and description, set with work set, are what an author and a reviewer both read; nothing else travels with the work. State what is already true, with file and line. State what will bite, especially anything that fails silently. Name the decisions the implementer owns and require the argument, not just the outcome. Say what is out of scope. If the task turns out to be already done, or wrong, record that with evidence instead of building something anyway. A specification that describes only a symptom produces an implementation that solves only the symptom.

Identity. The identifier is <name>-<id>-<role>, built from three separate variables: LOAM_AGENT_NAME is the bare name, LOAM_AGENT_ID the bare id, LOAM_AGENT_ROLE the role. Putting the whole identifier into NAME writes doubled names into permanent review records and cannot be fixed afterwards. Author and reviewer must be different identities, because roles gate capabilities -- a reviewer holds git.clone but not git.push and genuinely cannot push.

Review discipline. Tell the reviewer to mutate the code and confirm a test dies by an assertion, not by a panic or a compile error; a reviewer that only re-runs the suite adds nothing. Ask what the fixtures make indistinguishable -- seed values that let two code paths produce identical output are the most common defect that survives a rigorous-looking mutation battery. Read prose as prose: comments and docs asserting a mechanism that does not exist are the second most common, and a diff will not reveal them. Block on prose that gives a reader a false model; do not block on prose that is merely loose where a neighbouring sentence fixes the scope.

Hazards to state explicitly, because agents do not assume them. Every shell invocation is fresh and exports do not persist. Uncommitted work dies with the agent, so commit early even if broken. A backgrounded gate does not finish -- ending a turn stops it. request-review does not open a pull request, so an author cannot observe CI on its own branch. After pushing, verify: git rev-list --count @{u}..HEAD must be 0, because a branch reaches reviewable with its commits still local and everything downstream still looks healthy. And distrust any check that can report success without performing the check -- go vet with a build tag runs no tests, a test piped into grep reports grep's exit status, and generated files and containers are shared territory between parallel agents.$instructions$
WHERE name = 'orchestrator' AND builtin AND coalesce(instructions, '') = '';
