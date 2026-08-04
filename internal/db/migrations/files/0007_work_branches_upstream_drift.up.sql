-- Adds work_branches.upstream_drift (loam-giq.11): whether the upstream
-- loam/<name> branch a proposal acceptance pushed has since been moved by
-- someone other than Loam, in a way Loam refuses to reconcile on its own
-- (docs/sync-spec.md "Upstream Drift on `loam/<work-branch>`").
--
-- Its OWN column, deliberately not a fourth `conflict` value. The two
-- describe independent facts that can hold simultaneously -- a target can
-- advance into a merge conflict while the loam/ branch is separately
-- rewritten -- and a single column would let whichever happened second
-- overwrite the first, leaving the operator to fix one problem while never
-- learning about the other. They also want different sentences:
-- flagged/reset mean "the target moved, catch up"; diverged means "someone
-- rewrote the branch Loam pushed".
--
-- Only two values, and there is no 'fast_forward' among them: a
-- fast-forward is ADOPTED by the sync cycle (the work branch advances, a
-- fresh review round opens, accepted_tip absorbs the commit) rather than
-- recorded, so it is never a state anything reads back. 'diverged' is the
-- one case Loam will not guess at.
--
-- NOT NULL DEFAULT 'none', so every existing row is 'none' the moment this
-- migration runs -- which is the truth for them: nothing had ever compared
-- the mirrored loam/ ref against accepted_tip before this feature, so no
-- row can carry a drift observation that this default would erase. The
-- first sync tick after deploy re-derives the value for every work branch
-- with a recorded PR anyway, since the reconciliation is level-triggered
-- (see internal/mirrorsync/drift_reconciler.go).
ALTER TABLE work_branches
    ADD COLUMN upstream_drift text NOT NULL DEFAULT 'none'
        CONSTRAINT work_branches_upstream_drift_check
        CHECK (upstream_drift IN ('none', 'diverged'));
