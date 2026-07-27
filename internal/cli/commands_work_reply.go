package cli

import (
	"context"
	"flag"
	"fmt"

	"connectrpc.com/connect"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// newWorkReplyFlags builds the flag.FlagSet for `loam work reply [repo]
// [work-branch] --thread <thread-id>`, plus the parsed --thread value.
func newWorkReplyFlags() (*flag.FlagSet, *string) {
	fs := newFlagSet("work reply")
	thread := fs.String("thread", "", "the thread to reply to")
	return fs, thread
}

// replyOutput is `work reply`'s success shape (docs/cli-spec.md -> reply):
// the posted reply, nothing more. There is no round field even though the
// server stamps one, and no thread id: the spec's output example is exactly
// { author, body }.
type replyOutput struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

// runWorkReply implements `loam work reply [repo] [work-branch] --thread
// <thread-id>` with the body read from stdin (docs/cli-spec.md -> reply).
//
// Unlike `work comment`, this command is IMMEDIATE: it never opens the
// staging area and never reads it. A reply is a single server-side write, so
// there is nothing local to batch and nothing to publish later.
//
// It also makes no pre-flight existence or state check. ReplyToThread itself
// resolves the work branch (NotFound), rejects the terminal complete/closed
// states (FailedPrecondition — docs/cli-spec.md -> State gates), and
// resolves the thread (NotFound), all in one round trip; a second RPC here
// would only duplicate those checks and could still disagree with the one
// that actually decides. The %w wrap below carries the *connect.Error
// through to mapCommandError unchanged, so NotFound stays exit 3 and
// FailedPrecondition stays exit 2.
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
	repo, workBranch, err := resolveWorkBranchIdentity(deps.workspace, positional)
	if err != nil {
		return err
	}
	body, err := readStdin(deps.stdin)
	if err != nil {
		return err
	}
	if body == "" {
		return newUsageCLIError("work reply requires a reply body on stdin", nil)
	}
	req := &loamv1.ReplyToThreadRequest{Repo: repo, WorkBranch: workBranch, ThreadId: *thread, Body: body}
	resp, err := deps.connect.WorkBranch().ReplyToThread(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("replying to thread %s on work branch %s/%s: %w", *thread, repo, workBranch, err)
	}
	comment := resp.Msg.GetComment()
	return deps.encoder.Encode(replyOutput{Author: comment.GetAuthor(), Body: comment.GetBody()})
}
