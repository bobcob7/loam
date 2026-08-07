package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// This file holds demo:m5's assertions: the admin ProposalService's queue
// shape, and the forge's own pull-request list read back through the
// Forgejo REST surface.
//
// They are in Go for check.go's reason, and two of demo:m5's claims are
// not expressible as a grep at all:
//
//   - "no SECOND pull request was created" is a claim about the LENGTH of
//     the forge's whole pull-request list. A grep for the first PR's
//     number succeeds just as well when a second one exists beside it,
//     and a grep for the absence of "#2" would pass if the endpoint
//     returned an error page instead of a list.
//   - "the PR body attributes Loam and only Loam" is two claims at once:
//     the body must equal the work branch's description followed by the
//     exact footer, AND must not contain any agent identity. Only the
//     first is greppable, and only approximately -- a body containing the
//     footer somewhere in the middle would satisfy it.
//
// Exit status is the contract -- non-zero on any failed assertion.

// attributionFooter is docs/sync-spec.md -> Proposal Acceptance's PR-body
// footer, verbatim, reproduced here because internal/mirrorsync's own
// constant is unexported. Reproducing it is deliberate rather than
// unfortunate: this file is the demo's INDEPENDENT statement of what the
// body must be, so a change to the production constant has to be made
// twice and is caught here as a failing demo rather than accepted
// silently by an assertion that reads the value it is checking.
const attributionFooter = "---\nProposed via Loam."

// proposalsDoc is ListProposals' response as protojson renders it:
// lowerCamelCase field names, and zero-valued fields omitted entirely --
// so an unset upstreamPrUrl decodes as "" rather than erroring, which is
// itself the "no PR yet" state this demo asserts against before the first
// accept.
type proposalsDoc struct {
	Proposals []struct {
		WorkBranch struct {
			Repo          string `json:"repo"`
			Name          string `json:"name"`
			Target        string `json:"target"`
			Title         string `json:"title"`
			State         string `json:"state"`
			Author        string `json:"author"`
			UpstreamPRURL string `json:"upstreamPrUrl"`
		} `json:"workBranch"`
		Verdicts []verdictSummaryDoc `json:"verdicts"`
		// Acceptable is loam-u84g's Proposal.acceptable. protojson omits a
		// false bool entirely, so an unacceptable proposal arrives with no
		// key at all and decodes to false -- which is the correct reading
		// and is also the safe one: absent must never be mistaken for "yes,
		// accept this". The demo asserts it explicitly rather than inferring
		// it from state, because state is only ONE of the three things that
		// can block an accept.
		Acceptable bool `json:"acceptable"`
	} `json:"proposals"`
}

// verdictSummaryDoc is one loam.v1.VerdictSummary as ListProposals renders
// it. It is a named type rather than an inline struct so
// approveVerdictFailures can take a slice of it.
type verdictSummaryDoc struct {
	Reviewer string `json:"reviewer"`
	Outcome  string `json:"outcome"`
	Stale    bool   `json:"stale"`
	Round    uint32 `json:"round"`
}

// runCheckProposals asserts over one ListProposals response read from
// stdin.
//
// -deny-names is as load-bearing as -want-names here. demo:m5 asserts the
// SAME work branch is in the queue after its first accept, in it but
// UNACCEPTABLE once a conflicting target advance resets it to draft
// (loam-u84g -- it used to assert absence there, which is what let an
// approved branch disappear from the operator's only surface), acceptable
// again once it has been caught up and re-approved, and gone for good once
// its PR merges. Only the absence half proves the queue's predicate is
// evaluating anything.
//
// -acceptable is what keeps the middle step honest now that the branch stays
// listed. Asserting presence alone would pass against a server that listed it
// AND offered the accept button, which is a worse failure than the one this
// bead fixed: the admin would press a button AcceptProposal refuses.
func runCheckProposals(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("check-proposals", flag.ContinueOnError)
	label := fs.String("label", "proposals", "what is being checked, echoed into pass/fail messages")
	count := fs.Int("count", unsetCount, "assert exactly this many proposals (0 is meaningful: an empty queue)")
	wantNames := fs.String("want-names", "", "comma-separated: assert some proposal's work branch name equals each")
	denyNames := fs.String("deny-names", "", "comma-separated: assert NO proposal's work branch name equals any")
	selectName := fs.String("select-name", "", "address the one proposal for this work branch (required by -state/-pr-url/-approver/-round/-acceptable)")
	wantState := fs.String("state", "", "assert the addressed proposal's state, e.g. WORK_BRANCH_STATE_REVIEWED")
	wantPRURL := fs.String("pr-url", "", "assert the addressed proposal's upstream_pr_url")
	wantApprover := fs.String("approver", "", "assert the addressed proposal carries an approve verdict from this reviewer")
	wantRound := fs.Int("round", unsetCount, "assert the addressed proposal's approve verdict is in this round")
	wantAcceptable := fs.String("acceptable", "", "assert the addressed proposal's acceptable flag: \"true\" or \"false\"")
	if err := fs.Parse(args); err != nil {
		return err
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	var doc proposalsDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("decoding %s: %w (payload: %s)", *label, err, truncate(string(payload)))
	}
	names := make([]string, 0, len(doc.Proposals))
	for _, p := range doc.Proposals {
		names = append(names, p.WorkBranch.Name)
	}
	var failures []string
	failures = appendCountFailure(failures, "proposal", len(doc.Proposals), *count)
	for _, want := range split(*wantNames) {
		if !containsString(names, want) {
			failures = append(failures, fmt.Sprintf("work branch %q is not in the proposal queue; queued: %v", want, names))
		}
	}
	for _, deny := range split(*denyNames) {
		if containsString(names, deny) {
			failures = append(failures, fmt.Sprintf("work branch %q is in the proposal queue, but must not be", deny))
		}
	}
	wantsRow := *wantState != "" || *wantPRURL != "" || *wantApprover != "" || *wantRound != unsetCount || *wantAcceptable != ""
	switch {
	case *selectName == "" && wantsRow:
		failures = append(failures, "-state/-pr-url/-approver/-round/-acceptable address one proposal; pass -select-name to name it")
	case *selectName != "":
		failures = append(failures, proposalRowFailures(doc, *selectName, *wantState, *wantPRURL, *wantApprover, *wantRound, *wantAcceptable)...)
	}
	return report(stdout, *label, failures)
}

// proposalRowFailures applies the per-proposal assertions to the single
// row -select-name addresses. Addressing by work-branch name rather than
// by index matters because the queue spans every enrolled repo and is
// ordered by the store, so an index would silently address a different
// branch as soon as a second one qualifies.
// wantAcceptable is the same tri-state string runCheckVerdicts' -stale uses,
// and for the identical reason: a plain bool flag defaults to false, so
// "acceptable is false" would silently become "no assertion" on a typo --
// which is exactly the assertion this bead most needs not to lose.
func proposalRowFailures(doc proposalsDoc, name, wantState, wantPRURL, wantApprover string, wantRound int, wantAcceptable string) []string {
	for _, p := range doc.Proposals {
		if p.WorkBranch.Name != name {
			continue
		}
		var failures []string
		if wantState != "" && p.WorkBranch.State != wantState {
			failures = append(failures, fmt.Sprintf("proposal %s has state %q, want %q", name, p.WorkBranch.State, wantState))
		}
		if wantPRURL != "" && p.WorkBranch.UpstreamPRURL != wantPRURL {
			failures = append(failures, fmt.Sprintf("proposal %s carries upstream_pr_url %q, want %q", name, p.WorkBranch.UpstreamPRURL, wantPRURL))
		}
		switch wantAcceptable {
		case "":
		case "true", "false":
			if strconv.FormatBool(p.Acceptable) != wantAcceptable {
				failures = append(failures, fmt.Sprintf("proposal %s is acceptable=%t, want %s", name, p.Acceptable, wantAcceptable))
			}
		default:
			failures = append(failures, fmt.Sprintf("-acceptable takes \"true\" or \"false\", got %q", wantAcceptable))
		}
		if wantApprover != "" || wantRound != unsetCount {
			failures = append(failures, approveVerdictFailures(p.Verdicts, name, wantApprover, wantRound)...)
		}
		return failures
	}
	return []string{fmt.Sprintf("no proposal for work branch %q in the queue", name)}
}

// approveVerdictFailures asserts the addressed proposal carries an
// approve verdict from wantApprover, in wantRound. It looks for an
// APPROVE specifically: ListProposals returns the round's verdicts
// whatever they are, and "the reviewer voted" is not the precondition
// AcceptProposal enforces.
func approveVerdictFailures(verdicts []verdictSummaryDoc, name, wantApprover string, wantRound int) []string {
	for _, v := range verdicts {
		if wantApprover != "" && v.Reviewer != wantApprover {
			continue
		}
		if v.Outcome != "VERDICT_OUTCOME_APPROVE" {
			continue
		}
		if wantRound != unsetCount && int(v.Round) != wantRound {
			continue
		}
		if v.Stale {
			return []string{fmt.Sprintf("proposal %s's approve verdict from %s is marked stale, so it cannot be the current round's", name, v.Reviewer)}
		}
		return nil
	}
	got := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		got = append(got, fmt.Sprintf("%s/%s/round %d/stale %t", v.Reviewer, v.Outcome, v.Round, v.Stale))
	}
	return []string{fmt.Sprintf("proposal %s has no approve verdict from %q in round %d; got %v", name, wantApprover, wantRound, got)}
}

// pullRequestDoc is one row of Forgejo's list-pulls response, decoded with
// the same field names forge.Forgejo's own forgejoPullRequest uses plus
// the title/body the demo asserts attribution on.
type pullRequestDoc struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// runCheckPRs asserts over the forge's own pull-request list, read from
// the Forgejo REST surface with state=all -- every PR ever opened on the
// repo, regardless of state.
//
// state=all is what makes -count 1 mean what demo:m5 needs it to mean.
// The default state=open list empties itself the moment the PR merges, so
// a count taken against it would report zero and pass a "no second PR"
// assertion for the wrong reason.
//
// -merged and -state are separate string flags rather than one, because
// Forgejo encodes a merged PR as state "closed" WITH merged true, and the
// demo asserts both halves: a shim that hard-coded merged=true would
// satisfy the merged assertion on its own, and state alone cannot
// distinguish merged from closed-without-merging.
func runCheckPRs(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("check-prs", flag.ContinueOnError)
	label := fs.String("label", "pull requests", "what is being checked, echoed into pass/fail messages")
	count := fs.Int("count", unsetCount, "assert exactly this many pull requests exist (0 is meaningful)")
	number := fs.Int("number", unsetCount, "address the pull request with this number (required by the assertions below)")
	wantHead := fs.String("head", "", "assert the addressed PR's head branch")
	wantBase := fs.String("base", "", "assert the addressed PR's base branch")
	wantTitle := fs.String("title", "", "assert the addressed PR's title")
	wantState := fs.String("state", "", "assert the addressed PR's state: open or closed")
	wantMerged := fs.String("merged", "", "assert the addressed PR's merged flag: \"true\" or \"false\"")
	wantURL := fs.String("url", "", "assert the addressed PR's html_url")
	description := fs.String("description", "", "assert the addressed PR's body is exactly this description plus the Loam attribution footer")
	denyBody := fs.String("deny-body", "", "comma-separated: assert none of these substrings appears anywhere in the body")
	if err := fs.Parse(args); err != nil {
		return err
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	var prs []pullRequestDoc
	if err := json.Unmarshal(payload, &prs); err != nil {
		return fmt.Errorf("decoding %s: %w (payload: %s)", *label, err, truncate(string(payload)))
	}
	var failures []string
	failures = appendCountFailure(failures, "pull request", len(prs), *count)
	wantsRow := *wantHead != "" || *wantBase != "" || *wantTitle != "" || *wantState != "" ||
		*wantMerged != "" || *wantURL != "" || *description != "" || *denyBody != ""
	switch {
	case *number == unsetCount && wantsRow:
		failures = append(failures, "-head/-base/-title/-state/-merged/-url/-description/-deny-body address one pull request; pass -number to name it")
	case *number != unsetCount:
		failures = append(failures, prRowFailures(prs, *number, prAssertions{
			head: *wantHead, base: *wantBase, title: *wantTitle, state: *wantState,
			merged: *wantMerged, url: *wantURL, description: *description, denyBody: *denyBody,
		})...)
	}
	return report(stdout, *label, failures)
}

// prAssertions groups check-prs' per-PR flags so prRowFailures keeps a
// signature short enough to read.
type prAssertions struct {
	head, base, title, state, merged, url, description, denyBody string
}

// prRowFailures applies the per-PR assertions to the single PR with the
// given number.
func prRowFailures(prs []pullRequestDoc, number int, want prAssertions) []string {
	var row *pullRequestDoc
	for i := range prs {
		if prs[i].Number == number {
			row = &prs[i]
			break
		}
	}
	if row == nil {
		numbers := make([]string, 0, len(prs))
		for _, pr := range prs {
			numbers = append(numbers, strconv.Itoa(pr.Number))
		}
		return []string{fmt.Sprintf("no pull request numbered %d; the forge has %v", number, numbers)}
	}
	var failures []string
	if want.head != "" && row.Head.Ref != want.head {
		failures = append(failures, fmt.Sprintf("PR #%d's head branch is %q, want %q", number, row.Head.Ref, want.head))
	}
	if want.base != "" && row.Base.Ref != want.base {
		failures = append(failures, fmt.Sprintf("PR #%d's base branch is %q, want %q", number, row.Base.Ref, want.base))
	}
	if want.title != "" && row.Title != want.title {
		failures = append(failures, fmt.Sprintf("PR #%d's title is %q, want %q", number, row.Title, want.title))
	}
	if want.state != "" && row.State != want.state {
		failures = append(failures, fmt.Sprintf("PR #%d's state is %q, want %q", number, row.State, want.state))
	}
	if want.url != "" && row.HTMLURL != want.url {
		failures = append(failures, fmt.Sprintf("PR #%d's html_url is %q, want %q", number, row.HTMLURL, want.url))
	}
	switch want.merged {
	case "":
	case "true", "false":
		if strconv.FormatBool(row.Merged) != want.merged {
			failures = append(failures, fmt.Sprintf("PR #%d has merged=%t, want merged=%s", number, row.Merged, want.merged))
		}
	default:
		failures = append(failures, fmt.Sprintf("-merged %q is neither \"true\" nor \"false\"", want.merged))
	}
	if want.description != "" {
		if wantBody := want.description + "\n\n" + attributionFooter; row.Body != wantBody {
			failures = append(failures, fmt.Sprintf("PR #%d's body is %q, want the description followed by the attribution footer: %q", number, row.Body, wantBody))
		}
	}
	for _, deny := range split(want.denyBody) {
		if strings.Contains(row.Body, deny) {
			failures = append(failures, fmt.Sprintf("PR #%d's body contains %q, which must never reach an upstream PR body", number, deny))
		}
	}
	return failures
}
