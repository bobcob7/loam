package cli

import (
	"context"
	"flag"
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

// runWorkStart implements `loam work start <repo> <from>`. Both are
// required — there is no default base branch.
func runWorkStart(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work start")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) != 2 {
		return newUsageError("work start requires exactly a repo and a from argument")
	}
	return errNotImplemented
}

// newWorkSetFlags builds the flag.FlagSet for `loam work set [repo]
// [work-branch] [--title <title>]`.
func newWorkSetFlags() *flag.FlagSet {
	fs := newFlagSet("work set")
	fs.String("title", "", "new title for the work branch")
	return fs
}

// runWorkSet implements `loam work set [repo] [work-branch] [--title
// <title>]`.
func runWorkSet(ctx context.Context, deps *Deps, args []string) error {
	fs := newWorkSetFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work set", positional); err != nil {
		return err
	}
	return errNotImplemented
}

// runWorkRequestReview implements `loam work request-review [repo]
// [work-branch]`.
func runWorkRequestReview(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work request-review")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work request-review", positional); err != nil {
		return err
	}
	return errNotImplemented
}

// newWorkListFlags builds the flag.FlagSet for `loam work list [--repo
// <repo>] [--author <id>] [--target <branch>] [--awaiting-review] [--state
// <state>] [--limit <n>]` (see docs/cli-spec.md -> work list).
func newWorkListFlags() *flag.FlagSet {
	fs := newFlagSet("work list")
	fs.String("repo", "", "limit to one enrolled repo")
	fs.String("author", "", "limit to work branches authored by this agent identifier")
	fs.String("target", "", "limit to work branches targeting this branch")
	fs.Bool("awaiting-review", false, "limit to work branches awaiting the caller's verdict")
	fs.String("state", "reviewable", "draft, reviewable, reviewed, complete, or closed")
	fs.Int("limit", 100, "maximum number of work branches to return")
	return fs
}

// runWorkList implements `loam work list`.
func runWorkList(ctx context.Context, deps *Deps, args []string) error {
	fs := newWorkListFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) > 0 {
		return newUsageError("work list takes no positional arguments")
	}
	return errNotImplemented
}

// runWorkShow implements `loam work show [repo] [work-branch]`.
func runWorkShow(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work show")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work show", positional); err != nil {
		return err
	}
	return errNotImplemented
}

// runWorkDiff implements `loam work diff [repo] [work-branch]`.
func runWorkDiff(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work diff")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work diff", positional); err != nil {
		return err
	}
	return errNotImplemented
}

// newWorkCommentsFlags builds the flag.FlagSet for `loam work comments
// [repo] [work-branch] [--staged]`.
func newWorkCommentsFlags() *flag.FlagSet {
	fs := newFlagSet("work comments")
	fs.Bool("staged", false, "return the caller's staged comments instead of published threads")
	return fs
}

// runWorkComments implements `loam work comments [repo] [work-branch]
// [--staged]`.
func runWorkComments(ctx context.Context, deps *Deps, args []string) error {
	fs := newWorkCommentsFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work comments", positional); err != nil {
		return err
	}
	return errNotImplemented
}

// runWorkVerdicts implements `loam work verdicts [repo] [work-branch]`.
func runWorkVerdicts(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work verdicts")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work verdicts", positional); err != nil {
		return err
	}
	return errNotImplemented
}

// newWorkCommentFlags builds the flag.FlagSet for `loam work comment
// [repo] [work-branch] [--file <path> --line <n>] [--resolve <thread-id>]
// [--edit <staged-id>] [--discard <staged-id>]`.
func newWorkCommentFlags() *flag.FlagSet {
	fs := newFlagSet("work comment")
	fs.String("file", "", "anchor the new thread to this file")
	fs.Int("line", 0, "anchor the new thread to this line")
	fs.String("resolve", "", "mark this thread id resolved")
	fs.String("edit", "", "replace the body of this staged comment id")
	fs.String("discard", "", "remove this staged comment id")
	return fs
}

// runWorkComment implements `loam work comment [repo] [work-branch]
// [--file <path> --line <n>] [--resolve <thread-id>] [--edit <staged-id>]
// [--discard <staged-id>]`.
func runWorkComment(ctx context.Context, deps *Deps, args []string) error {
	fs := newWorkCommentFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work comment", positional); err != nil {
		return err
	}
	return errNotImplemented
}

// newWorkReplyFlags builds the flag.FlagSet for `loam work reply [repo]
// [work-branch] --thread <thread-id>`, plus the parsed --thread value.
func newWorkReplyFlags() (*flag.FlagSet, *string) {
	fs := newFlagSet("work reply")
	thread := fs.String("thread", "", "the thread to reply to")
	return fs, thread
}

// runWorkReply implements `loam work reply [repo] [work-branch] --thread
// <thread-id>`. --thread is required.
func runWorkReply(ctx context.Context, deps *Deps, args []string) error {
	fs, thread := newWorkReplyFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work reply", positional); err != nil {
		return err
	}
	if *thread == "" {
		return newUsageError("work reply requires --thread")
	}
	return errNotImplemented
}

// newWorkVerdictFlags builds the flag.FlagSet for `loam work verdict
// [repo] [work-branch] --outcome <approve|disapprove|neutral>`, plus the
// parsed --outcome value.
func newWorkVerdictFlags() (*flag.FlagSet, *string) {
	fs := newFlagSet("work verdict")
	outcome := fs.String("outcome", "", "approve, disapprove, or neutral")
	return fs, outcome
}

// runWorkVerdict implements `loam work verdict [repo] [work-branch]
// --outcome <approve|disapprove|neutral>`. --outcome is required.
func runWorkVerdict(ctx context.Context, deps *Deps, args []string) error {
	fs, outcome := newWorkVerdictFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work verdict", positional); err != nil {
		return err
	}
	switch *outcome {
	case "approve", "disapprove", "neutral":
	default:
		return newUsageError("work verdict requires --outcome=approve|disapprove|neutral")
	}
	return errNotImplemented
}
