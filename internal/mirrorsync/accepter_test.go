package mirrorsync

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

const (
	acceptRepoName    = "acme/widgets"
	acceptForgeHost   = "forge.example.com"
	acceptUpstreamURL = "https://forge.example.com/acme/widgets.git"
	acceptDataDir     = "/srv/loam"
	acceptBranchName  = "wb-9c2f1a"
	acceptTitle       = "Add the widget"
	acceptDescription = "This adds the widget and wires it up."
	acceptPRURL       = "https://forge.example.com/acme/widgets/pulls/7"
)

// createPRCall records one CreatePR invocation in full, so an assertion
// about the body, the head branch, or the target reads the exact arguments
// the engine passed rather than a re-derivation of them.
type createPRCall struct {
	repo, head, target, title, description string
}

// recordCall records one RecordUpstreamPR invocation.
type recordCall struct {
	id     uuid.UUID
	prURL  string
	number int32
}

// pushCall records one upstream push invocation.
type pushCall struct {
	host, mirrorDir, upstreamURL, refspec string
}

// acceptHarness wires a StoreProposalAccepter over mocks that are ALL
// configured, always -- including the ones a given test expects never to
// fire. That is deliberate and it is what makes the negative assertions in
// this file worth anything: a "CreatePR was never called" check must fail
// on an assertion against a recorded call slice, not on an unconfigured
// moq panicking, because a panic proves only that the test reached a
// method, never that the guard under test is the reason it did not.
type acceptHarness struct {
	accepter *StoreProposalAccepter
	pushes   *[]pushCall
	creates  *[]createPRCall
	finds    *[]createPRCall
	records  *[]recordCall
	branch   *workbranchstore.WorkBranch
}

// acceptHarnessOpts configures the one fixture row and the failure each
// collaborator should inject. Zero values give the happy path: a reviewed,
// unconflicted work branch with no recorded PR, a push that succeeds, and
// a CreatePR that answers #7.
type acceptHarnessOpts struct {
	attribution bool
	branch      *workbranchstore.WorkBranch
	repoErr     error
	branchErr   error
	pushErr     error
	createErr   error
	createURL   string
	createNum   int
	findErr     error
	findFound   bool
	findURL     string
	findNum     int
	recordErr   error
	// rereadBranch, when set, is what GetByName returns on its SECOND
	// call -- the re-read the engine makes after losing the recorded
	// column to a concurrent accept.
	rereadBranch *workbranchstore.WorkBranch
}

// acceptBranchFixture is the default work branch every accept test starts
// from: reviewed, unconflicted, titled and described, with no PR recorded.
func acceptBranchFixture(id uuid.UUID) workbranchstore.WorkBranch {
	title, description := acceptTitle, acceptDescription
	return workbranchstore.WorkBranch{
		ID:          id,
		Name:        acceptBranchName,
		Target:      "main",
		Title:       &title,
		Description: &description,
		State:       workbranchstore.StateReviewed,
		Conflict:    workbranchstore.ConflictNone,
		Author:      "agent-alpha",
	}
}

func newAcceptHarness(t *testing.T, opts acceptHarnessOpts) acceptHarness {
	t.Helper()
	repoID, wbID := uuid.New(), uuid.New()
	branch := acceptBranchFixture(wbID)
	if opts.branch != nil {
		branch = *opts.branch
	}
	// Both fields are defaulted only when NEITHER is set, so a test can
	// still express "the forge answered with an empty URL" (createNum
	// set, createURL deliberately empty) without the fixture helpfully
	// filling it back in -- which would silently unmake the assertion.
	if opts.createNum == 0 && opts.createURL == "" {
		opts.createNum, opts.createURL = 7, acceptPRURL
	}
	pushes, creates, finds, records := new([]pushCall), new([]createPRCall), new([]createPRCall), new([]recordCall)
	reads := 0
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			if opts.repoErr != nil {
				return reposstore.Repo{}, opts.repoErr
			}
			if name != acceptRepoName {
				return reposstore.Repo{}, errors.New("unexpected repo name " + name)
			}
			return reposstore.Repo{ID: repoID, Name: name, ForgeHost: acceptForgeHost, UpstreamURL: acceptUpstreamURL}, nil
		},
	}
	branches := &workBranchByNameLookupMock{
		GetByNameFunc: func(_ context.Context, gotRepoID uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
			reads++
			if opts.branchErr != nil {
				return workbranchstore.WorkBranch{}, opts.branchErr
			}
			if gotRepoID != repoID {
				return workbranchstore.WorkBranch{}, errors.New("work branch lookup was not scoped to the repo")
			}
			if name != branch.Name {
				return workbranchstore.WorkBranch{}, errors.New("unexpected work branch name " + name)
			}
			if reads > 1 && opts.rereadBranch != nil {
				return *opts.rereadBranch, nil
			}
			return branch, nil
		},
	}
	upstream := &upstreamRefPusherMock{
		PushFunc: func(_ context.Context, host, mirrorDir, upstreamURL, refspec string) ([]byte, error) {
			*pushes = append(*pushes, pushCall{host: host, mirrorDir: mirrorDir, upstreamURL: upstreamURL, refspec: refspec})
			if opts.pushErr != nil {
				return nil, opts.pushErr
			}
			return nil, nil
		},
	}
	prForge := &pullRequestOpenerMock{
		CreatePRFunc: func(_ context.Context, repo, head, target, title, description string) (string, int, error) {
			*creates = append(*creates, createPRCall{repo: repo, head: head, target: target, title: title, description: description})
			if opts.createErr != nil {
				return "", 0, opts.createErr
			}
			return opts.createURL, opts.createNum, nil
		},
		FindOpenPRFunc: func(_ context.Context, repo, head, target string) (string, int, bool, error) {
			*finds = append(*finds, createPRCall{repo: repo, head: head, target: target})
			if opts.findErr != nil {
				return "", 0, false, opts.findErr
			}
			return opts.findURL, opts.findNum, opts.findFound, nil
		},
	}
	recorder := &workBranchPRRecorderMock{
		RecordUpstreamPRFunc: func(_ context.Context, id uuid.UUID, prURL string, number int32) (workbranchstore.WorkBranch, error) {
			*records = append(*records, recordCall{id: id, prURL: prURL, number: number})
			if opts.recordErr != nil {
				return workbranchstore.WorkBranch{}, opts.recordErr
			}
			updated := branch
			updated.UpstreamPRURL, updated.UpstreamPRNumber = &prURL, &number
			return updated, nil
		},
	}
	accepter := NewStoreProposalAccepter(acceptDataDir, testLogger(), opts.attribution, repos, branches, recorder, prForge, upstream)
	return acceptHarness{accepter: accepter, pushes: pushes, creates: creates, finds: finds, records: records, branch: &branch}
}

// accept runs the engine against the fixture repo/branch.
func (h acceptHarness) accept(ctx context.Context) (AcceptResult, error) {
	return h.accepter.AcceptProposal(ctx, RepoID(acceptRepoName), h.branch.Name)
}

// TestAcceptProposal_PushesTheNamespacedBranchAndOpensThePR is the happy
// path in full: the refspec, the PR arguments, and the recorded columns,
// all read off the exact calls the engine made.
func TestAcceptProposal_PushesTheNamespacedBranchAndOpensThePR(t *testing.T) {
	t.Parallel()
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true})
	result, err := h.accept(t.Context())
	require.NoError(t, err)
	require.Len(t, *h.pushes, 1)
	assert.Equal(t, "refs/heads/wb-9c2f1a:refs/heads/loam/wb-9c2f1a", (*h.pushes)[0].refspec)
	assert.Equal(t, acceptForgeHost, (*h.pushes)[0].host)
	assert.Equal(t, acceptUpstreamURL, (*h.pushes)[0].upstreamURL)
	assert.Equal(t, "/srv/loam/mirrors/acme/widgets.git", (*h.pushes)[0].mirrorDir)
	require.Len(t, *h.creates, 1)
	assert.Equal(t, "loam/wb-9c2f1a", (*h.creates)[0].head)
	assert.Equal(t, "main", (*h.creates)[0].target)
	assert.Equal(t, acceptTitle, (*h.creates)[0].title)
	require.Len(t, *h.records, 1)
	assert.Equal(t, h.branch.ID, (*h.records)[0].id)
	assert.Equal(t, acceptPRURL, (*h.records)[0].prURL)
	assert.Equal(t, int32(7), (*h.records)[0].number)
	assert.Equal(t, AcceptResult{UpstreamBranch: "loam/wb-9c2f1a", PRURL: acceptPRURL, PRNumber: 7, CreatedPR: true}, result)
}

// TestAcceptProposal_ReAcceptFastForwardsWithoutOpeningASecondPR is the
// headline idempotency proof: accepting a branch that ALREADY carries a
// recorded PR number pushes again (that push is what updates the PR) and
// calls CreatePR exactly zero more times.
//
// The two accepts run against the same harness and the same mocks, so the
// call counters are cumulative across both -- exactly one CreatePR and
// exactly one RecordUpstreamPR for two accepts. Deleting the null-check in
// AcceptProposal makes the second accept call CreatePR again and this
// fails on Len(creates) == 1, not on an error return: the second accept is
// asserted to SUCCEED, so a test that passed only because the second call
// errored out for an unrelated reason is ruled out.
func TestAcceptProposal_ReAcceptFastForwardsWithoutOpeningASecondPR(t *testing.T) {
	t.Parallel()
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true})
	first, err := h.accept(t.Context())
	require.NoError(t, err)
	require.True(t, first.CreatedPR)
	// The catch-up/re-review round leaves the row exactly as the first
	// accept wrote it: same branch, PR recorded.
	number := int32(first.PRNumber)
	url := first.PRURL
	h.branch.UpstreamPRNumber, h.branch.UpstreamPRURL = &number, &url

	second, err := h.accept(t.Context())
	require.NoError(t, err, "a re-accept must succeed, not error out; a failing second accept would make the call-count assertions below vacuous")
	assert.False(t, second.CreatedPR)
	assert.Equal(t, first.PRNumber, second.PRNumber)
	assert.Equal(t, first.PRURL, second.PRURL)
	assert.Len(t, *h.creates, 1, "a re-accept must not open a second upstream PR")
	assert.Len(t, *h.records, 1, "a re-accept must not re-record the PR number")
	assert.Len(t, *h.pushes, 2, "a re-accept must still push, since that push is what fast-forwards the existing PR")
	assert.Equal(t, (*h.pushes)[0], (*h.pushes)[1], "the re-accept must push the same refspec to the same branch")
}

// TestAcceptProposal_PushIsNeverForced pins the no-force property at the
// exact argument the engine controls. A '+' anywhere in the refspec, on
// either side of the colon, is a force marker; there is no other channel,
// since the push seam carries no flags and gittransport never adds
// --force.
func TestAcceptProposal_PushIsNeverForced(t *testing.T) {
	t.Parallel()
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true})
	_, err := h.accept(t.Context())
	require.NoError(t, err)
	require.Len(t, *h.pushes, 1)
	refspec := (*h.pushes)[0].refspec
	assert.NotContains(t, refspec, "+", "a '+' in the refspec is a forced update")
	assert.Equal(t, "refs/heads/wb-9c2f1a:refs/heads/loam/wb-9c2f1a", refspec)
}

// TestUpstreamProposalRefspec_NeverProducesAForceRefspec sweeps the
// adversarial names a work_branches.name column could hold if something
// upstream of it broke, and proves each is either rejected outright or
// produces a refspec with no force marker and no second refspec smuggled
// in. This is the property upstreamProposalRefspec's structural claim
// rests on, tested at the constructor rather than only through one happy
// path.
func TestUpstreamProposalRefspec_NeverProducesAForceRefspec(t *testing.T) {
	t.Parallel()
	names := []string{
		"wb-9c2f1a", "+wb-9c2f1a", "wb-9c2f1a:refs/heads/main", "../main",
		"--force", "-f", "wb/../main", "refs/heads/main", "", "wb 9c2f1a", "+",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refspec, upstreamBranch, err := upstreamProposalRefspec(name)
			if err != nil {
				assert.ErrorIs(t, err, errUnsafeWorkBranchName)
				assert.Empty(t, refspec)
				return
			}
			assert.NotContains(t, refspec, "+")
			assert.NotContains(t, refspec, " ")
			assert.Equal(t, "refs/heads/"+name+":refs/heads/loam/"+name, refspec)
			assert.Equal(t, "loam/"+name, upstreamBranch)
		})
	}
}

// TestAcceptProposal_APushFailureRecordsNoPR proves the ordering
// docs/sync-spec.md requires: nothing reaches the forge and nothing
// reaches the row when the branch never made it upstream.
func TestAcceptProposal_APushFailureRecordsNoPR(t *testing.T) {
	t.Parallel()
	pushErr := errors.New("connection refused")
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true, pushErr: pushErr})
	_, err := h.accept(t.Context())
	require.ErrorIs(t, err, pushErr)
	assert.Empty(t, *h.creates, "a failed push must not open a PR")
	assert.Empty(t, *h.records, "a failed push must not record a PR")
}

// TestAcceptProposal_ACreatePRFailureLeavesTheNumberUnrecorded proves the
// retry contract: an ordinary CreatePR failure (network, 5xx, a cancelled
// context) records nothing, so upstream_pr_number stays NULL and a
// re-accept re-attempts against the branch the first push already created.
func TestAcceptProposal_ACreatePRFailureLeavesTheNumberUnrecorded(t *testing.T) {
	t.Parallel()
	createErr := errors.New("502 bad gateway")
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true, createErr: createErr})
	_, err := h.accept(t.Context())
	require.ErrorIs(t, err, createErr)
	assert.Len(t, *h.pushes, 1)
	assert.Empty(t, *h.records, "a failed CreatePR must leave upstream_pr_number NULL")
	assert.Empty(t, *h.finds, "only a duplicate rejection may trigger a lookup; a transport failure must not be read as one")
}

// TestAcceptProposal_ADuplicateRejectionAdoptsTheExistingPRByLookup is the
// "the forge said no" case that is NOT a failure: the previous accept
// created the PR and died before recording it, so this one adopts it. The
// number must come from FindOpenPR -- forge.ErrDuplicatePR's message
// carries Forgejo's internal id, which equals the per-repo number only on
// a repo's first PR.
func TestAcceptProposal_ADuplicateRejectionAdoptsTheExistingPRByLookup(t *testing.T) {
	t.Parallel()
	h := newAcceptHarness(t, acceptHarnessOpts{
		attribution: true,
		createErr:   forge.ErrDuplicatePR,
		findFound:   true,
		findURL:     "https://forge.example.com/acme/widgets/pulls/42",
		findNum:     42,
	})
	result, err := h.accept(t.Context())
	require.NoError(t, err)
	require.Len(t, *h.finds, 1)
	assert.Equal(t, "loam/wb-9c2f1a", (*h.finds)[0].head)
	assert.Equal(t, "main", (*h.finds)[0].target)
	require.Len(t, *h.records, 1)
	assert.Equal(t, int32(42), (*h.records)[0].number, "the adopted number must come from the lookup, not from the duplicate rejection")
	assert.Equal(t, 42, result.PRNumber)
}

// TestAcceptProposal_ADuplicateWithNoFindableePRRecordsNothing pins the
// genuinely ambiguous answer: the forge refused as a duplicate but reports
// no open PR for the pair. Nothing is recorded and the error is its own
// distinguishable sentinel -- the non-destructive reading, since a
// fabricated number would permanently consume the row's one-shot guard.
func TestAcceptProposal_ADuplicateWithNoFindablePRRecordsNothing(t *testing.T) {
	t.Parallel()
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true, createErr: forge.ErrDuplicatePR, findFound: false})
	_, err := h.accept(t.Context())
	require.ErrorIs(t, err, errPRVanishedAfterDuplicate)
	assert.Empty(t, *h.records)
}

// TestAcceptProposal_AFailedLookupAfterADuplicateIsNotReadAsAbsence
// separates "the forge answered, and said there is no such PR" from "the
// lookup itself failed". Only the first is an absence; conflating them is
// exactly the class of bug 5aaf563 and loam-giq.5 were.
func TestAcceptProposal_AFailedLookupAfterADuplicateIsNotReadAsAbsence(t *testing.T) {
	t.Parallel()
	findErr := errors.New("context deadline exceeded")
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true, createErr: forge.ErrDuplicatePR, findErr: findErr})
	_, err := h.accept(t.Context())
	require.ErrorIs(t, err, findErr)
	assert.NotErrorIs(t, err, errPRVanishedAfterDuplicate, "a failed lookup is not a report that no PR exists")
	assert.Empty(t, *h.records)
}

// TestAcceptProposal_AnUnusablePRNumberIsNeverRecorded proves the engine
// validates what a SUCCESSFUL CreatePR returned before trusting it. A #0
// written to the column would both consume the row's one-shot idempotency
// guard and park the branch in StorePRPoller's poll set forever.
func TestAcceptProposal_AnUnusablePRNumberIsNeverRecorded(t *testing.T) {
	t.Parallel()
	// A negative number stands in for the zero the fixture defaulting
	// would otherwise swallow; validatePRIdentity's own table below
	// covers zero itself directly.
	for name, opts := range map[string]acceptHarnessOpts{
		"negative number":     {attribution: true, createNum: -5, createURL: acceptPRURL},
		"empty url":           {attribution: true, createNum: 7, createURL: ""},
		"empty url and no id": {attribution: true, createNum: -1, createURL: ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newAcceptHarness(t, opts)
			_, err := h.accept(t.Context())
			require.ErrorIs(t, err, errUnusablePRIdentity)
			assert.Empty(t, *h.records)
		})
	}
}

// TestValidatePRIdentity covers the validator directly, including the
// zero-number case the harness's own defaulting cannot express.
func TestValidatePRIdentity(t *testing.T) {
	t.Parallel()
	url, number, err := validatePRIdentity(acceptRepoName, "loam/wb-9c2f1a", acceptPRURL, 7)
	require.NoError(t, err)
	assert.Equal(t, acceptPRURL, url)
	assert.Equal(t, 7, number)
	_, _, err = validatePRIdentity(acceptRepoName, "loam/wb-9c2f1a", acceptPRURL, 0)
	assert.ErrorIs(t, err, errUnusablePRIdentity)
	_, _, err = validatePRIdentity(acceptRepoName, "loam/wb-9c2f1a", "", 7)
	assert.ErrorIs(t, err, errUnusablePRIdentity)
}

// TestAcceptProposal_BodyCarriesTheDescriptionAndTheLoamFooter pins the
// exact body an attributed PR gets: the description verbatim, a blank
// line, then the footer docs/sync-spec.md specifies and nothing else.
func TestAcceptProposal_BodyCarriesTheDescriptionAndTheLoamFooter(t *testing.T) {
	t.Parallel()
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true})
	_, err := h.accept(t.Context())
	require.NoError(t, err)
	require.Len(t, *h.creates, 1)
	assert.Equal(t, acceptDescription+"\n\n---\nProposed via Loam.", (*h.creates)[0].description)
}

// TestAcceptProposal_BodyNamesLoamAndNoAgent is sync.feature's "no agent
// identity appears in the body" as an assertion: neither the authoring
// agent nor the work branch's own name leaks into what upstream reviewers
// read.
func TestAcceptProposal_BodyNamesLoamAndNoAgent(t *testing.T) {
	t.Parallel()
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true})
	_, err := h.accept(t.Context())
	require.NoError(t, err)
	require.Len(t, *h.creates, 1)
	body := (*h.creates)[0].description
	assert.Contains(t, body, "Proposed via Loam.")
	assert.NotContains(t, body, h.branch.Author)
	assert.NotContains(t, body, h.branch.Name)
}

// TestAcceptProposal_AttributionDisabledSendsTheDescriptionAlone pins the
// gate: with LOAM_PR_ATTRIBUTION false the body is the description byte
// for byte -- no footer, no separator, not even a trailing newline.
func TestAcceptProposal_AttributionDisabledSendsTheDescriptionAlone(t *testing.T) {
	t.Parallel()
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: false})
	_, err := h.accept(t.Context())
	require.NoError(t, err)
	require.Len(t, *h.creates, 1)
	assert.Equal(t, acceptDescription, (*h.creates)[0].description)
	assert.NotContains(t, (*h.creates)[0].description, "Proposed via Loam")
}

// TestPRBody covers the footer builder directly, including the empty
// description defensive branch AcceptProposal itself cannot reach (a
// reviewed branch always has one).
func TestPRBody(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "body\n\n---\nProposed via Loam.", prBody("body", true))
	assert.Equal(t, "body", prBody("body", false))
	assert.Equal(t, "---\nProposed via Loam.", prBody("", true))
	assert.Empty(t, prBody("", false))
}

// TestAcceptProposal_RejectsABranchThatIsNotReviewed proves the state
// precondition is enforced here rather than trusted to the caller: an
// unreviewed branch never reaches the forge at all.
func TestAcceptProposal_RejectsABranchThatIsNotReviewed(t *testing.T) {
	t.Parallel()
	for _, state := range []workbranchstore.State{
		workbranchstore.StateDraft, workbranchstore.StateReviewable,
		workbranchstore.StateComplete, workbranchstore.StateClosed,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			branch := acceptBranchFixture(uuid.New())
			branch.State = state
			h := newAcceptHarness(t, acceptHarnessOpts{attribution: true, branch: &branch})
			_, err := h.accept(t.Context())
			require.ErrorIs(t, err, errProposalNotReviewed)
			assert.Empty(t, *h.pushes, "an unreviewed branch must never be pushed upstream")
			assert.Empty(t, *h.creates)
			assert.Empty(t, *h.records)
		})
	}
}

// TestAcceptProposal_RejectsAConflictedBranch proves the conflict
// precondition (docs/sync-spec.md's web-spec ripple): a branch that no
// longer merges into its target is never proposed upstream.
func TestAcceptProposal_RejectsAConflictedBranch(t *testing.T) {
	t.Parallel()
	for _, conflict := range []workbranchstore.Conflict{workbranchstore.ConflictFlagged, workbranchstore.ConflictReset} {
		t.Run(string(conflict), func(t *testing.T) {
			t.Parallel()
			branch := acceptBranchFixture(uuid.New())
			branch.Conflict = conflict
			h := newAcceptHarness(t, acceptHarnessOpts{attribution: true, branch: &branch})
			_, err := h.accept(t.Context())
			require.ErrorIs(t, err, errProposalConflicted)
			assert.Empty(t, *h.pushes)
			assert.Empty(t, *h.creates)
		})
	}
}

// TestAcceptProposal_AConcurrentAcceptThatWonTheColumnIsAdopted covers the
// race the store's guarded UPDATE catches: this call opened a PR and lost
// the column. The reported number must be the one that WON, read back off
// the row -- not the one this call happens to hold.
func TestAcceptProposal_AConcurrentAcceptThatWonTheColumnIsAdopted(t *testing.T) {
	t.Parallel()
	winner := int32(99)
	winnerURL := "https://forge.example.com/acme/widgets/pulls/99"
	reread := acceptBranchFixture(uuid.New())
	reread.UpstreamPRNumber, reread.UpstreamPRURL = &winner, &winnerURL
	h := newAcceptHarness(t, acceptHarnessOpts{
		attribution:  true,
		recordErr:    workbranchstore.ErrPRAlreadyRecorded,
		rereadBranch: &reread,
	})
	result, err := h.accept(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 99, result.PRNumber)
	assert.Equal(t, winnerURL, result.PRURL)
	assert.False(t, result.CreatedPR)
}

// TestAcceptProposal_ARecordFailureThatIsNotTheRaceIsReported proves an
// ordinary store failure is surfaced, never quietly treated as a
// successful accept.
func TestAcceptProposal_ARecordFailureThatIsNotTheRaceIsReported(t *testing.T) {
	t.Parallel()
	recordErr := errors.New("connection reset by peer")
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true, recordErr: recordErr})
	_, err := h.accept(t.Context())
	require.ErrorIs(t, err, recordErr)
}

// TestAcceptProposal_RejectsAnUnsafeWorkBranchName proves a name that
// could escape the loam/ namespace is refused before any push, sharing
// safeWorkBranchName with the poller's delete path so both ends of a
// proposal's upstream lifecycle agree on which refs are Loam's.
func TestAcceptProposal_RejectsAnUnsafeWorkBranchName(t *testing.T) {
	t.Parallel()
	branch := acceptBranchFixture(uuid.New())
	branch.Name = "../main"
	h := newAcceptHarness(t, acceptHarnessOpts{attribution: true, branch: &branch})
	_, err := h.accept(t.Context())
	require.ErrorIs(t, err, errUnsafeWorkBranchName)
	assert.Empty(t, *h.pushes)
}
