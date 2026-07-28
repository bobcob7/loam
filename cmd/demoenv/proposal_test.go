package main

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleQueue is one ListProposals response as protojson renders it,
// including the omissions that matter: a false `stale` is absent, and an
// unset `upstreamPrUrl` is absent entirely rather than empty.
const sampleQueue = `{
  "proposals": [
    {
      "workBranch": {"repo":"loam-demo/doc-server","name":"wb-abc123","target":"main","title":"T",
                     "description":"D","state":"WORK_BRANCH_STATE_REVIEWED","author":"ada-lovelace-1-author",
                     "upstreamPrUrl":"http://forge.invalid/loam-demo/doc-server/pulls/1"},
      "verdicts": [{"reviewer":"grace-hopper-2-reviewer","outcome":"VERDICT_OUTCOME_APPROVE","round":2}]
    }
  ],
  "pageInfo": {"total": 1}
}`

// sampleEmptyQueue is what an empty queue actually looks like on the wire:
// protojson omits the empty repeated field and the zero total, so the
// whole response is "{}" with a nested empty object. A decoder that only
// ever saw a populated response would not notice.
const sampleEmptyQueue = `{"pageInfo":{}}`

// samplePRs is one Forgejo list-pulls response with state=all.
const samplePRs = `[
  {"number":1,"html_url":"http://forge.invalid/loam-demo/doc-server/pulls/1","state":"open","merged":false,
   "title":"T","body":"D\n\n---\nProposed via Loam.","head":{"ref":"loam/wb-abc123"},"base":{"ref":"main"}}
]`

func checkProposals(t *testing.T, payload string, args ...string) error {
	t.Helper()
	return runCheckProposals(args, strings.NewReader(payload), io.Discard)
}

func checkPRs(t *testing.T, payload string, args ...string) error {
	t.Helper()
	return runCheckPRs(args, strings.NewReader(payload), io.Discard)
}

func TestCheckProposals_AllAssertionsHold(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkProposals(t, sampleQueue,
		"-count", "1", "-want-names", "wb-abc123", "-deny-names", "wb-other",
		"-select-name", "wb-abc123", "-state", "WORK_BRANCH_STATE_REVIEWED",
		"-pr-url", "http://forge.invalid/loam-demo/doc-server/pulls/1",
		"-approver", "grace-hopper-2-reviewer", "-round", "2"))
}

// TestCheckProposals_EmptyQueueIsAssertable is demo:m5's "the branch left
// the queue" assertion. The wire form of an empty queue omits the
// `proposals` field entirely, so -count 0 has to hold against "{}" and not
// only against a response carrying an empty array.
func TestCheckProposals_EmptyQueueIsAssertable(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkProposals(t, sampleEmptyQueue, "-count", "0", "-deny-names", "wb-abc123"))
}

func TestCheckProposals_DenyNamesCatchesAPresentBranch(t *testing.T) {
	t.Parallel()
	err := checkProposals(t, sampleQueue, "-deny-names", "wb-abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 assertion(s) failed")
}

func TestCheckProposals_WrongPRURLFails(t *testing.T) {
	t.Parallel()
	require.Error(t, checkProposals(t, sampleQueue, "-select-name", "wb-abc123", "-pr-url", "http://forge.invalid/pulls/2"))
}

// TestCheckProposals_AVerdictInAnotherRoundDoesNotCount pins the
// precondition demo:m5's second accept turns on: AcceptProposal requires an
// approve IN THE CURRENT ROUND, so an approve recorded in an earlier round
// must not satisfy the assertion that the branch is re-approved.
func TestCheckProposals_AVerdictInAnotherRoundDoesNotCount(t *testing.T) {
	t.Parallel()
	require.Error(t, checkProposals(t, sampleQueue, "-select-name", "wb-abc123", "-approver", "grace-hopper-2-reviewer", "-round", "1"))
}

// TestCheckProposals_ADisapproveIsNotAnApproval pins the other half: the
// queue carries the round's verdicts whatever they are, and "the reviewer
// voted" is not the precondition.
func TestCheckProposals_ADisapproveIsNotAnApproval(t *testing.T) {
	t.Parallel()
	payload := strings.Replace(sampleQueue, "VERDICT_OUTCOME_APPROVE", "VERDICT_OUTCOME_DISAPPROVE", 1)
	require.Error(t, checkProposals(t, payload, "-select-name", "wb-abc123", "-approver", "grace-hopper-2-reviewer", "-round", "2"))
}

// TestCheckProposals_AStaleApproveIsRejected pins the third: a verdict from
// a previous round that somehow arrived flagged stale cannot be the current
// round's approval, and the failure has to say so rather than passing.
func TestCheckProposals_AStaleApproveIsRejected(t *testing.T) {
	t.Parallel()
	payload := strings.Replace(sampleQueue, `"round":2}`, `"round":2,"stale":true}`, 1)
	err := checkProposals(t, payload, "-select-name", "wb-abc123", "-approver", "grace-hopper-2-reviewer", "-round", "2")
	require.Error(t, err)
}

func TestCheckProposals_SelectionRequiresAName(t *testing.T) {
	t.Parallel()
	err := checkProposals(t, sampleQueue, "-state", "WORK_BRANCH_STATE_REVIEWED")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 assertion(s) failed")
}

func TestCheckPRs_AllAssertionsHold(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkPRs(t, samplePRs,
		"-count", "1", "-number", "1",
		"-url", "http://forge.invalid/loam-demo/doc-server/pulls/1",
		"-head", "loam/wb-abc123", "-base", "main", "-title", "T",
		"-state", "open", "-merged", "false", "-description", "D",
		"-deny-body", "ada-lovelace-1-author,admin"))
}

// TestCheckPRs_CountIsTheNoSecondPRAssertion is demo:m5's headline claim.
// A second pull request beside the first must fail -count 1 -- a check
// that only looked up PR #1 would pass with #2 sitting next to it.
func TestCheckPRs_CountIsTheNoSecondPRAssertion(t *testing.T) {
	t.Parallel()
	twoPRs := `[
      {"number":1,"html_url":"u1","state":"open","merged":false,"title":"T","body":"D\n\n---\nProposed via Loam.","head":{"ref":"loam/wb-abc123"},"base":{"ref":"main"}},
      {"number":2,"html_url":"u2","state":"open","merged":false,"title":"T","body":"D\n\n---\nProposed via Loam.","head":{"ref":"loam/wb-abc123"},"base":{"ref":"main"}}
    ]`
	err := checkPRs(t, twoPRs, "-count", "1", "-number", "1", "-head", "loam/wb-abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 assertion(s) failed")
}

// TestCheckPRs_BodyMustBeExactlyDescriptionPlusFooter is why -description
// is an equality check rather than a "contains". A footer buried in the
// middle of a body, or extra text after it, is not what
// docs/sync-spec.md specifies.
func TestCheckPRs_BodyMustBeExactlyDescriptionPlusFooter(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`D`,
		`---\nProposed via Loam.\n\nD`,
		`D\n\n---\nProposed via Loam.\n\nopened by ada-lovelace`,
		`D\n---\nProposed via Loam.`,
	} {
		payload := strings.Replace(samplePRs, `D\n\n---\nProposed via Loam.`, body, 1)
		require.Error(t, checkPRs(t, payload, "-number", "1", "-description", "D"), "body %q must not satisfy the footer assertion", body)
	}
}

// TestCheckPRs_DenyBodyCatchesAnAgentIdentity pins the "only Loam" half:
// no agent identity may reach an upstream PR body, and a grep for the
// footer alone would never notice one.
func TestCheckPRs_DenyBodyCatchesAnAgentIdentity(t *testing.T) {
	t.Parallel()
	payload := strings.Replace(samplePRs, `D\n\n---\nProposed via Loam.`, `D by ada-lovelace-1-author\n\n---\nProposed via Loam.`, 1)
	require.Error(t, checkPRs(t, payload, "-number", "1", "-deny-body", "ada-lovelace-1-author"))
}

// TestCheckPRs_MergedAndStateAreSeparateClaims pins why the two flags are
// not one. Forgejo encodes a merged PR as state "closed" WITH merged true,
// so state alone cannot distinguish merged from closed-without-merging.
func TestCheckPRs_MergedAndStateAreSeparateClaims(t *testing.T) {
	t.Parallel()
	closedNotMerged := strings.Replace(samplePRs, `"state":"open"`, `"state":"closed"`, 1)
	require.NoError(t, checkPRs(t, closedNotMerged, "-number", "1", "-state", "closed", "-merged", "false"))
	require.Error(t, checkPRs(t, closedNotMerged, "-number", "1", "-state", "closed", "-merged", "true"))
}

func TestCheckPRs_MissingNumberIsAFailure(t *testing.T) {
	t.Parallel()
	err := checkPRs(t, samplePRs, "-number", "7", "-head", "loam/wb-abc123")
	require.Error(t, err)
}

func TestCheckPRs_AssertionsRequireANumber(t *testing.T) {
	t.Parallel()
	err := checkPRs(t, samplePRs, "-head", "loam/wb-abc123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 assertion(s) failed")
}

func TestCheckPRs_RejectsANonBooleanMergedFlag(t *testing.T) {
	t.Parallel()
	require.Error(t, checkPRs(t, samplePRs, "-number", "1", "-merged", "yes"))
}
