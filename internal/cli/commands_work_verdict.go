package cli

import (
	"context"
	"flag"
	"fmt"

	"connectrpc.com/connect"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// newWorkVerdictFlags builds the flag.FlagSet for `loam work verdict
// [repo] [work-branch] --outcome <approve|disapprove|neutral>`, plus the
// parsed --outcome value.
func newWorkVerdictFlags() (*flag.FlagSet, *string) {
	fs := newFlagSet("work verdict")
	outcome := fs.String("outcome", "", "approve, disapprove, or neutral")
	return fs, outcome
}

// verdictOutput is `work verdict`'s success shape (docs/cli-spec.md ->
// verdict). Published is the count the SERVER reports, not the number of
// items this process sent: the two differ by design, because a resolve-only
// staged item publishes no comment.
type verdictOutput struct {
	Repo       string `json:"repo"`
	WorkBranch string `json:"work_branch"`
	Outcome    string `json:"outcome"`
	Published  uint32 `json:"published"`
}

// runWorkVerdict implements `loam work verdict [repo] [work-branch]
// --outcome <approve|disapprove|neutral>` (docs/cli-spec.md -> verdict):
// publish the caller's whole staged batch atomically as one verdict, then
// clear the staging area.
//
// The atomicity is entirely the server's — SubmitVerdict does the threads,
// their opening comments, the resolutions, the verdict row, and the
// reviewable -> reviewed flip in a single transaction. This command's job is
// to make exactly one call carrying the whole batch, so there is no
// half-published state for it to have to reason about.
//
// It deliberately does no pre-flight RPC. The state gate (verdict is allowed
// only in reviewable/reviewed; draft has no round yet, and the terminal
// states are closed to it) is enforced inside that transaction, so a check
// here would be both duplicated and raceable. It also does no local
// author-only pre-check for the staged resolves: the server refuses them
// with PermissionDenied, which classifyConnectError maps to unauthorized,
// exit 2 — the same class `work comment`'s local pre-check reports.
func runWorkVerdict(ctx context.Context, deps *Deps, args []string) error {
	fs, outcomeFlag := newWorkVerdictFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work verdict", positional); err != nil {
		return err
	}
	outcome, err := parseVerdictOutcome(*outcomeFlag)
	if err != nil {
		return err
	}
	repo, workBranch, err := resolveWorkBranchIdentity(deps.workspace, positional)
	if err != nil {
		return err
	}
	return publishVerdict(ctx, deps, repo, workBranch, outcome)
}

// parseVerdictOutcome maps the --outcome flag to its proto enum. A missing
// or unrecognized outcome is a usage error (exit 2, docs/cli-spec.md ->
// verdict -> Errors) decided from the arguments alone, before any staging
// area is opened or any RPC is made — there is no default outcome, because
// guessing one would record a verdict the reviewer never cast.
func parseVerdictOutcome(outcome string) (loamv1.VerdictOutcome, error) {
	switch outcome {
	case "approve":
		return loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE, nil
	case "disapprove":
		return loamv1.VerdictOutcome_VERDICT_OUTCOME_DISAPPROVE, nil
	case "neutral":
		return loamv1.VerdictOutcome_VERDICT_OUTCOME_NEUTRAL, nil
	default:
		return loamv1.VerdictOutcome_VERDICT_OUTCOME_UNSPECIFIED, newUsageError("work verdict requires --outcome=approve|disapprove|neutral")
	}
}

// publishVerdict reads the staged batch, submits it, and only then clears
// the staging area.
//
// The ordering is the whole design of this function, and it is not
// symmetric. Publish-then-clear risks re-publishing if the clear fails
// (the comments are on the server, but a re-run would send them again);
// clear-then-publish risks LOSING the batch if the publish fails (the
// comments are gone locally and were never recorded anywhere). The second
// is unrecoverable and the first is not, so this publishes first — matching
// docs/cli-spec.md's "publishes … and clears", and the same reasoning
// docs/cli-spec.md applies to a rejected verdict ("the staged items remain
// until --discarded — no automatic cleanup").
//
// A failed clear is therefore reported loudly rather than swallowed: the
// verdict really was published, so returning success would leave the agent
// holding a staging area it believes is unpublished, and the next `work
// verdict` would duplicate every comment. The error names that state
// explicitly and exits non-zero.
func publishVerdict(ctx context.Context, deps *Deps, repo, workBranch string, outcome loamv1.VerdictOutcome) error {
	store, err := openStagingStore(deps.workspace, repo, workBranch)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	items, err := store.list()
	if err != nil {
		return err
	}
	req := &loamv1.SubmitVerdictRequest{
		Repo:             repo,
		WorkBranch:       workBranch,
		Outcome:          outcome,
		Comments:         verdictComments(items),
		ResolveThreadIds: resolveThreadIDs(items),
	}
	resp, err := deps.connect.WorkBranch().SubmitVerdict(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("submitting a verdict on work branch %s/%s: %w", repo, workBranch, err)
	}
	published := resp.Msg.GetPublished()
	if err := store.clear(); err != nil {
		deps.logger.Error("verdict published but the staging area could not be cleared", "repo", repo, "work_branch", workBranch, "published", published, "error", err)
		return fmt.Errorf("the verdict was published on %s/%s (%d comment(s) are now visible) but the local staging area could not be cleared; re-running `loam work verdict` would publish them a second time — discard them with `loam work comment --discard` first: %w", repo, workBranch, published, err)
	}
	return deps.encoder.Encode(verdictOutput{
		Repo:       repo,
		WorkBranch: workBranch,
		Outcome:    verdictOutcomeString(resp.Msg.GetOutcome()),
		Published:  published,
	})
}

// verdictComments converts the staged items that carry a body into the
// request's new-thread comments, preserving staging order so the published
// threads appear in the order the reviewer wrote them.
//
// A resolve-only item contributes nothing here: it has no body, and the
// server rejects a bodyless comment as an invalid argument. Its resolve
// target still travels, via resolveThreadIDs.
func verdictComments(items []stagedItem) []*loamv1.VerdictComment {
	comments := make([]*loamv1.VerdictComment, 0, len(items))
	for _, item := range items {
		if item.Body == "" {
			continue
		}
		comments = append(comments, &loamv1.VerdictComment{Anchor: stagedAnchor(item), Body: item.Body})
	}
	return comments
}

// stagedAnchor renders a staged item's file/line anchor, or nil for a
// top-level thread. A file with no line is a whole-file anchor (proto's
// FileLine.line is optional: "Absent = the whole file / no specific line"),
// which is why line 0 is sent as no line rather than as line zero — there is
// no line 0 in a file, and `work comment` stores exactly 0 for "unanchored
// within the file".
func stagedAnchor(item stagedItem) *loamv1.FileLine {
	if item.File == "" {
		return nil
	}
	anchor := &loamv1.FileLine{File: item.File}
	if item.Line != 0 {
		line := item.Line
		anchor.Line = &line
	}
	return anchor
}

// resolveThreadIDs collects the thread ids the staged batch asks to resolve.
// An item may carry both a body and a resolve target (docs/cli-spec.md ->
// comment (add): "--resolve may accompany a new comment"), in which case it
// contributes to both this list and verdictComments — one new thread plus
// one resolution, published in the same transaction.
func resolveThreadIDs(items []stagedItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.Resolve == "" {
			continue
		}
		ids = append(ids, item.Resolve)
	}
	return ids
}
