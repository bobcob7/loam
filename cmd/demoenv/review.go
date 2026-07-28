package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// This file holds demo:m4's assertions -- the review round-trip's own
// shapes, which are NOT the {ingested, truncated, results} envelope
// check-envelope decodes. They live in Go for exactly the reason check.go's
// header gives: every one of them is a claim about a named field of a
// specific JSON document, and the shell equivalents pass for the wrong
// reasons. Three of demo:m4's claims are unexpressible as a grep at all:
//
//   - "staged comments are NOT visible in the unstaged listing" is a claim
//     about an EMPTY array. `grep -q body` returning nothing is equally
//     consistent with the CLI having crashed, printed usage, or emitted
//     `null` -- all of which would read as the demo's headline property
//     holding when it did not. check-comments decodes the document and
//     asserts the count is exactly zero.
//   - "the round incremented" is a claim about a NUMBER inside a nested
//     array, in order. `grep '"round": 2'` matches a 2 anywhere in the
//     payload, including a line number, a thread that was already round 2,
//     or a different thread entirely.
//   - "the prior round's verdict is stale" is a claim about one reviewer's
//     row. `grep 'stale.*true'` cannot say WHOSE.
//
// Exit status is the contract -- non-zero on any failed assertion -- so the
// Taskfile's errexit does the rest.

// unsetCount is the -count sentinel meaning "do not assert a count".
// Zero is a MEANINGFUL value here (the unstaged listing must have exactly
// zero threads), so it cannot double as the unset default the way
// check-envelope's -min-results 0 does.
const unsetCount = -1

// threadDoc is one published thread as `loam work comments` emits it
// (internal/cli's threadOutput). File/Line are omitted from the JSON when
// absent, so a top-level thread decodes with an empty File -- which is why
// -select-file always names a real path and never "".
type threadDoc struct {
	ID       string       `json:"id"`
	Resolved bool         `json:"resolved"`
	File     string       `json:"file"`
	Line     uint32       `json:"line"`
	Round    uint32       `json:"round"`
	Comments []commentDoc `json:"comments"`
}

// commentDoc is one comment within a published thread (internal/cli's
// commentOutput). Round is the comment's OWN round, which for a reply may
// be later than its thread's -- the increment demo:m4 exists to show.
type commentDoc struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	Round  uint32 `json:"round"`
}

// stagedDoc is one locally staged item as `loam work comment` and `loam
// work comments --staged` emit it (internal/cli's stagedCommentOutput). It
// carries NO round: a staged item has not been published, so no round has
// been assigned to it yet -- which is itself part of what "not visible
// until submitted" means.
type stagedDoc struct {
	Staged  bool   `json:"staged"`
	ID      string `json:"id"`
	File    string `json:"file"`
	Line    uint32 `json:"line"`
	Body    string `json:"body"`
	Resolve string `json:"resolve"`
}

// commentsFlags are check-comments' parsed flags, shared by its two modes.
type commentsFlags struct {
	label          *string
	staged         *bool
	count          *int
	wantFiles      *string
	wantBodies     *string
	selectFile     *string
	threadRound    *int
	commentRounds  *string
	commentAuthors *string
}

// runCheckComments asserts over one `loam work comments` document read from
// stdin, in either of that command's two modes.
//
// The two modes decode DIFFERENT shapes from the same command, which is the
// whole point: without --staged the CLI returns what the server published
// (internal/handler/workbranch/review.go -> ListComments: "Staged comments
// are never returned here and cannot be"), and with --staged it returns the
// caller's local staging area and never asks the server at all. Asserting
// both with one subcommand keeps the demo's headline property -- the same
// command, the same work branch, the same moment, two different answers --
// expressed as one tool's two invocations rather than two unrelated checks.
func runCheckComments(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("check-comments", flag.ContinueOnError)
	f := &commentsFlags{
		label:          fs.String("label", "comments", "what is being checked, echoed into pass/fail messages"),
		staged:         fs.Bool("staged", false, "decode the `--staged` shape (local staging area) instead of published threads"),
		count:          fs.Int("count", unsetCount, "assert exactly this many entries (0 is meaningful: nothing published)"),
		wantFiles:      fs.String("want-files", "", "comma-separated: assert some entry's file equals each"),
		wantBodies:     fs.String("want-bodies", "", "comma-separated (so a body containing a comma is not assertable here): assert some comment/staged item's body equals each"),
		selectFile:     fs.String("select-file", "", "published mode: address the one thread anchored to this file"),
		threadRound:    fs.Int("thread-round", unsetCount, "published mode: assert the selected thread's own round"),
		commentRounds:  fs.String("comment-rounds", "", "published mode: assert the selected thread's comment rounds, in order (e.g. 1,1,2)"),
		commentAuthors: fs.String("comment-authors", "", "published mode: assert the selected thread's comment authors, in order"),
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	if *f.staged {
		return checkStaged(payload, f, stdout)
	}
	return checkPublished(payload, f, stdout)
}

// checkPublished asserts over the published-thread shape.
func checkPublished(payload []byte, f *commentsFlags, stdout io.Writer) error {
	var threads []threadDoc
	if err := json.Unmarshal(payload, &threads); err != nil {
		return fmt.Errorf("decoding %s: %w (payload: %s)", *f.label, err, truncate(string(payload)))
	}
	var failures []string
	failures = appendCountFailure(failures, "published thread", len(threads), *f.count)
	for _, want := range split(*f.wantFiles) {
		if !anyThreadFile(threads, want) {
			failures = append(failures, fmt.Sprintf("no thread is anchored to file %q; got %v", want, threadFiles(threads)))
		}
	}
	for _, want := range split(*f.wantBodies) {
		if !anyCommentBody(threads, want) {
			failures = append(failures, fmt.Sprintf("no published comment has body %q", want))
		}
	}
	failures = append(failures, selectedThreadFailures(threads, f)...)
	return report(stdout, *f.label, failures)
}

// selectedThreadFailures applies the per-thread assertions (-thread-round,
// -comment-rounds, -comment-authors) to the single thread -select-file
// names. Selection is by ANCHOR, never by index: `SubmitVerdict` publishes a
// whole staged batch in one transaction, so every thread in it shares one
// `now()` and ListThreadsForWorkBranch's `ORDER BY t.created_at ASC, t.id`
// falls through to the random uuid tie-break -- an index-addressed assertion
// would pass or fail depending on which uuid sorted first.
func selectedThreadFailures(threads []threadDoc, f *commentsFlags) []string {
	wantsSelection := *f.threadRound != unsetCount || *f.commentRounds != "" || *f.commentAuthors != ""
	if *f.selectFile == "" {
		if wantsSelection {
			return []string{"-thread-round/-comment-rounds/-comment-authors address one thread; pass -select-file to name it"}
		}
		return nil
	}
	matches := make([]threadDoc, 0, 1)
	for _, thread := range threads {
		if thread.File == *f.selectFile {
			matches = append(matches, thread)
		}
	}
	if len(matches) != 1 {
		return []string{fmt.Sprintf("want exactly one thread anchored to %q, found %d; anchors present: %v", *f.selectFile, len(matches), threadFiles(threads))}
	}
	selected := matches[0]
	var failures []string
	if *f.threadRound != unsetCount && int(selected.Round) != *f.threadRound {
		failures = append(failures, fmt.Sprintf("thread on %s was raised in round %d, want round %d", selected.File, selected.Round, *f.threadRound))
	}
	if *f.commentRounds != "" {
		got := make([]string, 0, len(selected.Comments))
		for _, comment := range selected.Comments {
			got = append(got, strconv.FormatUint(uint64(comment.Round), 10))
		}
		if want := split(*f.commentRounds); !equalStrings(got, want) {
			failures = append(failures, fmt.Sprintf("thread on %s has comment rounds %v, want %v (in order)", selected.File, got, want))
		}
	}
	if *f.commentAuthors != "" {
		got := make([]string, 0, len(selected.Comments))
		for _, comment := range selected.Comments {
			got = append(got, comment.Author)
		}
		if want := split(*f.commentAuthors); !equalStrings(got, want) {
			failures = append(failures, fmt.Sprintf("thread on %s has comment authors %v, want %v (in order)", selected.File, got, want))
		}
	}
	return failures
}

// checkStaged asserts over the `--staged` shape.
func checkStaged(payload []byte, f *commentsFlags, stdout io.Writer) error {
	var items []stagedDoc
	if err := json.Unmarshal(payload, &items); err != nil {
		return fmt.Errorf("decoding %s: %w (payload: %s)", *f.label, err, truncate(string(payload)))
	}
	var failures []string
	failures = appendCountFailure(failures, "staged item", len(items), *f.count)
	files := make([]string, 0, len(items))
	for i, item := range items {
		files = append(files, item.File)
		// Every row `comments --staged` returns IS still staged; a false
		// here would mean the CLI listed something it had already published.
		if !item.Staged {
			failures = append(failures, fmt.Sprintf("staged item %d (%s) reports staged=false", i, item.ID))
		}
		if item.ID == "" {
			failures = append(failures, fmt.Sprintf("staged item %d has no id, so it could not be edited or discarded", i))
		}
	}
	for _, want := range split(*f.wantFiles) {
		if !containsString(files, want) {
			failures = append(failures, fmt.Sprintf("no staged item is anchored to file %q; got %v", want, files))
		}
	}
	for _, want := range split(*f.wantBodies) {
		found := false
		for _, item := range items {
			if item.Body == want {
				found = true
				break
			}
		}
		if !found {
			failures = append(failures, fmt.Sprintf("no staged item has body %q", want))
		}
	}
	return report(stdout, *f.label, failures)
}

// verdictDoc is one row of `loam work verdicts` (internal/cli's
// workVerdictOutput). Stale is DERIVED server-side from the verdict's round
// against the branch's current one, never stored -- see
// internal/handler/workbranch/review.go -> ListVerdicts.
type verdictDoc struct {
	Reviewer string `json:"reviewer"`
	Outcome  string `json:"outcome"`
	Round    uint32 `json:"round"`
	Stale    bool   `json:"stale"`
}

// runCheckVerdicts asserts over one `loam work verdicts` document read from
// stdin.
//
// -stale is a string, not a bool flag, precisely because BOTH values are
// load-bearing in this demo and the difference between them is the whole
// point: the same reviewer's same verdict must read stale=false while its
// round is current and stale=true once a later round opens. A bool flag's
// unset value is indistinguishable from an explicit false, so asserting
// "stale is false" would silently become "no assertion" on a typo.
func runCheckVerdicts(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("check-verdicts", flag.ContinueOnError)
	label := fs.String("label", "verdicts", "what is being checked, echoed into pass/fail messages")
	count := fs.Int("count", unsetCount, "assert exactly this many verdict rows")
	reviewer := fs.String("reviewer", "", "address the row for this reviewer (required by -outcome/-round/-stale)")
	outcome := fs.String("outcome", "", "assert the addressed row's outcome (approve, disapprove, neutral)")
	round := fs.Int("round", unsetCount, "assert the addressed row's round")
	stale := fs.String("stale", "", "assert the addressed row's stale flag: \"true\" or \"false\"")
	if err := fs.Parse(args); err != nil {
		return err
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	var verdicts []verdictDoc
	if err := json.Unmarshal(payload, &verdicts); err != nil {
		return fmt.Errorf("decoding %s: %w (payload: %s)", *label, err, truncate(string(payload)))
	}
	var failures []string
	failures = appendCountFailure(failures, "verdict", len(verdicts), *count)
	wantsRow := *outcome != "" || *round != unsetCount || *stale != ""
	switch {
	case *reviewer == "" && wantsRow:
		failures = append(failures, "-outcome/-round/-stale address one reviewer's row; pass -reviewer to name it")
	case *reviewer != "":
		failures = append(failures, verdictRowFailures(verdicts, *reviewer, *outcome, *round, *stale)...)
	}
	return report(stdout, *label, failures)
}

// verdictRowFailures applies the per-reviewer assertions to the single row
// for reviewer. Addressing by reviewer rather than by index matters because
// ListVerdicts returns one row PER REVIEWER (their latest verdict), so an
// index would silently address a different agent as soon as a second one
// votes.
func verdictRowFailures(verdicts []verdictDoc, reviewer, wantOutcome string, wantRound int, wantStale string) []string {
	var row *verdictDoc
	for i := range verdicts {
		if verdicts[i].Reviewer == reviewer {
			row = &verdicts[i]
			break
		}
	}
	if row == nil {
		reviewers := make([]string, 0, len(verdicts))
		for _, v := range verdicts {
			reviewers = append(reviewers, v.Reviewer)
		}
		return []string{fmt.Sprintf("no verdict from reviewer %q; got %v", reviewer, reviewers)}
	}
	var failures []string
	if wantOutcome != "" && row.Outcome != wantOutcome {
		failures = append(failures, fmt.Sprintf("%s's verdict outcome is %q, want %q", reviewer, row.Outcome, wantOutcome))
	}
	if wantRound != unsetCount && int(row.Round) != wantRound {
		failures = append(failures, fmt.Sprintf("%s's verdict is recorded in round %d, want round %d", reviewer, row.Round, wantRound))
	}
	switch wantStale {
	case "":
	case "true", "false":
		if strconv.FormatBool(row.Stale) != wantStale {
			failures = append(failures, fmt.Sprintf("%s's verdict has stale=%t, want stale=%s", reviewer, row.Stale, wantStale))
		}
	default:
		failures = append(failures, fmt.Sprintf("-stale %q is neither \"true\" nor \"false\"", wantStale))
	}
	return failures
}

// workListDoc is `loam work list`'s envelope (internal/cli's
// workListOutput). Truncated is decoded but not asserted on: the demo lists
// a single seeded repo, so it is always false, and pinning it would assert
// the fixture rather than the behaviour.
type workListDoc struct {
	Truncated bool `json:"truncated"`
	Results   []struct {
		Repo   string `json:"repo"`
		Name   string `json:"name"`
		Target string `json:"target"`
		Title  string `json:"title"`
		Author string `json:"author"`
		State  string `json:"state"`
	} `json:"results"`
}

// runCheckWorkList asserts over one `loam work list` document read from
// stdin. -deny-names is as load-bearing as -want-names here: the demo
// asserts the SAME work branch is absent from the reviewer's
// --awaiting-review queue while it is reviewed, and present again once a new
// round opens, and only the absence half proves the filter is doing
// anything at all.
func runCheckWorkList(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("check-worklist", flag.ContinueOnError)
	label := fs.String("label", "work list", "what is being checked, echoed into pass/fail messages")
	count := fs.Int("count", unsetCount, "assert exactly this many rows (0 is meaningful: nothing queued)")
	wantNames := fs.String("want-names", "", "comma-separated: assert some row's name equals each")
	denyNames := fs.String("deny-names", "", "comma-separated: assert NO row's name equals any")
	wantState := fs.String("want-state", "", "assert every row's state equals this")
	wantRepo := fs.String("want-repo", "", "assert every row's repo equals this")
	if err := fs.Parse(args); err != nil {
		return err
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	var doc workListDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("decoding %s: %w (payload: %s)", *label, err, truncate(string(payload)))
	}
	names := make([]string, 0, len(doc.Results))
	for _, row := range doc.Results {
		names = append(names, row.Name)
	}
	var failures []string
	failures = appendCountFailure(failures, "work branch", len(doc.Results), *count)
	for _, want := range split(*wantNames) {
		if !containsString(names, want) {
			failures = append(failures, fmt.Sprintf("work branch %q is not listed; got %v", want, names))
		}
	}
	for _, deny := range split(*denyNames) {
		if containsString(names, deny) {
			failures = append(failures, fmt.Sprintf("work branch %q is listed, but must not be", deny))
		}
	}
	for i, row := range doc.Results {
		if *wantState != "" && row.State != *wantState {
			failures = append(failures, fmt.Sprintf("row %d (%s) has state %q, want %q", i, row.Name, row.State, *wantState))
		}
		if *wantRepo != "" && row.Repo != *wantRepo {
			failures = append(failures, fmt.Sprintf("row %d (%s) has repo %q, want %q", i, row.Name, row.Repo, *wantRepo))
		}
	}
	return report(stdout, *label, failures)
}

// runThreadID prints the id of the one thread anchored to -file, and
// nothing else, so the Taskfile can capture it with plain command
// substitution and pass it to `loam work reply --thread`.
//
// This is extraction, not assertion, and it is deliberately a separate
// subcommand from check-comments rather than a flag on it: check-comments
// writes PASS/FAIL lines to stdout, and a subcommand that sometimes emitted
// a bare id there instead would make its own output un-greppable by whoever
// reads the demo transcript. It still FAILS (exit 1) rather than printing an
// empty string when the anchor does not resolve to exactly one thread --
// an empty --thread argument would otherwise reach the server as a usage
// error several steps later, naming the wrong culprit.
func runThreadID(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("thread-id", flag.ContinueOnError)
	file := fs.String("file", "", "the file anchor of the thread whose id to print")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("thread-id requires -file")
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	var threads []threadDoc
	if err := json.Unmarshal(payload, &threads); err != nil {
		return fmt.Errorf("decoding threads: %w (payload: %s)", err, truncate(string(payload)))
	}
	var ids []string
	for _, thread := range threads {
		if thread.File == *file {
			ids = append(ids, thread.ID)
		}
	}
	if len(ids) != 1 {
		return fmt.Errorf("want exactly one thread anchored to %q, found %d; anchors present: %v", *file, len(ids), threadFiles(threads))
	}
	fmt.Fprintln(stdout, ids[0])
	return nil
}

// runField prints one named top-level string field of a JSON object read
// from stdin, and nothing else -- demo:m4 uses it to learn the randomly
// generated work-branch name `loam work start` just created.
//
// This is the same extraction-not-assertion role thread-id plays, and it
// exists for the same reason: the alternative is a sed expression like
// `s/.*"name":"\([^"]*\)".*/\1/p` over the CLI's compact JSON, whose `.*`
// is greedy and so silently addresses the LAST "name" in the document
// rather than the only one the demo means. An absent or non-string field is
// an error, never an empty string: an empty work-branch name would flow
// several steps downstream and fail there, naming the wrong culprit.
func runField(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("field", flag.ContinueOnError)
	name := fs.String("name", "", "the top-level field to print")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("field requires -name")
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("decoding document: %w (payload: %s)", err, truncate(string(payload)))
	}
	value, ok := doc[*name].(string)
	if !ok {
		return fmt.Errorf("document has no string field %q (payload: %s)", *name, truncate(string(payload)))
	}
	if value == "" {
		return fmt.Errorf("field %q is empty (payload: %s)", *name, truncate(string(payload)))
	}
	fmt.Fprintln(stdout, value)
	return nil
}

// appendCountFailure records an exact-count mismatch, unless want is the
// unsetCount sentinel.
func appendCountFailure(failures []string, noun string, got, want int) []string {
	if want == unsetCount || got == want {
		return failures
	}
	return append(failures, fmt.Sprintf("got %d %s(s), want exactly %d", got, noun, want))
}

// threadFiles gathers every thread's anchor, for a failure message that
// shows what was actually returned rather than only what was missing.
func threadFiles(threads []threadDoc) []string {
	files := make([]string, 0, len(threads))
	for _, thread := range threads {
		files = append(files, thread.File)
	}
	return files
}

// anyThreadFile reports whether any thread is anchored to want.
func anyThreadFile(threads []threadDoc, want string) bool {
	return containsString(threadFiles(threads), want)
}

// anyCommentBody reports whether any comment on any thread has body want.
func anyCommentBody(threads []threadDoc, want string) bool {
	for _, thread := range threads {
		for _, comment := range thread.Comments {
			if comment.Body == want {
				return true
			}
		}
	}
	return false
}

// containsString reports whether values contains want.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// equalStrings reports whether two ordered lists are element-wise equal.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != strings.TrimSpace(want[i]) {
			return false
		}
	}
	return true
}
