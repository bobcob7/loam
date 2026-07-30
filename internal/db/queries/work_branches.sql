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
-- MarkWorkBranchConflicted (which also stamps conflict = 'reset' in the
-- same statement); -> complete only through CompleteWorkBranch ("There is
-- no agent complete command" -- server-only, set when the upstream PR
-- merges); -> closed only through CloseWorkBranch (admin-only, and it also
-- records close_reason in the same statement). Splitting these out means
-- this method's guard can never be widened by accident into a path that
-- reaches complete/closed without their own bookkeeping.
--
-- Both transitions into reviewable also require a title and description
-- to already be set (docs/cli-spec.md "request-review": "Requires a title
-- and description to already be set (via set)"; the State gates table,
-- ":289"/":300" in this file's own review notes). title/description are
-- nullable columns that start out NULL (CreateWorkBranch never sets
-- them) and, once set, are only ever written as a non-NULL, possibly
-- empty string (SetWorkBranchTitleDescription's pgText always writes
-- Valid: true, never SQL NULL) -- so both "never set" (NULL) and "set to
-- empty" ('') must be checked; neither alone is enough.
UPDATE work_branches
SET state = $2, updated_at = now()
WHERE id = $1
  AND (
    (state = 'draft' AND $2::text = 'reviewable'
      AND title IS NOT NULL AND title <> ''
      AND description IS NOT NULL AND description <> '') OR
    (state = 'reviewable' AND $2::text = 'reviewed') OR
    (state = 'reviewed' AND $2::text = 'reviewable'
      AND title IS NOT NULL AND title <> ''
      AND description IS NOT NULL AND description <> '')
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
-- is no agent complete command"; "the work branch flips to complete only
-- when that PR merges" -- a statement about the trigger, with no state
-- precondition attached to it).
--
-- The guard is any non-terminal state, matching CloseWorkBranch above, NOT
-- the narrower state IN ('reviewable','reviewed') it started as. That
-- narrower guard was unreachable-by-design in one real case and wrong in
-- it: docs/git-spec.md "Target Advances & Catch-Up" resets a reviewable/
-- reviewed branch -- "including an accepted proposal with an open PR" --
-- all the way back to 'draft' on a conflicting target advance, while
-- deliberately leaving the upstream PR untouched. If that PR then merges
-- on the forge, the forge merge is authoritative (loam-giq.8 DESIGN:
-- "merged -> state='complete' regardless of whatever conflict/round state
-- it was in") and the branch must reach 'complete'; under the old guard it
-- could not, and internal/mirrorsync's PR poller would have re-reported
-- the same ErrIllegalTransition on every sync tick, forever, for a
-- proposal that had in fact shipped.
--
-- The "only an accepted proposal completes" property is not lost, it just
-- lives at the caller now: StorePRPoller (internal/mirrorsync/pr_poller.go)
-- is the only caller in the tree and only ever completes a branch whose
-- work_branches.upstream_pr_number is recorded and whose PR the forge just
-- reported merged. That column is only ever written by proposal acceptance,
-- which requires 'reviewed'.
UPDATE work_branches
SET state = 'complete', updated_at = now()
WHERE id = $1 AND state NOT IN ('complete', 'closed')
RETURNING *;

-- name: MarkWorkBranchConflicted :one
-- The server's mergeability check re-evaluates EVERY open (non-terminal)
-- work branch on every target-branch advance (docs/git-spec.md "Target
-- Advances & Catch-Up": "the server tests each open (non-terminal) work
-- branch against the new tip"). That is level-triggered, not
-- edge-triggered: the same branch can be found still-conflicting on
-- several advances in a row before anyone catches it up, so this method
-- is idempotent -- calling it again on an already-conflicted branch is a
-- benign no-op, never errIllegalTransition.
--
-- A draft branch just gains the flag (conflict -> flagged, state
-- untouched); a reviewable/reviewed branch is reset to draft AND flagged
-- in the SAME statement (conflict -> reset, state -> draft), so no reader
-- ever observes conflict = 'reset' paired with a pre-demotion state. If
-- the branch is ALREADY draft+reset (a prior demotion that has not caught
-- up yet) and is found conflicting again, this call preserves 'reset'
-- rather than downgrading it to 'flagged': 'reset' is what tells
-- ClearWorkBranchConflict to restore the branch directly to reviewable
-- ("the round was interrupted, not abandoned") -- silently losing that
-- distinction on a second failed re-check would strand the branch as
-- merely-flagged, with no restore-to-reviewable path left.
--
-- Guarded to state IN ('draft', 'reviewable', 'reviewed') -- the open,
-- non-terminal states the mergeability check applies to at all; a
-- complete/closed branch reaching this method is a caller bug, not a
-- state this method silently accepts.
UPDATE work_branches
SET conflict = CASE
      WHEN state IN ('reviewable', 'reviewed') THEN 'reset'
      WHEN conflict = 'reset' THEN 'reset'
      ELSE 'flagged'
    END,
    state = CASE WHEN state IN ('reviewable', 'reviewed') THEN 'draft' ELSE state END,
    updated_at = now()
WHERE id = $1 AND state IN ('draft', 'reviewable', 'reviewed')
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

-- name: RecordWorkBranchUpstreamPR :one
-- Proposal Acceptance's record leg (docs/sync-spec.md "Proposal
-- Acceptance", step 2: "record upstream_pr_url and upstream_pr_number").
-- The upstream PR is opened once per work branch and never re-opened: a
-- re-accept after a catch-up fast-forwards the SAME branch upstream, which
-- updates the SAME PR in place, so this statement's whole job is to write
-- that identity exactly once.
--
-- upstream_pr_number IS NULL is therefore a guard, not a convenience. It is
-- the second half of proposal acceptance's idempotency, the half that
-- survives a race: the accept engine also skips CreatePR when the column it
-- already read was non-NULL, but two concurrent accepts can both read NULL
-- and both call CreatePR, and only one of them may win the column. Without
-- this predicate the loser would overwrite the winner's number, and
-- internal/mirrorsync's PR poller -- whose entire poll set is "rows with a
-- recorded upstream_pr_number" -- would then poll a PR nobody can reach and
-- silently stop tracking the real one. Zero rows here means "already
-- recorded" (or "no such row"), which internal/workbranchstore separates
-- into ErrPRAlreadyRecorded and ErrNotFound rather than folding into a
-- silent no-op success.
--
-- The url and number are written in ONE statement, never separately: a row
-- carrying a number with no url (or the reverse) would be a half-accepted
-- proposal no reader in the tree knows how to interpret.
--
-- accepted_tip ($4, loam-cgg) rides in the SAME statement for the identical
-- reason: it is what ListProposals compares against the live mirror tip to
-- decide "PR branch is behind the work branch" (docs/web-spec.md's
-- proposal definition), and a row that recorded a PR number without also
-- recording what was pushed for it would be exactly the half-accepted
-- shape the url/number pairing above already refuses to produce.
UPDATE work_branches
SET upstream_pr_url = $2, upstream_pr_number = $3, accepted_tip = $4, updated_at = now()
WHERE id = $1 AND upstream_pr_number IS NULL
RETURNING *;

-- name: RecordWorkBranchAcceptedTip :one
-- Proposal Acceptance's record leg for a RE-accept (loam-cgg): a
-- fast-forward of an already-recorded PR opens no new pull request and so
-- never reaches RecordWorkBranchUpstreamPR above, but it still moves the
-- upstream branch to a new tip and that tip is exactly what
-- ListProposals's "is the PR branch behind the work branch" comparison
-- needs refreshed. Unlike RecordWorkBranchUpstreamPR this UPDATE carries
-- no upstream_pr_number guard: a re-accept is expected to run any number
-- of times against a branch that already carries a PR (docs/sync-spec.md
-- "Proposal Acceptance": "Re-accepting a caught-up work branch updates the
-- existing PR"), so unconditionally overwriting the previously recorded
-- tip with the one this accept just pushed is the correct behavior, not a
-- race to arbitrate.
UPDATE work_branches
SET accepted_tip = $2, updated_at = now()
WHERE id = $1
RETURNING *;
