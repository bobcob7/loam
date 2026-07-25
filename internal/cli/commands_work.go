package cli

import "context"

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

// runWorkStart implements `loam work start <repo> [from]`.
func runWorkStart(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work start")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) < 1 {
		return newUsageError("work start requires a repo argument")
	}
	if len(positional) > 2 {
		return newUsageError("work start takes at most a repo and a from branch")
	}
	return errNotImplemented
}

// runWorkSet implements `loam work set [repo] [work-branch] [--title
// <title>]`.
func runWorkSet(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work set")
	fs.String("title", "", "new title for the work branch")
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

// runWorkList implements `loam work list [--repo <repo>] [--author <id>]
// [--target <branch>] [--awaiting-review] [--state <state>] [--limit <n>]`.
// --limit is the NOTES spec correction on top of docs/cli-spec.md.
func runWorkList(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work list")
	fs.String("repo", "", "limit to one enrolled repo")
	fs.String("author", "", "limit to work branches authored by this agent identifier")
	fs.String("target", "", "limit to work branches targeting this branch")
	fs.Bool("awaiting-review", false, "limit to work branches awaiting the caller's verdict")
	fs.String("state", "reviewable", "draft, reviewable, reviewed, complete, or closed")
	fs.Int("limit", 0, "maximum number of work branches to return")
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

// runWorkComments implements `loam work comments [repo] [work-branch]
// [--staged]`.
func runWorkComments(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work comments")
	fs.Bool("staged", false, "return the caller's staged comments instead of published threads")
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

// runWorkComment implements `loam work comment [repo] [work-branch]
// [--file <path> --line <n>] [--resolve <thread-id>] [--edit <staged-id>]
// [--discard <staged-id>]`.
func runWorkComment(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work comment")
	fs.String("file", "", "anchor the new thread to this file")
	fs.Int("line", 0, "anchor the new thread to this line")
	fs.String("resolve", "", "mark this thread id resolved")
	fs.String("edit", "", "replace the body of this staged comment id")
	fs.String("discard", "", "remove this staged comment id")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work comment", positional); err != nil {
		return err
	}
	return errNotImplemented
}

// runWorkReply implements `loam work reply [repo] [work-branch] --thread
// <thread-id>`. --thread is required.
func runWorkReply(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work reply")
	thread := fs.String("thread", "", "the thread to reply to")
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

// runWorkVerdict implements `loam work verdict [repo] [work-branch]
// --outcome <approve|disapprove|neutral>`. --outcome is required.
func runWorkVerdict(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("work verdict")
	outcome := fs.String("outcome", "", "approve, disapprove, or neutral")
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
