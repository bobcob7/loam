package cli

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/pflag"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// The work-branch READ commands: `work list`, `work show`, `work diff`,
// `work comments`, and `work verdicts` (docs/cli-spec.md -> list, show,
// diff, comments (get), verdicts). Every one of them is state-ungated --
// "Reads always work" (docs/cli-spec.md -> State gates) -- so none of them
// inspects the work branch's state before acting.
//
// All but one are a single read RPC plus an encode. The exception is
// `comments --staged`, which never reaches the review RPCs at all: staged
// items live only in the caller's local .loam staging area, which is
// exactly what makes "not visible until submitted" true (docs/cli-spec.md
// -> comments (get); internal/handler/workbranch/review.go -> ListComments:
// "Staged comments are never returned here and cannot be").

// --- work list ---

// workListFlags holds the parsed values of `work list`'s six filter flags.
// They travel as one struct rather than six return values simply because
// there are six of them; unlike commentFlags, no combination of them is
// illegal -- filters compose.
type workListFlags struct {
	repo           *string
	author         *string
	target         *string
	awaitingReview *bool
	state          *string
	limit          *int
}

// newWorkListFlags builds the pflag.FlagSet for `loam work list [--repo
// <repo>] [--author <id>] [--target <branch>] [--awaiting-review] [--state
// <state>] [--limit <n>]` (see docs/cli-spec.md -> work list), plus the
// parsed values.
func newWorkListFlags() (*pflag.FlagSet, *workListFlags) {
	fs := newFlagSet("work list")
	f := &workListFlags{
		repo:           fs.String("repo", "", "limit to one enrolled repo"),
		author:         fs.String("author", "", "limit to work branches authored by this agent identifier"),
		target:         fs.String("target", "", "limit to work branches targeting this branch"),
		awaitingReview: fs.Bool("awaiting-review", false, "limit to work branches awaiting the caller's verdict"),
		state:          fs.String("state", "reviewable", "draft, reviewable, reviewed, complete, or closed"),
		limit:          fs.Int("limit", 100, "maximum number of work branches to return"),
	}
	return fs, f
}

// workListRow is one row of `work list`'s results (docs/cli-spec.md -> work
// list -> Output). Deliberately narrower than `show`: no description, since
// a list of full descriptions is exactly the response bloat `show` exists
// to keep separate (proto/loam/v1/common.proto -> WorkBranch.description:
// "Omitted in list summaries").
type workListRow struct {
	Repo   string `json:"repo"`
	Name   string `json:"name"`
	Target string `json:"target"`
	Title  string `json:"title"`
	Author string `json:"author"`
	State  string `json:"state"`
}

// workListOutput is `work list`'s envelope (docs/cli-spec.md -> work list ->
// Output). It is NOT a bare array: `truncated` reports that the server cut
// the result short, which a bare array could not distinguish from a
// genuinely complete one.
type workListOutput struct {
	Truncated bool          `json:"truncated"`
	Results   []workListRow `json:"results"`
}

// workListRowsFrom converts the proto work branches into list rows. The
// slice is always non-nil so an empty result encodes as `[]`, not `null` --
// an empty result is a normal exit 0 (docs/cli-spec.md -> work list ->
// Errors) and must parse as an empty list for the agent reading it.
func workListRowsFrom(branches []*loamv1.WorkBranch) []workListRow {
	rows := make([]workListRow, 0, len(branches))
	for _, wb := range branches {
		rows = append(rows, workListRow{
			Repo:   wb.GetRepo(),
			Name:   wb.GetName(),
			Target: wb.GetTarget(),
			Title:  wb.GetTitle(),
			Author: wb.GetAuthor(),
			State:  workBranchStateString(wb.GetState()),
		})
	}
	return rows
}

// workBranchStateFrom parses a --state flag value into its proto enum, the
// inverse of workBranchStateString. ok is false for anything outside the
// five documented states -- including the empty string, which is reachable
// only by explicitly passing `--state=`, never by omitting the flag (its
// default is "reviewable"). Refusing it rather than silently treating it as
// "use the default" keeps `--state=<typo>` from quietly listing something
// other than what was asked for.
func workBranchStateFrom(s string) (loamv1.WorkBranchState, bool) {
	switch s {
	case "draft":
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT, true
	case "reviewable":
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE, true
	case "reviewed":
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED, true
	case "complete":
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_COMPLETE, true
	case "closed":
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED, true
	default:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_UNSPECIFIED, false
	}
}

// workListRequest validates `work list`'s flags and builds the RPC request.
// Every rejection here is a bad filter value (exit 2, docs/cli-spec.md ->
// work list -> Errors) decided from the arguments alone, so a malformed
// invocation never reaches the network.
//
// Only the filters actually given are sent: ListWorkBranchesRequest's
// repo/author/target are proto optional, and sending an empty string would
// filter for work branches whose author is literally "" rather than not
// filtering at all. --state is the exception -- it always has a value (its
// flag default is "reviewable", matching the server's own default for the
// unset field), so it is always sent explicitly.
func workListRequest(f *workListFlags) (*loamv1.ListWorkBranchesRequest, error) {
	if *f.limit < 0 {
		return nil, newUsageError("work list: --limit must not be negative")
	}
	state, ok := workBranchStateFrom(*f.state)
	if !ok {
		return nil, newUsageCLIError(fmt.Sprintf("work list: --state %q is not one of draft, reviewable, reviewed, complete, or closed", *f.state), nil)
	}
	req := &loamv1.ListWorkBranchesRequest{
		AwaitingReview: *f.awaitingReview,
		State:          &state,
		Page:           &loamv1.Page{Limit: uint32(*f.limit)},
	}
	if *f.repo != "" {
		req.Repo = f.repo
	}
	if *f.author != "" {
		req.Author = f.author
	}
	if *f.target != "" {
		req.Target = f.target
	}
	return req, nil
}

// runWorkList implements `loam work list [--repo <repo>] [--author <id>]
// [--target <branch>] [--awaiting-review] [--state <state>] [--limit <n>]`
// (docs/cli-spec.md -> work list). It takes no positional arguments and
// infers nothing from the workspace: it lists across every enrolled repo by
// default, so there is no identifier to resolve. A server rejection --
// NotFound for an unenrolled --repo -- reaches mapCommandError unchanged
// via the %w wrap below.
func runWorkList(ctx context.Context, deps *Deps, args []string) error {
	fs, f := newWorkListFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) > 0 {
		return newUsageError("work list takes no positional arguments")
	}
	req, err := workListRequest(f)
	if err != nil {
		return err
	}
	resp, err := deps.connect.WorkBranch().ListWorkBranches(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("listing work branches: %w", err)
	}
	return deps.encoder.Encode(workListOutput{
		Truncated: resp.Msg.GetTruncated(),
		Results:   workListRowsFrom(resp.Msg.GetWorkBranches()),
	})
}

// --- work show ---

// workShowOutput is `work show`'s shape (docs/cli-spec.md -> show): the
// work branch's metadata, title, description, and state. The diff and the
// comment threads are deliberately absent -- they are `diff` and `comments`,
// "to keep each response small".
//
// Round is GetWorkBranchResponse's Round message (proto/loam/v1/workbranch.proto),
// populated by internal/handler/workbranch.GetWorkBranch from the same
// RoundStore.CurrentRound lookup RequestReview and ReplyToThread use. It is
// omitted from the JSON entirely -- not rendered as `"round": {"number": 0}`
// -- for a branch with no round yet (still DRAFT), matching UpstreamPRURL's
// presence/absence convention below; loam-0pj.10 refused to fabricate this
// field before the proto carried it, and a zeroed Round would be the same
// fabrication under a different name.
type workShowOutput struct {
	Repo        string `json:"repo"`
	Name        string `json:"name"`
	Target      string `json:"target"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Author      string `json:"author"`
	// UpstreamPRURL is omitted entirely until the admin has accepted the
	// proposal and Loam has opened the pull request, matching the proto's
	// `optional string upstream_pr_url = 8`. It is a pointer rather than a
	// string with omitempty so that "not yet accepted" and "accepted, but
	// the server sent an empty URL" stay distinguishable -- the latter
	// would be a server bug, and collapsing the two would hide it.
	//
	// Before this field existed an agent whose proposal had been accepted
	// had no CLI route to its own pull request at all: the URL was
	// reachable only through the admin ProposalService queue, which agents
	// cannot call (loam-ls7u).
	UpstreamPRURL *string `json:"upstream_pr_url,omitempty"`
	// Round is nil (and therefore omitted, via omitempty) for a branch with
	// no review round yet; see the type doc comment above.
	Round *workShowRoundOutput `json:"round,omitempty"`
	// LatestVerdict is the branch's single most recent verdict overall,
	// fetched via a second RPC (ListVerdicts) rather than derived from
	// `state`. `state` reports workflow POSITION, not outcome --
	// internal/reviewpublish/publish.go's publishInTx flips
	// reviewable -> reviewed on ANY verdict outcome, approve or
	// disapprove alike -- so an agent polling `show` alone could
	// otherwise read "reviewed" as "approved" (loam-o718). It stays a
	// client-side merge rather than a WorkBranch proto field specifically
	// to avoid a field-9 collision with loam-giq.11's conflict/
	// upstream_drift fields on the same message.
	//
	// Nil (and therefore omitted via omitempty) when the branch has no
	// verdicts yet, matching Round/UpstreamPRURL's presence/absence
	// convention above -- a zeroed object would be a fabrication under a
	// different name (loam-0pj.10).
	LatestVerdict *workShowVerdictOutput `json:"latest_verdict,omitempty"`
}

// workShowVerdictOutput is workShowOutput's latest_verdict shape. All four
// fields travel together deliberately -- Outcome alone, without Stale,
// would let a stale approve read as a live one, replacing one foot-gun
// (state) with a worse one (loam-o718).
type workShowVerdictOutput struct {
	Outcome  string `json:"outcome"`
	Reviewer string `json:"reviewer"`
	Round    uint32 `json:"round"`
	Stale    bool   `json:"stale"`
}

// workShowLatestVerdict picks the single most recent verdict overall from
// ListVerdicts's response -- not a per-reviewer roll-up and not filtered to
// non-stale ones. ListVerdicts already collapses to one row per reviewer
// (their latest round) but NOT to one row per round: the schema allows many
// reviewers per round (`UNIQUE (round_id, reviewer)`,
// internal/db/migrations/files/0001_init.up.sql:113), so several rows can
// share the branch's highest round number.
//
// VerdictSummary carries no timestamp, but list position still encodes
// recency: the server orders `ORDER BY r.number DESC, v.created_at ASC`
// (internal/db/queries/review_rounds.sql:50) -- newest round first, and
// OLDEST-CAST FIRST within a round. So among rows tied at the maximum
// round, the LAST one in the response is the most recently cast, and `>=`
// below (not `>`) is what selects it; `>` would silently keep the oldest
// vote of that round instead, letting one reviewer's stale-looking earlier
// verdict outrank another's live one just because it was cast first.
func workShowLatestVerdict(verdicts []*loamv1.VerdictSummary) *loamv1.VerdictSummary {
	var latest *loamv1.VerdictSummary
	for _, v := range verdicts {
		if latest == nil || v.GetRound() >= latest.GetRound() {
			latest = v
		}
	}
	return latest
}

// workShowRoundOutput is workShowOutput's round shape, matching
// GetWorkBranchResponse_Round field-for-field and docs/cli-spec.md -> show's
// `"round": { "number": 2, "requested_by": "..." }` example.
type workShowRoundOutput struct {
	Number      uint32 `json:"number"`
	RequestedBy string `json:"requested_by"`
}

// runWorkShow implements `loam work show [repo] [work-branch]`
// (docs/cli-spec.md -> show). An unresolvable identifier is a usage error
// (exit 2) from resolveWorkBranchIdentity; a server NotFound is exit 3 via
// the %w wrap below.
//
// It makes a second RPC, ListVerdicts, purely to populate LatestVerdict --
// the client-side merge loam-o718 chose over a WorkBranch proto field. That
// choice is deliberately not defended with a graceful-degradation branch: an
// error from ListVerdicts is a real error and surfaces as one, because
// ListVerdicts and GetWorkBranch are gated by the same CapabilityWorkRead
// (internal/handler/workbranch/review.go:74, workbranch.go:331), so no role
// that can reach this far can have GetWorkBranch succeed and ListVerdicts
// fail on permissions.
func runWorkShow(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work show")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work show", positional); err != nil {
		return err
	}
	repo, workBranch, err := resolveWorkBranchIdentity(deps.workspace, positional)
	if err != nil {
		return err
	}
	resp, err := deps.connect.WorkBranch().GetWorkBranch(ctx, connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: repo, WorkBranch: workBranch}))
	if err != nil {
		return fmt.Errorf("fetching work branch %s/%s: %w", repo, workBranch, err)
	}
	wb := resp.Msg.GetWorkBranch()
	out := workShowOutput{
		Repo:        wb.GetRepo(),
		Name:        wb.GetName(),
		Target:      wb.GetTarget(),
		Title:       wb.GetTitle(),
		Description: wb.GetDescription(),
		State:       workBranchStateString(wb.GetState()),
		Author:      wb.GetAuthor(),
	}
	// Presence is read off the proto's optional field directly rather than
	// through GetUpstreamPrUrl(), whose zero value cannot distinguish
	// "absent" from "present and empty".
	if wb.UpstreamPrUrl != nil {
		url := wb.GetUpstreamPrUrl()
		out.UpstreamPRURL = &url
	}
	if round := resp.Msg.GetRound(); round != nil {
		out.Round = &workShowRoundOutput{Number: round.GetNumber(), RequestedBy: round.GetRequestedBy()}
	}
	verdictsResp, err := deps.connect.WorkBranch().ListVerdicts(ctx, connect.NewRequest(&loamv1.ListVerdictsRequest{Repo: repo, WorkBranch: workBranch}))
	if err != nil {
		return fmt.Errorf("listing verdicts for work branch %s/%s: %w", repo, workBranch, err)
	}
	if latest := workShowLatestVerdict(verdictsResp.Msg.GetVerdicts()); latest != nil {
		out.LatestVerdict = &workShowVerdictOutput{
			Outcome:  verdictOutcomeString(latest.GetOutcome()),
			Reviewer: latest.GetReviewer(),
			Round:    latest.GetRound(),
			Stale:    latest.GetStale(),
		}
	}
	return deps.encoder.Encode(out)
}

// --- work diff ---

// workDiffOutput wraps the unified diff in a field rather than printing it
// bare, so the response is a document in the active LOAM_OUTPUT_FORMAT like
// every other command's (docs/cli-spec.md -> diff: "as a field in the active
// LOAM_OUTPUT_FORMAT (e.g. { "diff": "…" } for JSON)").
type workDiffOutput struct {
	Diff string `json:"diff"`
}

// runWorkDiff implements `loam work diff [repo] [work-branch]`
// (docs/cli-spec.md -> diff).
//
// Beyond the documented exit 3 (no such work branch) and exit 2
// (unresolvable identifier), GetWorkBranchDiff can answer
// FailedPrecondition when the mirror cannot support the range: the work
// branch's ref or its target's is missing, or the two share no merge base.
// Since loam-5iu that is a genuine edge case rather than the common one --
// `work start` now creates the ref server-side, so a freshly started
// branch diffs cleanly (empty) instead of failing. classifyConnectError
// maps it to precondition_failed / exit 2 carrying the server's own
// message, reported honestly rather than suppressed: a fabricated empty
// diff would read as "no changes".
func runWorkDiff(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work diff")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work diff", positional); err != nil {
		return err
	}
	repo, workBranch, err := resolveWorkBranchIdentity(deps.workspace, positional)
	if err != nil {
		return err
	}
	resp, err := deps.connect.WorkBranch().GetWorkBranchDiff(ctx, connect.NewRequest(&loamv1.GetWorkBranchDiffRequest{Repo: repo, WorkBranch: workBranch}))
	if err != nil {
		return fmt.Errorf("fetching diff for work branch %s/%s: %w", repo, workBranch, err)
	}
	return deps.encoder.Encode(workDiffOutput{Diff: resp.Msg.GetDiff()})
}

// --- work comments ---

// newWorkCommentsFlags builds the pflag.FlagSet for `loam work comments
// [repo] [work-branch] [--staged]`, plus the parsed --staged value.
func newWorkCommentsFlags() (*pflag.FlagSet, *bool) {
	fs := newFlagSet("work comments")
	staged := fs.Bool("staged", false, "return the caller's staged comments instead of published threads")
	return fs, staged
}

// commentOutput is one comment within a published thread (docs/cli-spec.md
// -> comments (get) -> Output). round is the comment's OWN round, which for
// a reply may be later than the thread's (proto/loam/v1/common.proto ->
// Comment.round: "never inherited from Thread.round").
type commentOutput struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	Round  uint32 `json:"round"`
}

// threadOutput is one published thread (docs/cli-spec.md -> comments (get)
// -> Output). File/Line are omitted when absent so a top-level thread does
// not report a phantom `"file": ""` anchor and a whole-file anchor does not
// report a line 0 an agent would read as a real line number. round is the
// round the thread was RAISED in and never changes.
type threadOutput struct {
	ID       string          `json:"id"`
	Resolved bool            `json:"resolved"`
	File     string          `json:"file,omitempty"`
	Line     uint32          `json:"line,omitempty"`
	Round    uint32          `json:"round"`
	Comments []commentOutput `json:"comments"`
}

// threadOutputsFrom converts proto threads into their output shape. Both
// slices are non-nil so an empty result encodes as `[]` rather than `null`.
func threadOutputsFrom(threads []*loamv1.Thread) []threadOutput {
	rows := make([]threadOutput, 0, len(threads))
	for _, thread := range threads {
		comments := make([]commentOutput, 0, len(thread.GetComments()))
		for _, comment := range thread.GetComments() {
			comments = append(comments, commentOutput{Author: comment.GetAuthor(), Body: comment.GetBody(), Round: comment.GetRound()})
		}
		rows = append(rows, threadOutput{
			ID:       thread.GetId(),
			Resolved: thread.GetResolved(),
			File:     thread.GetAnchor().GetFile(),
			Line:     thread.GetAnchor().GetLine(),
			Round:    thread.GetRound(),
			Comments: comments,
		})
	}
	return rows
}

// runWorkComments implements `loam work comments [repo] [work-branch]
// [--staged]` (docs/cli-spec.md -> comments (get)).
//
// The two modes read from entirely different places, which IS the
// behaviour: without --staged this returns only what the server has
// published, so another agent's -- or the caller's own -- staged items are
// invisible; with --staged it reads the caller's local staging area and
// never asks the server for comments at all.
func runWorkComments(ctx context.Context, deps *Deps, args []string) error {
	fs, staged := newWorkCommentsFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work comments", positional); err != nil {
		return err
	}
	repo, workBranch, err := resolveWorkBranchIdentity(deps.workspace, positional)
	if err != nil {
		return err
	}
	if *staged {
		return encodeStagedComments(ctx, deps, repo, workBranch)
	}
	threads, err := listAllThreads(ctx, deps.connect.WorkBranch(), repo, workBranch)
	if err != nil {
		return err
	}
	return deps.encoder.Encode(threadOutputsFrom(threads))
}

// listAllThreads reads every page of a work branch's published threads,
// following pagination rather than trusting the first response for the same
// reason findThread does: `comments` has no --limit in docs/cli-spec.md, so
// it promises the threads on the work branch, not the first server page of
// them. A server NotFound on the first call is the work branch not existing
// (exit 3) and reaches mapCommandError unchanged via the %w wrap.
func listAllThreads(ctx context.Context, client WorkBranchClient, repo, workBranch string) ([]*loamv1.Thread, error) {
	var all []*loamv1.Thread
	for offset := uint32(0); ; {
		req := &loamv1.ListCommentsRequest{Repo: repo, WorkBranch: workBranch, Page: &loamv1.Page{Offset: offset}}
		resp, err := client.ListComments(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, fmt.Errorf("listing comment threads on %s/%s: %w", repo, workBranch, err)
		}
		threads := resp.Msg.GetThreads()
		all = append(all, threads...)
		offset += uint32(len(threads))
		if len(threads) == 0 || offset >= resp.Msg.GetPageInfo().GetTotal() {
			return all, nil
		}
	}
}

// encodeStagedComments reports the caller's locally staged items --
// `comments --staged`.
//
// It checks the work branch exists first, for the same reason
// checkCommentTargets does before staging: the staging area is keyed by
// (repo, work-branch, agent) and a mistyped work branch names a directory
// that is simply empty, so without this check a typo would report "nothing
// staged" -- indistinguishable from a correct branch with nothing staged --
// instead of the exit 3 docs/cli-spec.md -> comments (get) -> Errors
// promises. Nothing about the comments themselves is fetched: staged items
// are not on the server (internal/handler/workbranch/review.go ->
// ListComments) and asking for them there would be meaningless.
func encodeStagedComments(ctx context.Context, deps *Deps, repo, workBranch string) error {
	req := &loamv1.GetWorkBranchRequest{Repo: repo, WorkBranch: workBranch}
	if _, err := deps.connect.WorkBranch().GetWorkBranch(ctx, connect.NewRequest(req)); err != nil {
		return fmt.Errorf("checking work branch %s/%s exists: %w", repo, workBranch, err)
	}
	store, err := openStagingStore(deps.workspace, repo, workBranch)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	items, err := store.list()
	if err != nil {
		return err
	}
	return deps.encoder.Encode(stagedCommentOutputsFrom(items))
}

// stagedCommentOutputsFrom renders staged items in the shape `work comment`
// reports for a single one (docs/cli-spec.md -> comments (get): "returns
// those staged items instead (the shape produced by comment)"), reusing
// stagedCommentOutput rather than declaring a near-identical second type.
// Staged is true on every row: each item listed here IS still staged, which
// is what distinguishes these rows from the published threads the same
// command returns without --staged. The slice is non-nil so nothing staged
// encodes as `[]`.
func stagedCommentOutputsFrom(items []stagedItem) []stagedCommentOutput {
	rows := make([]stagedCommentOutput, 0, len(items))
	for _, item := range items {
		rows = append(rows, stagedCommentOutput{
			Staged:  true,
			ID:      item.ID,
			File:    item.File,
			Line:    item.Line,
			Body:    item.Body,
			Resolve: item.Resolve,
		})
	}
	return rows
}

// --- work verdicts ---

// workVerdictOutput is one row of `work verdicts` (docs/cli-spec.md ->
// verdicts -> Output).
type workVerdictOutput struct {
	Reviewer string `json:"reviewer"`
	Outcome  string `json:"outcome"`
	Round    uint32 `json:"round"`
	Stale    bool   `json:"stale"`
}

// verdictOutcomeString renders a loamv1.VerdictOutcome as the lowercase
// string docs/cli-spec.md's output examples use. VERDICT_OUTCOME_UNSPECIFIED
// and any unrecognized value render as "unspecified" -- the server rejects
// an unspecified outcome at SubmitVerdict (protoToOutcome), so no stored
// verdict can carry one.
//
// Shared by the read commands (loam-0pj.10) and by `work verdict`
// (loam-0pj.13), which were written concurrently and each defined an
// identical copy; the duplicate was removed at merge. `work verdict`
// applies this to the SERVER's echoed outcome rather than to its own
// --outcome flag, so the outcome it reports is the one actually recorded.
func verdictOutcomeString(o loamv1.VerdictOutcome) string {
	switch o {
	case loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE:
		return "approve"
	case loamv1.VerdictOutcome_VERDICT_OUTCOME_DISAPPROVE:
		return "disapprove"
	case loamv1.VerdictOutcome_VERDICT_OUTCOME_NEUTRAL:
		return "neutral"
	default:
		return "unspecified"
	}
}

// workVerdictOutputsFrom converts the proto verdict summaries into rows,
// non-nil so no verdicts encodes as `[]`.
//
// It copies `stale` straight through and derives nothing: staleness is
// computed server-side from each verdict's round against the branch's
// current one (internal/handler/workbranch/review.go -> ListVerdicts:
// "Staleness is DERIVED, never stored"), and a second derivation here --
// e.g. "stale unless round == max(round)" -- would be a parallel mechanism
// that could disagree with the authoritative one.
func workVerdictOutputsFrom(verdicts []*loamv1.VerdictSummary) []workVerdictOutput {
	rows := make([]workVerdictOutput, 0, len(verdicts))
	for _, v := range verdicts {
		rows = append(rows, workVerdictOutput{
			Reviewer: v.GetReviewer(),
			Outcome:  verdictOutcomeString(v.GetOutcome()),
			Round:    v.GetRound(),
			Stale:    v.GetStale(),
		})
	}
	return rows
}

// runWorkVerdicts implements `loam work verdicts [repo] [work-branch]`
// (docs/cli-spec.md -> verdicts). The response is already one row per
// reviewer -- their latest verdict, with the stale flag answering "does this
// agent currently approve?" (internal/handler/workbranch/review.go ->
// ListVerdicts) -- so this neither de-duplicates nor filters: dropping stale
// rows here would contradict "including those marked stale".
func runWorkVerdicts(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work verdicts")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work verdicts", positional); err != nil {
		return err
	}
	repo, workBranch, err := resolveWorkBranchIdentity(deps.workspace, positional)
	if err != nil {
		return err
	}
	resp, err := deps.connect.WorkBranch().ListVerdicts(ctx, connect.NewRequest(&loamv1.ListVerdictsRequest{Repo: repo, WorkBranch: workBranch}))
	if err != nil {
		return fmt.Errorf("listing verdicts on work branch %s/%s: %w", repo, workBranch, err)
	}
	return deps.encoder.Encode(workVerdictOutputsFrom(resp.Msg.GetVerdicts()))
}
