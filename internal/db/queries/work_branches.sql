-- Work branches queries (loam-54o.10): the work_branches aggregate --
-- create, get, list (with filters and offset pagination), title/description,
-- and the state + conflict transitions (docs/persistence-spec.md
-- "work_branches"; docs/git-spec.md "Target Advances & Catch-Up"). IDs are
-- generated in Go (uuid.NewV7, per persistence-spec "Conventions"), never in
-- SQL, so CreateWorkBranch takes id as a bound parameter.
--
-- Every state/conflict transition below is a single guarded
-- UPDATE ... WHERE ... RETURNING *: the legal-from-state check and the
-- write happen atomically in one statement, so a concurrent racer never
-- sees a transition applied from a state it was no longer valid in, and an
-- illegal call (wrong current state) always returns zero rows rather than
-- silently applying anyway. internal/workbranchstore maps zero rows to a
-- distinguishable illegal-transition error (or not-found, if the id itself
-- does not exist), never lets it pass as a silent no-op success.

-- name: CreateWorkBranch :one
-- repo_id + name is the aggregate's identity (work_branches_repo_id_name_key,
-- UNIQUE(repo_id, name)); state and conflict take their column defaults
-- ('draft', 'none').
INSERT INTO work_branches (id, repo_id, name, target, author)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetWorkBranchByID :one
SELECT * FROM work_branches WHERE id = $1;

-- name: GetWorkBranchByName :one
-- Resolves the (repo_id, name) identity to a row -- the lookup a ref-name
-- (e.g. the pre-receive hook's proposed ref update) needs before it has an
-- id to call any other query with.
SELECT * FROM work_branches WHERE repo_id = $1 AND name = $2;

-- name: SetWorkBranchTitleDescription :one
-- Editable "at any point" per docs/cli-spec.md's `set` command, EXCEPT a
-- terminal state (its own State gates table: `set` allowed in draft/
-- reviewable/reviewed, rejected in complete/closed) -- guarded here, not
-- left to the caller to remember.
UPDATE work_branches
SET title = $2, description = $3, updated_at = now()
WHERE id = $1 AND state NOT IN ('complete', 'closed')
RETURNING *;

-- name: UpdateWorkBranchState :one
-- The ordinary, agent-facing lifecycle transitions only
-- (docs/cli-spec.md "Its lifecycle": "draft -> (request-review) ->
-- reviewable -> (first verdict) -> reviewed. A re-review returns it to
-- reviewable"): draft -> reviewable (request-review), reviewable ->
-- reviewed (first verdict), reviewed -> reviewable (re-review, by the
-- author or the admin sending it back). Every other pair is deliberately
-- absent from this guard and reached (if at all) through a dedicated
-- method instead: reviewable/reviewed -> draft only through
-- DemoteWorkBranchOnConflict (which also stamps conflict = 'reset' in the
-- same statement); -> complete only through CompleteWorkBranch ("There is
-- no agent complete command" -- server-only, set when the upstream PR
-- merges); -> closed only through CloseWorkBranch (admin-only, and it also
-- records close_reason in the same statement). Splitting these out means
-- this method's guard can never be widened by accident into a path that
-- reaches complete/closed without their own bookkeeping.
UPDATE work_branches
SET state = $2, updated_at = now()
WHERE id = $1
  AND (
    (state = 'draft' AND $2::text = 'reviewable') OR
    (state = 'reviewable' AND $2::text = 'reviewed') OR
    (state = 'reviewed' AND $2::text = 'reviewable')
  )
RETURNING *;

-- name: CloseWorkBranch :one
-- The admin-only close path (docs/cli-spec.md: "closed (admin-only, or
-- when the upstream PR is closed)"), recording close_reason in the same
-- statement -- any non-terminal state may close.
UPDATE work_branches
SET state = 'closed', close_reason = $2, updated_at = now()
WHERE id = $1 AND state NOT IN ('complete', 'closed')
RETURNING *;

-- name: CompleteWorkBranch :one
-- Set by the server when the upstream PR merges (docs/cli-spec.md: "There
-- is no agent complete command"); only a reviewable/reviewed branch (an
-- accepted proposal) can complete.
UPDATE work_branches
SET state = 'complete', updated_at = now()
WHERE id = $1 AND state IN ('reviewable', 'reviewed')
RETURNING *;

-- name: FlagWorkBranchConflict :one
-- Target advance no longer merges cleanly, and the branch was draft: "a
-- draft branch just gains the flag" (docs/git-spec.md "Target Advances &
-- Catch-Up") -- state does not change, only conflict: none -> flagged.
UPDATE work_branches
SET conflict = 'flagged', updated_at = now()
WHERE id = $1 AND state = 'draft' AND conflict = 'none'
RETURNING *;

-- name: DemoteWorkBranchOnConflict :one
-- Target advance no longer merges cleanly, and the branch was reviewable
-- or reviewed: it is "reset to draft and flagged as conflicted" in one
-- statement (docs/git-spec.md "Target Advances & Catch-Up") -- conflict:
-- none -> reset, state: reviewable/reviewed -> draft, together, so no
-- reader ever observes conflict = 'reset' paired with a pre-demotion state.
UPDATE work_branches
SET conflict = 'reset', state = 'draft', updated_at = now()
WHERE id = $1 AND state IN ('reviewable', 'reviewed') AND conflict = 'none'
RETURNING *;

-- name: ClearWorkBranchConflict :one
-- A catch-up push brings the branch up to date (docs/git-spec.md "Target
-- Advances & Catch-Up"): conflict always returns to 'none'; state only
-- moves if the branch had been demoted (conflict was 'reset') -- it "flips
-- directly back to reviewable", no request-review needed. A merely
-- 'flagged' branch (never demoted, stayed draft throughout) just loses the
-- flag and its state is untouched by this statement. Guarded to
-- conflict IN ('flagged', 'reset') so clearing an already-'none' conflict
-- (nothing to clear) returns zero rows rather than a silent no-op success.
UPDATE work_branches
SET conflict = 'none',
    state = CASE WHEN conflict = 'reset' THEN 'reviewable' ELSE state END,
    updated_at = now()
WHERE id = $1 AND conflict IN ('flagged', 'reset')
RETURNING *;

-- name: ListWorkBranches :many
-- Offset pagination (docs/persistence-spec.md "Conventions"), paired with
-- CountWorkBranches for PageInfo.total. Every filter is optional: an empty
-- string ($2 target, $3 author, $4 state, $5 awaiting-verdict reviewer)
-- means "no filter on this column" rather than "match the empty string" --
-- none of these columns is ever legitimately empty (mirrors the same
-- sentinel convention LookupSymbolsByName's file filter uses,
-- internal/db/queries/code_graph.sql) -- and repo_id ($1) is NULL for "no
-- filter", never a real filter value of the zero UUID.
--
-- The awaiting-verdict filter ($5) joins review_rounds/verdicts on the
-- CURRENT round only (the same MAX(number)-per-branch subquery
-- CurrentRoundApproveCount uses, review_rounds.sql) -- a reviewable branch
-- whose current round has no live verdict yet from the named reviewer.
-- Staleness is derived here exactly as it is everywhere else in this
-- codebase: never a stored flag.
SELECT wb.* FROM work_branches wb
WHERE ($1::uuid IS NULL OR wb.repo_id = $1)
  AND ($2::text = '' OR wb.target = $2)
  AND ($3::text = '' OR wb.author = $3)
  AND ($4::text = '' OR wb.state = $4)
  AND (
    $5::text = '' OR (
      wb.state = 'reviewable'
      AND NOT EXISTS (
        SELECT 1 FROM verdicts v
        JOIN review_rounds rr ON rr.id = v.round_id
        WHERE rr.work_branch_id = wb.id
          AND rr.number = (SELECT MAX(rr2.number) FROM review_rounds rr2 WHERE rr2.work_branch_id = wb.id)
          AND v.reviewer = $5
      )
    )
  )
ORDER BY wb.created_at DESC, wb.id
LIMIT $6 OFFSET $7;

-- name: CountWorkBranches :one
-- Same filter predicate as ListWorkBranches, minus LIMIT/OFFSET, for
-- PageInfo.total.
SELECT COUNT(*) FROM work_branches wb
WHERE ($1::uuid IS NULL OR wb.repo_id = $1)
  AND ($2::text = '' OR wb.target = $2)
  AND ($3::text = '' OR wb.author = $3)
  AND ($4::text = '' OR wb.state = $4)
  AND (
    $5::text = '' OR (
      wb.state = 'reviewable'
      AND NOT EXISTS (
        SELECT 1 FROM verdicts v
        JOIN review_rounds rr ON rr.id = v.round_id
        WHERE rr.work_branch_id = wb.id
          AND rr.number = (SELECT MAX(rr2.number) FROM review_rounds rr2 WHERE rr2.work_branch_id = wb.id)
          AND v.reviewer = $5
      )
    )
  );
