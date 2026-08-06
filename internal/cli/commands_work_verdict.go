package cli

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/pflag"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// newWorkVerdictFlags builds the pflag.FlagSet for `loam work verdict
// [repo] [work-branch] --outcome <approve|disapprove|neutral>`, plus the
// parsed --outcome value.
func newWorkVerdictFlags() (*pflag.FlagSet, *string) {
	fs := newFlagSet("work verdict")
	outcome := fs.String("outcome", "", "approve, disapprove, or neutral")
	return fs, outcome
}

// verdictOutput is `work verdict`'s success shape (docs/cli-spec.md ->
// verdict). Published is the count the SERVER reports, not the number of
// items this process sent: the two differ by design, because a resolve-only
// staged item publishes no comment.
//
// PublishedIDs, ResolvedThreadIDs and StagingDir exist because a bare
// count is unfalsifiable (loam-rgyg). A reviewer who staged ten comments
// and read `"published": 0` had no way to tell "the server rejected them"
// from "this invocation was looking at a different staging area" — and no
// way, across three rounds and eighteen comments, to confirm that what
// went out was what they wrote. The staged ids are the reviewer's OWN
// names for the items (`s1`, `s2`, …, the same ids `work comment` handed
// back and `--list` shows), so the list can be checked against their notes
// without a second call; StagingDir names the directory those ids came
// from, which is the fact that was missing when the count was wrong.
//
// The ids are local staging ids, not server thread ids: SubmitVerdict
// returns only an outcome and a count, so server ids are not available to
// report here. Local ids are the more useful half anyway — they answer
// "did everything I staged go out", which is the question that was
// silently answered wrong.
type verdictOutput struct {
	Repo              string   `json:"repo"`
	WorkBranch        string   `json:"work_branch"`
	Outcome           string   `json:"outcome"`
	Published         uint32   `json:"published"`
	PublishedIDs      []string `json:"published_ids"`
	ResolvedThreadIDs []string `json:"resolved_thread_ids"`
	StagingDir        string   `json:"staging_dir"`
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
	publishedIDs := publishedStagedIDs(items)
	if err := requirePublishedAll(publishedIDs, published, store.location()); err != nil {
		return err
	}
	if err := store.clear(); err != nil {
		deps.logger.Error("verdict published but the staging area could not be cleared", "repo", repo, "work_branch", workBranch, "published", published, "error", err)
		return fmt.Errorf("the verdict was published on %s/%s (%d comment(s) are now visible) but the local staging area could not be cleared; re-running `loam work verdict` would publish them a second time — discard them with `loam work comment --discard` first: %w", repo, workBranch, published, err)
	}
	return deps.encoder.Encode(verdictOutput{
		Repo:              repo,
		WorkBranch:        workBranch,
		Outcome:           verdictOutcomeString(resp.Msg.GetOutcome()),
		Published:         published,
		PublishedIDs:      publishedIDs,
		ResolvedThreadIDs: resolveThreadIDs(items),
		StagingDir:        store.location(),
	})
}

// publishedStagedIDs lists the local staging ids of the items that carried
// a body, in staging order — exactly the items verdictComments turned into
// published threads, so the two are two views of one list and cannot drift.
// A resolve-only item is absent by the same rule that keeps it out of
// verdictComments: it publishes no comment, and reporting its id under
// "published" would overstate what the reviewer's findings amount to. Its
// id still travels, as a thread id, via resolveThreadIDs.
//
// The slice is non-nil so an outcome-only verdict reports `[]`, not
// `null` — "nothing was staged" must not render as "this field is absent"
// in the one output whose job is to be checkable.
func publishedStagedIDs(items []stagedItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.Body == "" {
			continue
		}
		ids = append(ids, item.ID)
	}
	return ids
}

// requirePublishedAll refuses to report success when the server published
// a different number of comments than this invocation sent.
//
// This is the guard loam-rgyg asks for: `work verdict` is irreversible by
// design, so it must never silently narrow its own scope. A count that
// disagrees with the batch means some of the reviewer's findings are not
// on the work branch while the verdict that was supposed to carry them
// is — and the reviewer, reading a success-shaped response, would never
// look again.
//
// Two things follow from WHEN this runs, and both are deliberate. It runs
// after the RPC, because only the server can report what it accepted, so
// the verdict really is published by the time this fires; the message
// therefore leads with that fact rather than implying a rollback the
// server never offers. And it runs BEFORE store.clear(), so a failure here
// leaves every staged item exactly where it was: the reviewer can read
// them back with `work comment --list`, compare against the ids below, and
// decide, rather than being left with nothing to compare.
//
// Note what this cannot catch, and why the staging directory is in the
// message anyway. When a verdict is issued against a staging area that
// never held the comments — the original loam-rgyg failure — sent and
// published are both zero, they agree, and no guard on counts alone can
// see it. Naming the directory that answered is what makes that case
// visible, which is why it appears here and in every success response too.
func requirePublishedAll(publishedIDs []string, published uint32, stagingDir string) error {
	if uint32(len(publishedIDs)) == published {
		return nil
	}
	return fmt.Errorf(
		"the verdict was published, but the server reports %d comment(s) published from the %d staged in %s (%s); those staged comments are still staged locally — read them with `loam work comment --list` and compare before re-submitting, since re-submitting publishes the whole batch again",
		published, len(publishedIDs), stagingDir, strings.Join(publishedIDs, ", "),
	)
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
