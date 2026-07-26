-- name: OpenReviewRound :one
-- Opens a new review round for a work branch: number = MAX(number)+1 for
-- that work_branch_id (1 if none exist yet). A single atomic
-- INSERT ... SELECT so the number computation and the row creation happen
-- as one statement -- review_rounds_work_branch_id_number_key still
-- catches a concurrent-open race as a real unique-violation rather than
-- silently double-assigning a round number (docs/persistence-spec.md
-- "review_rounds"; called on every transition into reviewable: author
-- request-review, admin send-back, catch-up auto-restore).
INSERT INTO review_rounds (id, work_branch_id, number, requested_by)
SELECT $1, $2, COALESCE(MAX(number), 0) + 1, $3
FROM review_rounds
WHERE work_branch_id = $2
RETURNING *;

-- name: CurrentReviewRound :one
-- The work branch's current round: the review_rounds row with the highest
-- number. Every "is this stale" comparison in this table group goes
-- through here or the equivalent inline subquery below -- never through a
-- stored flag.
SELECT * FROM review_rounds
WHERE work_branch_id = $1
ORDER BY number DESC
LIMIT 1;

-- name: SubmitVerdict :one
-- Re-submitting the same (round_id, reviewer) replaces the prior verdict
-- in place (upsert on conflict), never creating a second row --
-- verdicts_round_id_reviewer_key is the constraint Demo M1 shows off: one
-- verdict per reviewer per round.
INSERT INTO verdicts (id, round_id, reviewer, outcome)
VALUES ($1, $2, $3, $4)
ON CONFLICT (round_id, reviewer)
DO UPDATE SET outcome = EXCLUDED.outcome, updated_at = now()
RETURNING *;

-- name: ListVerdictsForWorkBranch :many
-- All verdicts across all rounds for a work branch, newest round first, for
-- history. is_current_round is computed by the same query that reads each
-- row (comparing round numbers against the branch's current MAX(number)),
-- never read from a stored column -- staleness is derived, not stored
-- (docs/persistence-spec.md "verdicts").
SELECT
    v.id, v.round_id, r.number AS round_number, v.reviewer, v.outcome,
    v.created_at, v.updated_at,
    (r.number = (SELECT MAX(r2.number) FROM review_rounds r2 WHERE r2.work_branch_id = $1)) AS is_current_round
FROM verdicts v
JOIN review_rounds r ON r.id = v.round_id
WHERE r.work_branch_id = $1
ORDER BY r.number DESC, v.created_at ASC;

-- name: CurrentRoundApproveCount :one
-- Approve-outcome count for the work branch's current round only -- backs
-- the proposal queue / approval bar (docs/persistence-spec.md "verdicts").
-- Joins on the current round via a MAX(number) subquery rather than
-- trusting any stored staleness flag; when the branch has no rounds yet
-- the subquery is NULL, no row ever matches, and the count is simply 0.
SELECT COUNT(*) FROM verdicts v
JOIN review_rounds r ON r.id = v.round_id
WHERE r.work_branch_id = $1
AND r.number = (SELECT MAX(r2.number) FROM review_rounds r2 WHERE r2.work_branch_id = $1)
AND v.outcome = 'approve';
