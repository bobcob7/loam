-- Review-discussion queries (loam-54o.12): the threads + comments table
-- group (docs/persistence-spec.md "threads", "comments"). IDs are
-- generated in Go (uuid.NewV7, per persistence-spec "Conventions"), never
-- in SQL, so every INSERT below takes id as a bound parameter.
--
-- Both tables carry round_id, and they are INDEPENDENT: threads.round_id is
-- the round the thread was RAISED in and never changes, while a comment's
-- round_id is the branch's current round at the moment that comment was
-- posted -- so a reply can, and routinely does, land in a LATER round than
-- its thread (docs/persistence-spec.md "comments"; replies.feature "A reply
-- records the round it was made in"). Neither is derived from the other.
--
-- Staged comments are NOT in these tables at all: they live locally in the
-- CLI's .loam until a verdict publishes them (docs/cli-spec.md "comment").
-- Everything here is, by construction, already published.

-- name: OpenThreadWithComment :one
-- Opens a thread AND its opening comment as ONE statement. Two chained
-- data-modifying CTEs, not two round-trips: a thread with no comments is
-- never observable by any reader, and no caller has to remember to wrap
-- the pair in a transaction of its own to get that property (loam-54o.12
-- DESIGN: "Opening a thread and its opening comment is one atomic
-- operation"). Both rows are stamped with the SAME round_id ($3) -- the
-- branch's current round at the moment the thread is raised.
--
-- The comment CTE selects FROM t rather than binding the thread id again,
-- so the comment cannot be attached to anything but the thread this
-- statement just inserted.
WITH t AS (
    INSERT INTO threads (id, work_branch_id, round_id, author, file, line)
    VALUES ($1, $2, $3, $4, $5, $6)
    RETURNING *
), c AS (
    INSERT INTO comments (id, thread_id, round_id, author, body)
    SELECT $7, t.id, $3, $4, $8 FROM t
    RETURNING *
)
SELECT
    t.id, t.work_branch_id, t.round_id, t.author, t.file, t.line,
    t.resolved, t.created_at, t.updated_at,
    c.id AS comment_id, c.body AS comment_body, c.created_at AS comment_created_at
FROM t, c;

-- name: AddComment :one
-- A reply: one comment appended to an EXISTING thread, immediate and never
-- staged (docs/cli-spec.md "reply"). round_id ($3) is the branch's current
-- round at the moment of the reply, resolved by the caller against
-- review_rounds -- deliberately independent of the thread's own round_id,
-- which this statement never reads and never changes.
INSERT INTO comments (id, thread_id, round_id, author, body)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetThread :one
SELECT * FROM threads WHERE id = $1;

-- name: ResolveThread :one
-- Author-only resolution (docs/cli-spec.md "comment": "Only the thread's
-- original author may resolve it"), enforced as a single guarded
-- UPDATE ... WHERE ... RETURNING * exactly like every work_branches
-- transition: the ownership check and the write are one atomic statement,
-- so a concurrent racer can never slip a resolve past a check that was
-- true a moment earlier. work_branch_id is in the guard too, so a thread
-- id belonging to some OTHER work branch can never be resolved through
-- this work branch's request. Zero rows means "not this author, not this
-- work branch, or no such thread" -- the store re-reads the row to tell
-- those apart rather than guessing.
--
-- Idempotent: resolving an already-resolved thread the caller authored
-- still matches and returns the row, so a retried verdict is not an error.
UPDATE threads
SET resolved = true, updated_at = now()
WHERE id = $1 AND work_branch_id = $2 AND author = $3
RETURNING *;

-- name: ListThreadsForWorkBranch :many
-- Offset pagination over THREADS (docs/persistence-spec.md "Conventions"),
-- paired with CountThreadsForWorkBranch for PageInfo.total -- the page unit
-- is the thread, since ListComments returns Thread[] and a thread's
-- comments are never split across pages. round_number is joined in from
-- review_rounds so a client can render "raised in round N" without a
-- second query; it is the round the thread was raised in, NOT a derived
-- staleness flag (this table group has none).
SELECT
    t.id, t.work_branch_id, t.round_id, t.author, t.file, t.line,
    t.resolved, t.created_at, t.updated_at,
    r.number AS round_number
FROM threads t
JOIN review_rounds r ON r.id = t.round_id
WHERE t.work_branch_id = $1
ORDER BY t.created_at ASC, t.id
LIMIT $2 OFFSET $3;

-- name: CountThreadsForWorkBranch :one
-- Same predicate as ListThreadsForWorkBranch, minus LIMIT/OFFSET, for
-- PageInfo.total.
SELECT COUNT(*) FROM threads WHERE work_branch_id = $1;

-- name: ListCommentsForThreads :many
-- Every comment on the given threads, oldest first, each carrying its OWN
-- round number -- which may be greater than its thread's, since a reply
-- lands in whatever round was current when it was posted. Fetched for a
-- whole page of thread ids in one query rather than per thread.
SELECT
    c.id, c.thread_id, c.round_id, c.author, c.body, c.created_at,
    r.number AS round_number
FROM comments c
JOIN review_rounds r ON r.id = c.round_id
WHERE c.thread_id = ANY($1::uuid[])
ORDER BY c.created_at ASC, c.id;
