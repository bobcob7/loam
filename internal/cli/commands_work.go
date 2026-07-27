package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// requireWorkBranchArgs validates the [repo] [work-branch] positional
// convention shared by most `work` subcommands (see docs/cli-spec.md ->
// Work Branches): both are optional, inferred from the workspace when
// omitted, but at most two may be given.
func requireWorkBranchArgs(command string, positional []string) error {
	if len(positional) > 2 {
		return newUsageError(command + " takes at most a repo and a work branch")
	}
	return nil
}

// workStartOutput is `work start`'s success shape (docs/cli-spec.md ->
// start): a freshly created work branch has no title yet, so unlike
// workBranchOutput below there is no title field at all here.
type workStartOutput struct {
	Repo   string `json:"repo"`
	Name   string `json:"name"`
	Target string `json:"target"`
	State  string `json:"state"`
}

// workBranchOutput is the shared success shape for `work set` and `work
// request-review` (docs/cli-spec.md -> set, request-review): the updated
// work branch, including its title.
type workBranchOutput struct {
	Repo   string `json:"repo"`
	Name   string `json:"name"`
	Target string `json:"target"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

// workBranchOutputFrom converts a loamv1.WorkBranch response into
// workBranchOutput.
func workBranchOutputFrom(wb *loamv1.WorkBranch) workBranchOutput {
	return workBranchOutput{
		Repo:   wb.GetRepo(),
		Name:   wb.GetName(),
		Target: wb.GetTarget(),
		Title:  wb.GetTitle(),
		State:  workBranchStateString(wb.GetState()),
	}
}

// workBranchStateString renders a loamv1.WorkBranchState as the lowercase
// string docs/cli-spec.md's output examples use ("draft", "reviewable",
// "reviewed", "complete", "closed"). WORK_BRANCH_STATE_UNSPECIFIED and any
// unrecognized value render as "unspecified" -- the server's own state
// gates (internal/workbranchstore, internal/handler/workbranch) rule out
// ever seeing that in a real response.
func workBranchStateString(s loamv1.WorkBranchState) string {
	switch s {
	case loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT:
		return "draft"
	case loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE:
		return "reviewable"
	case loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED:
		return "reviewed"
	case loamv1.WorkBranchState_WORK_BRANCH_STATE_COMPLETE:
		return "complete"
	case loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED:
		return "closed"
	default:
		return "unspecified"
	}
}

// readStdin reads all of stdin and trims surrounding whitespace. Trimming
// matters because docs/cli-spec.md -> set treats "empty stdin" as "leave
// the description unchanged" -- a bare trailing newline from `echo` (rather
// than `printf`) must not itself count as a provided, non-empty
// description. `work comment` depends on the same distinction: a lone
// newline must not satisfy its required body, nor turn a bare `--discard`
// into a conflicting new-thread invocation.
func readStdin(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("reading stdin: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// runWorkStart implements `loam work start <repo> <from>` (docs/cli-spec.md
// -> start). Both are required — there is no default base branch. Creates
// a randomly named work branch server-side via
// WorkBranchService.CreateWorkBranch; a *connect.Error the server returns
// (NotFound for an unenrolled repo, InvalidArgument for an invalid target)
// reaches mapCommandError unchanged via the %w wrap below, so it still
// classifies to the right exit code.
func runWorkStart(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work start")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) != 2 {
		return newUsageError("work start requires exactly a repo and a from argument")
	}
	repo, from := positional[0], positional[1]
	resp, err := deps.connect.WorkBranch().CreateWorkBranch(ctx, connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: repo, From: from}))
	if err != nil {
		return fmt.Errorf("starting work branch in %s from %s: %w", repo, from, err)
	}
	wb := resp.Msg.GetWorkBranch()
	return deps.encoder.Encode(workStartOutput{Repo: wb.GetRepo(), Name: wb.GetName(), Target: wb.GetTarget(), State: workBranchStateString(wb.GetState())})
}

// newWorkSetFlags builds the flag.FlagSet for `loam work set [repo]
// [work-branch] [--title <title>]`, plus the parsed --title value.
func newWorkSetFlags() (fs *flag.FlagSet, title *string) {
	fs = newFlagSet("work set")
	title = fs.String("title", "", "new title for the work branch")
	return fs, title
}

// runWorkSet implements `loam work set [repo] [work-branch] [--title
// <title>]` (optional description read from stdin; docs/cli-spec.md ->
// set). At least one of --title or a non-empty stdin is required (exit 2
// otherwise). Only the fields actually provided are sent, so
// UpdateWorkBranch's proto optional semantics ("leave unset to keep the
// current value") apply as documented. A server-side rejection --
// terminal-state precondition_failed, or not_found -- reaches
// mapCommandError unchanged via the %w wrap below.
func runWorkSet(ctx context.Context, deps *Deps, args []string) error {
	fs, title := newWorkSetFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work set", positional); err != nil {
		return err
	}
	repo, workBranch, err := resolveWorkBranchIdentity(deps.workspace, positional)
	if err != nil {
		return err
	}
	description, err := readStdin(deps.stdin)
	if err != nil {
		return err
	}
	if *title == "" && description == "" {
		return newUsageCLIError("work set requires --title or a non-empty description on stdin", nil)
	}
	req := &loamv1.UpdateWorkBranchRequest{Repo: repo, WorkBranch: workBranch}
	if *title != "" {
		req.Title = title
	}
	if description != "" {
		req.Description = &description
	}
	resp, err := deps.connect.WorkBranch().UpdateWorkBranch(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("updating work branch %s/%s: %w", repo, workBranch, err)
	}
	return deps.encoder.Encode(workBranchOutputFrom(resp.Msg.GetWorkBranch()))
}

// runWorkRequestReview implements `loam work request-review [repo]
// [work-branch]` (docs/cli-spec.md -> request-review). No comment argument
// -- the proto's RequestReviewRequest reserves that field; feedback lives
// in comment threads instead. A server-side rejection -- terminal state,
// already reviewable, or missing title/description, all
// precondition_failed -- reaches mapCommandError unchanged via the %w wrap
// below.
func runWorkRequestReview(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work request-review")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work request-review", positional); err != nil {
		return err
	}
	repo, workBranch, err := resolveWorkBranchIdentity(deps.workspace, positional)
	if err != nil {
		return err
	}
	resp, err := deps.connect.WorkBranch().RequestReview(ctx, connect.NewRequest(&loamv1.RequestReviewRequest{Repo: repo, WorkBranch: workBranch}))
	if err != nil {
		return fmt.Errorf("requesting review for work branch %s/%s: %w", repo, workBranch, err)
	}
	return deps.encoder.Encode(workBranchOutputFrom(resp.Msg.GetWorkBranch()))
}

// The work-branch READ commands -- list, show, diff, comments, verdicts --
// live in commands_work_read.go.

// `work reply` lives in commands_work_reply.go and `work verdict` in
// commands_work_verdict.go — the two publishing commands, kept together
// with the staging batch one of them consumes.
