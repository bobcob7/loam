package cli

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/pflag"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// commentFlags holds the parsed values of `work comment`'s five flags. They
// travel as one struct rather than five return values because their
// legality is a relationship between them (see resolveCommentMode), not a
// property of any one of them.
type commentFlags struct {
	file    *string
	line    *int
	resolve *string
	edit    *string
	discard *string
	list    *bool
}

// newWorkCommentFlags builds the pflag.FlagSet for `loam work comment
// [repo] [work-branch] [--file <path> --line <n>] [--resolve <thread-id>]
// [--edit <staged-id>] [--discard <staged-id>] [--list]`, plus the parsed
// values.
func newWorkCommentFlags() (*pflag.FlagSet, *commentFlags) {
	fs := newFlagSet("work comment")
	f := &commentFlags{
		file:    fs.String("file", "", "anchor the new thread to this file"),
		line:    fs.Int("line", 0, "anchor the new thread to this line"),
		resolve: fs.String("resolve", "", "mark this thread id resolved"),
		edit:    fs.String("edit", "", "replace the body of this staged comment id"),
		discard: fs.String("discard", "", "remove this staged comment id"),
		list:    fs.Bool("list", false, "report the staged comments and the staging directory holding them"),
	}
	return fs, f
}

// commentMode names which of the three mutually exclusive things a single
// `work comment` invocation does (docs/cli-spec.md -> comment (add): "a
// single invocation either opens a new thread …, --edits a staged comment,
// or --discards one").
type commentMode int

const (
	commentModeNew commentMode = iota
	commentModeEdit
	commentModeDiscard
	commentModeList
)

// stagedCommentOutput is `work comment`'s success shape (docs/cli-spec.md ->
// comment (add)): the staged item plus its local id. Staged is false only
// for a --discard, where the item reported is the one that is no longer
// staged. File/Line/Body/Resolve are omitted when absent so a top-level
// comment does not report a phantom `"file": ""` anchor and a resolve-only
// item does not report an empty body.
//
// There is no round field, by design: the round is assigned when `work
// verdict` publishes, not when an item is staged (see stagedItem).
type stagedCommentOutput struct {
	Staged  bool   `json:"staged"`
	ID      string `json:"id"`
	File    string `json:"file,omitempty"`
	Line    uint32 `json:"line,omitempty"`
	Body    string `json:"body,omitempty"`
	Resolve string `json:"resolve,omitempty"`
}

// runWorkComment implements `loam work comment [repo] [work-branch]
// [--file <path> --line <n>] [--resolve <thread-id>] [--edit <staged-id>]
// [--discard <staged-id>]` with the body read from stdin (docs/cli-spec.md
// -> comment (add)).
//
// The order below is deliberate: everything that can fail as usage (exit 2)
// is decided from the arguments alone, before any RPC, so a malformed
// invocation never reaches the network. Only then is the work branch
// checked to exist (exit 3), and only then is the staging area opened —
// a rejected invocation must not leave a staging directory behind for a
// work branch it never validated.
func runWorkComment(ctx context.Context, deps *Deps, args []string) error {
	fs, f := newWorkCommentFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if err := requireWorkBranchArgs("work comment", positional); err != nil {
		return err
	}
	repo, workBranch, err := resolveWorkBranchIdentity(deps.workspace, positional)
	if err != nil {
		return err
	}
	body := ""
	if !commentModeSkipsBody(f) {
		body, err = readStdin(deps.stdin)
		if err != nil {
			return err
		}
	}
	mode, err := resolveCommentMode(f, body)
	if err != nil {
		return err
	}
	if err := checkCommentTargets(ctx, deps, repo, workBranch, *f.resolve); err != nil {
		return err
	}
	return stageComment(deps, repo, workBranch, mode, f, body)
}

// commentModeSkipsBody decides, from flags alone, whether an invocation can
// be resolved and legally completed without ever reading stdin (loam-hi5o.6:
// reading stdin unconditionally before knowing the mode is what makes a
// lone --discard or --resolve hang forever on an interactive or
// un-redirected terminal, since EOF never comes).
//
// It is deliberately narrow: it returns true only where forcing body="" into
// resolveCommentMode below lands on the SAME legal outcome a real body
// would have, which holds for exactly two shapes --
//
//   - --discard, alone or in the --edit-and-discard conflict: every check
//     resolveCommentMode makes for this shape other than the body-presence
//     rejection at its "case *f.discard != \"\"" branch is flag-only, and
//     that one body check is the behaviour this bead deliberately narrows
//     away (see the bead's NOTES) -- a body piped alongside a lone --discard
//     is no longer read, so it can no longer be detected and rejected; it is
//     silently ignored.
//   - --resolve alone, with no --file/--line/--edit/--discard: resolved with
//     body="" this is always the legal "resolve-only" outcome, so a body
//     piped alongside it is likewise silently ignored now rather than
//     attached to the resolve.
//
// Every other shape's legality or content depends on the actual body (a new
// top-level or --file/--line-anchored comment needs the body text itself;
// --edit needs it as the replacement text and to know whether one was
// given at all), so those must still read stdin -- this function must not
// grow to cover them.
func commentModeSkipsBody(f *commentFlags) bool {
	switch {
	case *f.list:
		return true
	case *f.discard != "":
		return true
	case *f.edit != "":
		return false
	case *f.resolve != "" && *f.file == "" && *f.line == 0:
		return true
	default:
		return false
	}
}

// resolveCommentMode decides which mode an invocation selected and rejects
// every conflicting or under-specified combination as a usage error (exit
// 2, docs/cli-spec.md -> comment (add) -> Errors: "conflicting modes, a
// missing body when one is required"). A body on stdin means "open a new
// thread", which is why it conflicts with --discard: --discard takes no
// body, and silently ignoring one would drop text the agent believed it had
// staged.
func resolveCommentMode(f *commentFlags, body string) (commentMode, error) {
	switch {
	case *f.list:
		if *f.edit != "" || *f.discard != "" || *f.file != "" || *f.line != 0 || *f.resolve != "" {
			return commentModeNew, newUsageCLIError("work comment: --list only reports the staging area; it cannot be combined with --file, --line, --resolve, --edit, or --discard", nil)
		}
		return commentModeList, nil
	case *f.edit != "" && *f.discard != "":
		return commentModeNew, newUsageCLIError("work comment: --edit and --discard are mutually exclusive", nil)
	case *f.edit != "":
		if err := requireNoNewThreadFlags(f, "--edit"); err != nil {
			return commentModeNew, err
		}
		if body == "" {
			return commentModeNew, newUsageCLIError("work comment: --edit requires the replacement body on stdin", nil)
		}
		return commentModeEdit, nil
	case *f.discard != "":
		if err := requireNoNewThreadFlags(f, "--discard"); err != nil {
			return commentModeNew, err
		}
		if body != "" {
			return commentModeNew, newUsageCLIError("work comment: --discard takes no body; a body on stdin opens a new thread instead", nil)
		}
		return commentModeDiscard, nil
	}
	if err := validateNewComment(f, body); err != nil {
		return commentModeNew, err
	}
	return commentModeNew, nil
}

// requireNoNewThreadFlags rejects the new-thread flags (--file, --line,
// --resolve) when mode is --edit or --discard: those two act on an
// already-staged item, so an anchor or a resolve target alongside them
// describes a different mode than the one selected.
func requireNoNewThreadFlags(f *commentFlags, mode string) error {
	if *f.file != "" || *f.line != 0 || *f.resolve != "" {
		return newUsageCLIError(fmt.Sprintf("work comment: %s cannot be combined with --file, --line, or --resolve", mode), nil)
	}
	return nil
}

// validateNewComment checks the new-thread mode's own requirements: a body
// is required unless the invocation is a bare --resolve (docs/cli-spec.md:
// "Required unless only --resolve or --discard is given"), --line is
// meaningless without the --file it indexes into, and an anchor is
// meaningless with no comment to anchor.
func validateNewComment(f *commentFlags, body string) error {
	if *f.line < 0 {
		return newUsageCLIError(fmt.Sprintf("work comment: --line %d is not a line number", *f.line), nil)
	}
	if *f.line != 0 && *f.file == "" {
		return newUsageCLIError("work comment: --line anchors a comment within a file; pass --file too", nil)
	}
	if body == "" && *f.resolve == "" {
		return newUsageCLIError("work comment requires a comment body on stdin unless only --resolve or --discard is given", nil)
	}
	if body == "" && *f.file != "" {
		return newUsageCLIError("work comment: --file/--line anchor a new comment; a resolve-only invocation takes no anchor", nil)
	}
	return nil
}

// checkCommentTargets validates the invocation against the server: the work
// branch must exist, and a --resolve target must be an existing thread the
// caller itself opened. `comment` is state-ungated (docs/cli-spec.md ->
// State gates: "the CLI checks that the work branch exists, nothing more"),
// so this is an existence check, never a state gate.
func checkCommentTargets(ctx context.Context, deps *Deps, repo, workBranch, resolveThread string) error {
	client := deps.connect.WorkBranch()
	if _, err := client.GetWorkBranch(ctx, connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: repo, WorkBranch: workBranch})); err != nil {
		return fmt.Errorf("checking work branch %s/%s exists: %w", repo, workBranch, err)
	}
	if resolveThread == "" {
		return nil
	}
	thread, found, err := findThread(ctx, client, repo, workBranch, resolveThread)
	if err != nil {
		return err
	}
	if !found {
		return newNotFoundError(fmt.Sprintf("thread %s does not exist on work branch %s/%s", resolveThread, repo, workBranch), nil)
	}
	return requireThreadAuthor(thread, deps.config.Identifier())
}

// findThread scans the work branch's published threads for threadID,
// following pagination rather than trusting the first response: a server
// default page limit would otherwise make a real thread on a busy work
// branch look missing, reporting exit 3 for an id that exists. found is
// false only once every page has been read.
func findThread(ctx context.Context, client WorkBranchClient, repo, workBranch, threadID string) (thread *loamv1.Thread, found bool, err error) {
	for offset := uint32(0); ; {
		req := &loamv1.ListCommentsRequest{Repo: repo, WorkBranch: workBranch, Page: &loamv1.Page{Offset: offset}}
		resp, err := client.ListComments(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, false, fmt.Errorf("listing comment threads on %s/%s: %w", repo, workBranch, err)
		}
		threads := resp.Msg.GetThreads()
		for _, candidate := range threads {
			if candidate.GetId() == threadID {
				return candidate, true, nil
			}
		}
		offset += uint32(len(threads))
		if len(threads) == 0 || offset >= resp.Msg.GetPageInfo().GetTotal() {
			return nil, false, nil
		}
	}
}

// requireThreadAuthor enforces "only the thread's original author may
// resolve it" (docs/cli-spec.md -> comment (add)). The original author is
// the author of the thread's FIRST comment — the one that opened it — not
// of its most recent reply, so replying to someone else's thread never
// confers the right to resolve it. A thread carrying no comments at all has
// no identifiable author and is refused rather than allowed: the check
// fails closed.
func requireThreadAuthor(thread *loamv1.Thread, caller string) error {
	comments := thread.GetComments()
	if len(comments) == 0 {
		return newUnauthorizedError(fmt.Sprintf("thread %s has no identifiable author; only a thread's author may resolve it", thread.GetId()), nil)
	}
	if author := comments[0].GetAuthor(); author != caller {
		return newUnauthorizedError(fmt.Sprintf("thread %s was opened by %s, not %s; only a thread's author may resolve it", thread.GetId(), author, caller), nil)
	}
	return nil
}

// stageComment applies mode to the caller's local staging area and encodes
// the resulting item. Nothing here touches the server: staged items stay
// invisible to everyone else until `work verdict` publishes them
// (docs/cli-spec.md -> comment (add)).
func stageComment(deps *Deps, repo, workBranch string, mode commentMode, f *commentFlags, body string) error {
	store, err := openStagingStore(deps.workspace, repo, workBranch)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if mode == commentModeList {
		return encodeStagedListing(deps, store)
	}
	item, staged, err := applyCommentMode(store, mode, f, body)
	if err != nil {
		return err
	}
	return deps.encoder.Encode(stagedCommentOutput{
		Staged:  staged,
		ID:      item.ID,
		File:    item.File,
		Line:    item.Line,
		Body:    item.Body,
		Resolve: item.Resolve,
	})
}

// stagedListingOutput is `work comment --list`'s success shape: the staged
// items, how many there are, and — the part that is not merely convenient —
// the directory they were read from.
//
// StagingDir is the field this command exists for (loam-rgyg). `comments
// --staged` already returns a bare array, which answers "what is staged"
// but not "staged WHERE", and those were the two questions a reviewer
// could not tell apart when a verdict published none of their ten
// comments: an empty array from the wrong staging area is byte-identical
// to an empty array from the right one. Count is redundant with
// len(items), deliberately: it is the number a reviewer compares against
// `published` in the verdict response, and making them read it off a list
// they have to count themselves is how a mismatch stays unnoticed.
type stagedListingOutput struct {
	StagingDir string                `json:"staging_dir"`
	Count      int                   `json:"count"`
	Items      []stagedCommentOutput `json:"items"`
}

// encodeStagedListing reports the caller's staged items and where they
// live — the inspectable step before the irreversible one.
func encodeStagedListing(deps *Deps, store *stagingStore) error {
	items, err := store.list()
	if err != nil {
		return err
	}
	rows := stagedCommentOutputsFrom(items)
	return deps.encoder.Encode(stagedListingOutput{StagingDir: store.location(), Count: len(rows), Items: rows})
}

// applyCommentMode performs the selected mode's single staging mutation.
// staged reports whether the item is staged AFTER the call, so a --discard
// reports the item it removed with staged=false.
func applyCommentMode(store *stagingStore, mode commentMode, f *commentFlags, body string) (item stagedItem, staged bool, err error) {
	switch mode {
	case commentModeEdit:
		item, err = store.edit(*f.edit, body)
		return item, true, err
	case commentModeDiscard:
		item, err = store.discard(*f.discard)
		return item, false, err
	default:
		item, err = store.add(stagedItem{File: *f.file, Line: uint32(*f.line), Body: body, Resolve: *f.resolve})
		return item, true, err
	}
}
