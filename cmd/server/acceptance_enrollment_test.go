//go:build acceptance

// Step definitions for features/enrollment.feature (loam-ofg.12):
// RepoAdminService's EnrollRepo, ProbeRepo, SetTargetBranches, GetRepo, and
// RemoveRepo, driven as the admin actor over the real, in-process server.
//
// # The identity-preserving proxy
//
// Every scenario in this file enrolls or probes a repo whose upstream_url
// is a literal like "https://github.com/bobcob7/doc-server", and several
// scenarios assert the resulting identifier is EXACTLY "bobcob7/doc-server"
// (RepoAdminService derives it from the URL's own path -- handler.go's
// deriveRepoIdentity). That derivation has to run against a URL this suite
// can actually reach, which rules out the literal github.com URL, and it
// also rules out the shared fakeforge instance's own GitURL: that helper
// mounts every repo under a fixed "/git" segment (fakeforge/server.go's
// gitPathPrefix), so a repo named "bobcob7/doc-server" resolves through it
// to a URL whose path derives "git/bobcob7/doc-server" -- three segments,
// rejected outright by validRepoName's exact "<group>/<repo_name>" rule
// (see internal/handler/repoadmin/enroll_integration_test.go's own doc
// comment, which embraces that quirk for ITS chosen repo name "widgets" ->
// "git/widgets" rather than fighting it).
//
// enrollmentIdentityProxy is how this file avoids that quirk instead of
// inheriting it: a second, private httptest.Server, built once for the
// whole suite, that fronts the SAME shared fakeforge instance but
// re-injects the "/git" mount segment on the way in, so ITS OWN base URL
// carries none of it. A repo seeded as "bobcob7/doc-server" and addressed
// through THIS proxy therefore derives the identifier "bobcob7/doc-server"
// exactly, while every byte still lands in the one shared fakeforge
// instance every other scenario in this suite already uses (same tokens,
// same git storage, same provider REST surface) -- nothing about the fake
// forge itself changes, and no production code changes either.
//
// The proxy rewrites only paths that are not one of fakeforge's own fixed
// REST/control mounts ("/api/", "/provider/", "/control/"): those three are
// addressed directly by host + a fixed path (CredentialService's live
// token validation, RepoAdminService.EnrollRepo's write-access probe), and
// prepending "/git" to them would 404 against fakeforge's actual mux
// entries instead of reaching them.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/cucumber/godog"
	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/fakeforge"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/mirrorsync"
)

// acceptanceEnrollmentRepo is the one repo identifier every scenario in
// features/enrollment.feature concerns, exactly as clone-and-push.feature's
// scenarios all share the same literal "bobcob7/doc-server" (see
// acceptance_world_test.go's afterScenario doc comment on why that is safe
// across scenarios: the repos row and the fake-forge repo are both torn
// down at the end of every scenario). Used only by the two Given steps
// whose Gherkin text does not name a repo at all ("a work branch
// targeting ... is under review").
const acceptanceEnrollmentRepo = "bobcob7/doc-server"

// registerEnrollmentSteps wires every step features/enrollment.feature
// needs. It also builds this suite's one identity-preserving proxy (see
// this file's own doc comment) -- once, since registerEnrollmentSteps
// itself is called exactly once for the whole suite (initializeScenario's
// own doc comment) -- and closes it via h.t.Cleanup, the same lifetime
// startAcceptanceForge gives the shared fakeforge instance itself.
func (h *acceptanceHarness) registerEnrollmentSteps(sc *godog.ScenarioContext) {
	proxy := newEnrollmentIdentityProxy(h.forge)
	h.t.Cleanup(proxy.Close)
	sc.Step(`^a working credential exists for the forge host "([^"]*)"$`, func(ctx context.Context, statedHost string) error {
		return h.stepAWorkingCredentialExists(ctx, proxy.URL, statedHost)
	})
	sc.Step(`^I enroll "([^"]*)" with target branch "([^"]*)"$`, func(ctx context.Context, upstreamURL, branch string) error {
		return h.stepIEnroll(ctx, proxy.URL, upstreamURL, branch)
	})
	sc.Step(`^I probe "([^"]*)"$`, func(ctx context.Context, upstreamURL string) error {
		return h.stepIProbe(ctx, proxy.URL, upstreamURL)
	})
	sc.Step(`^"([^"]*)" is enrolled$`, func(ctx context.Context, repo string) error {
		return h.enrollRepoForReal(ctx, worldFrom(ctx), proxy.URL, repo, "main")
	})
	sc.Step(`^the repo "([^"]*)" is enrolled$`, h.stepTheRepoIsEnrolled)
	sc.Step(`^the server clones it and begins syncing and ingesting "([^"]*)"$`, h.stepServerClonesAndBeginsSyncingAndIngesting)
	sc.Step(`^its identifier is "([^"]*)"$`, h.stepItsIdentifierIs)
	sc.Step(`^I see its branches and its default branch$`, h.stepISeeItsBranchesAndDefaultBranch)
	sc.Step(`^the default branch is offered as the indexed branch$`, h.stepDefaultBranchOfferedAsIndexed)
	sc.Step(`^"([^"]*)" is enrolled with target branch "([^"]*)"$`, h.stepRepoIsEnrolled)
	sc.Step(`^"([^"]*)" is enrolled with target branches "([^"]*)" and "([^"]*)" and indexed branch "([^"]*)"$`, h.stepRepoIsEnrolledWithTwoTargetBranches)
	sc.Step(`^I set its target branches to "([^"]*)" and "([^"]*)"$`, h.stepISetTargetBranchesToTwo)
	sc.Step(`^both "([^"]*)" and "([^"]*)" are eligible as work-branch targets$`, h.stepBothAreEligible)
	sc.Step(`^I try to designate "([^"]*)" as the indexed branch$`, h.stepITryToDesignateAsIndexedBranch)
	sc.Step(`^the change is rejected$`, h.stepTheChangeIsRejected)
	sc.Step(`^I change the indexed branch to "([^"]*)"$`, h.stepIChangeIndexedBranchTo)
	sc.Step(`^a full ingest job runs for "([^"]*)"$`, h.stepAFullIngestJobRunsFor)
	sc.Step(`^once it succeeds, graph and search queries reflect "([^"]*)"$`, h.stepGraphAndSearchQueriesReflect)
	sc.Step(`^a work branch targeting "([^"]*)" is under review$`, h.stepAWorkBranchTargetingIsUnderReview)
	sc.Step(`^I remove "([^"]*)" from the target branches$`, h.stepIRemoveFromTargetBranches)
	sc.Step(`^no new work branches can start from "([^"]*)"$`, h.stepNoNewWorkBranchesCanStartFrom)
	sc.Step(`^the existing work branch keeps its lifecycle$`, h.stepExistingWorkBranchKeepsLifecycle)
	sc.Step(`^it has no open work branches$`, h.stepItHasNoOpenWorkBranches)
	sc.Step(`^I view the repo$`, h.stepIViewTheRepo)
	sc.Step(`^it shows a sync state and the time of the last successful sync$`, h.stepShowsSyncStateAndLastSync)
	sc.Step(`^I remove it$`, h.stepIRemoveIt)
	sc.Step(`^it is no longer enrolled$`, h.stepItIsNoLongerEnrolled)
	sc.Step(`^its mirror, graph, and vector data are dropped$`, h.stepMirrorGraphVectorDataDropped)
	sc.Step(`^its work branch history is gone$`, h.stepWorkBranchHistoryIsGone)
	sc.Step(`^a work branch on it is in state "([^"]*)"$`, h.stepAWorkBranchOnItIsInState)
	sc.Step(`^I try to remove it$`, h.stepITryToRemoveIt)
	sc.Step(`^the removal is rejected as a failed precondition$`, h.stepRemovalIsRejectedAsFailedPrecondition)
	sc.Step(`^I am told exactly which work branches block the removal$`, h.stepIAmToldWhichWorkBranchesBlockRemoval)
}

// newEnrollmentIdentityProxy builds the private httptest.Server described
// in this file's own doc comment: every request path is forwarded to forge
// verbatim EXCEPT one that does not already address one of its fixed
// REST/control mounts, which gets forge's own "/git" mount segment
// re-injected before forwarding. The proxy never touches the request
// otherwise (headers, method, body) -- forge.ServeHTTP is called directly,
// in-process, so this adds no real network hop of its own beyond the one
// loopback round trip to the proxy's own listener.
func newEnrollmentIdentityProxy(forge *fakeforge.Server) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/provider/") && !strings.HasPrefix(r.URL.Path, "/control/") {
			r.URL.Path = "/git" + r.URL.Path
		}
		forge.ServeHTTP(w, r)
	}))
}

// enrollmentPathIdentifier extracts the "<group>/<repo_name>" identifier a
// literal upstream URL's own PATH names, mirroring
// internal/handler/repoadmin's deriveRepoIdentity (unexported there, so
// necessarily reproduced here): trim the leading slash, trim a trailing
// ".git". It is deliberately host-independent -- the whole reason this
// file's proxy design works is that the identifier depends only on the
// PATH, so the caller can seed and address the SAME identifier through a
// completely different host (the proxy's) than the literal URL names.
func enrollmentPathIdentifier(literalUpstreamURL string) (string, error) {
	u, err := url.Parse(literalUpstreamURL)
	if err != nil {
		return "", fmt.Errorf("parsing upstream url %s: %w", literalUpstreamURL, err)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	if name == "" {
		return "", fmt.Errorf("upstream url %s has no repo path to derive an identifier from", literalUpstreamURL)
	}
	return name, nil
}

// stepAWorkingCredentialExists is the Background step. statedHost is the
// literal forge host the Gherkin names ("github.com") -- unused for the
// actual credential key, deliberately: the RPCs this file's later steps
// make cannot reach the real github.com, so they address the identity-
// preserving proxy instead (this file's own doc comment), and a credential
// is only found by the EXACT host EnrollRepo/ProbeRepo derive from
// whatever URL they were actually given (loam-4kz: "there is no
// reconciling chokepoint"). The real, admin-facing
// CredentialService.SetUpstreamToken RPC is used rather than a direct
// store write, so this also proves the credential path an admin would
// actually use (including its live token-validation round trip against
// the fake forge) rather than a shortcut around it.
func (h *acceptanceHarness) stepAWorkingCredentialExists(ctx context.Context, proxyURL, statedHost string) error {
	client := adminv1connect.NewCredentialServiceClient(h.adminHTTPClient(), h.server.baseURL)
	if _, err := client.SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{
		Host:  proxyURL,
		Token: acceptanceForgeToken,
	})); err != nil {
		return fmt.Errorf("seeding a working credential (stated host %s, actual host %s): %w", statedHost, proxyURL, err)
	}
	return nil
}

// enrollRepoForReal drives the REAL RepoAdminService.EnrollRepo RPC to
// genuinely enroll repoIdentifier with a single target/indexed branch,
// through proxyURL. Used both by "I enroll <url> ..." (repoIdentifier
// parsed from the literal URL's path) and by "<repo> is enrolled"
// (repoIdentifier IS the literal argument) -- the two differ only in
// where repoIdentifier comes from, not in what enrolling means. A real
// mirror lands at production's own derived path and repos.sync_state
// genuinely reaches idle with a real last_synced_at, which is what makes
// this suitable for the sync-status and removal scenarios too, not only
// the ones that name EnrollRepo directly.
func (h *acceptanceHarness) enrollRepoForReal(ctx context.Context, world *acceptanceWorld, proxyURL, repoIdentifier, branch string) error {
	group, name, ok := strings.Cut(repoIdentifier, "/")
	if !ok {
		return fmt.Errorf("repo identifier %q must be shaped like <group>/<repo_name>", repoIdentifier)
	}
	world.repoGroup, world.repoName, world.targetBranch = group, name, branch
	if err := h.forge.SeedRepoFiles(ctx, repoIdentifier, acceptanceUpstreamFiles(repoIdentifier), fakeforge.SeedOptions{DefaultBranch: branch}); err != nil {
		return fmt.Errorf("seeding upstream repo %s: %w", repoIdentifier, err)
	}
	world.upstreamSeeded = true
	world.upstreamURL = proxyURL + "/" + repoIdentifier
	resp, err := h.newRepoAdminServiceClient().EnrollRepo(ctx, connect.NewRequest(&adminv1.EnrollRepoRequest{
		UpstreamUrl:    world.upstreamURL,
		TargetBranches: []string{branch},
		IndexedBranch:  branch,
	}))
	if err != nil {
		return fmt.Errorf("enrolling %s: %w", repoIdentifier, err)
	}
	world.lastEnrolledRepo = resp.Msg.GetRepo()
	repoID, err := h.repoIDByName(ctx, repoIdentifier)
	if err != nil {
		return fmt.Errorf("looking up repos.id for %s after enrolling: %w", repoIdentifier, err)
	}
	world.repoID = repoID
	world.mirrorDir = mirrorpath.Dir(h.server.dataDir, repoIdentifier)
	return nil
}

// repoIDByName reads repos.id back for name, the row EnrollRepo just
// created -- EnrollRepoResponse's EnrolledRepo carries no id field
// (docs/web-spec.md's proto only ever exposes the "<group>/<repo_name>"
// identifier), so this is the one place that needs the raw uuid: later
// ingest/removal assertions key off world.repoID directly.
func (h *acceptanceHarness) repoIDByName(ctx context.Context, name string) (uuid.UUID, error) {
	var id uuid.UUID
	if err := h.server.pool.QueryRow(ctx, `SELECT id FROM repos WHERE name = $1`, name).Scan(&id); err != nil {
		return uuid.UUID{}, fmt.Errorf("looking up repos.id for %s: %w", name, err)
	}
	return id, nil
}

// stepIEnroll is "When I enroll <url> with target branch <branch>",
// scenarios "Enrolling a repo by upstream URL" and "The repo identifier is
// derived from the URL". Both expect success, so a failure here fails the
// step directly rather than being deferred to a later Then.
func (h *acceptanceHarness) stepIEnroll(ctx context.Context, proxyURL, literalUpstreamURL, branch string) error {
	repoIdentifier, err := enrollmentPathIdentifier(literalUpstreamURL)
	if err != nil {
		return err
	}
	return h.enrollRepoForReal(ctx, worldFrom(ctx), proxyURL, repoIdentifier, branch)
}

// stepTheRepoIsEnrolled asserts BOTH that EnrollRepo's own response named
// repo as the resulting identifier AND that a fresh, independent GetRepo
// read confirms it was actually persisted -- so a handler that fabricated
// its response without writing anything real cannot pass this.
func (h *acceptanceHarness) stepTheRepoIsEnrolled(ctx context.Context, repo string) error {
	world := worldFrom(ctx)
	if world.lastEnrolledRepo == nil {
		return fmt.Errorf("no repo was enrolled in this scenario yet")
	}
	if world.lastEnrolledRepo.GetRepo() != repo {
		return fmt.Errorf("enrolled repo identifier is %q, want %q", world.lastEnrolledRepo.GetRepo(), repo)
	}
	persisted, err := h.getRepoAsAdmin(ctx, repo)
	if err != nil {
		return fmt.Errorf("re-reading %s after enrollment: %w", repo, err)
	}
	if persisted.GetRepo() != repo {
		return fmt.Errorf("GetRepo returned identifier %q, want %q", persisted.GetRepo(), repo)
	}
	return nil
}

// stepServerClonesAndBeginsSyncingAndIngesting proves the "clones it and
// begins syncing and ingesting" half independently of EnrollRepo's own
// response: a real bare mirror on disk carrying branch's actual tip (read
// with plain git, never through the code under test), and a real ingest
// job for branch that this step drains and requires to have SUCCEEDED --
// not merely queued, so this proves the whole pipeline EnrollRepo
// triggers, not just that a row was inserted.
func (h *acceptanceHarness) stepServerClonesAndBeginsSyncingAndIngesting(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	if world.lastEnrolledRepo.GetSync().GetState() != adminv1.SyncState_SYNC_STATE_IDLE {
		return fmt.Errorf("enrolled repo's sync state is %s, want SYNC_STATE_IDLE (EnrollRepo only returns after the clone finishes)", world.lastEnrolledRepo.GetSync().GetState())
	}
	if world.lastEnrolledRepo.GetSync().GetLastSyncedAt() == "" {
		return fmt.Errorf("enrolled repo reports no last_synced_at; the initial clone must record one")
	}
	tip, err := mirrorRefSHA(world.mirrorDir, "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("the mirror for %s has no %s ref on disk: %w", world.repo(), branch, err)
	}
	if tip == "" {
		return fmt.Errorf("the mirror for %s reports an empty SHA for %s", world.repo(), branch)
	}
	if err := h.ingestHarness.DrainIngestQueue(ctx, mirrorsync.RepoID(world.repo())); err != nil {
		return fmt.Errorf("draining the ingest queue for %s: %w", world.repo(), err)
	}
	return h.assertIngestSucceeded(ctx, world)
}

// stepItsIdentifierIs is "Then its identifier is <identifier>", the
// central claim of "The repo identifier is derived from the URL".
func (h *acceptanceHarness) stepItsIdentifierIs(ctx context.Context, identifier string) error {
	world := worldFrom(ctx)
	if world.lastEnrolledRepo == nil {
		return fmt.Errorf("no repo was enrolled in this scenario yet")
	}
	if world.lastEnrolledRepo.GetRepo() != identifier {
		return fmt.Errorf("derived identifier is %q, want %q", world.lastEnrolledRepo.GetRepo(), identifier)
	}
	return nil
}

// stepIProbe is "When I probe <url>", scenario "Probing a repo before
// enrollment lists its branches". Read-only: unlike stepIEnroll, this
// never creates a repos row, so world.repoID stays a zero UUID and
// afterScenario's generic cleanup skips its DELETE FROM repos -- only the
// fake-forge repo itself (world.upstreamSeeded) needs tearing down, which
// that same generic cleanup already does.
func (h *acceptanceHarness) stepIProbe(ctx context.Context, proxyURL, literalUpstreamURL string) error {
	world := worldFrom(ctx)
	repoIdentifier, err := enrollmentPathIdentifier(literalUpstreamURL)
	if err != nil {
		return err
	}
	group, name, ok := strings.Cut(repoIdentifier, "/")
	if !ok {
		return fmt.Errorf("repo identifier %q must be shaped like <group>/<repo_name>", repoIdentifier)
	}
	world.repoGroup, world.repoName = group, name
	if err := h.forge.SeedRepoFiles(ctx, repoIdentifier, acceptanceUpstreamFiles(repoIdentifier), fakeforge.SeedOptions{DefaultBranch: "main"}); err != nil {
		return fmt.Errorf("seeding upstream repo %s: %w", repoIdentifier, err)
	}
	world.upstreamSeeded = true
	world.upstreamURL = proxyURL + "/" + repoIdentifier
	resp, err := h.newRepoAdminServiceClient().ProbeRepo(ctx, connect.NewRequest(&adminv1.ProbeRepoRequest{UpstreamUrl: world.upstreamURL}))
	if err != nil {
		return fmt.Errorf("probing %s: %w", literalUpstreamURL, err)
	}
	world.lastProbeBranches = resp.Msg.GetBranches()
	world.lastProbeHead = resp.Msg.GetHead()
	return nil
}

// stepISeeItsBranchesAndDefaultBranch asserts the probe reported at least
// one branch and a default (head) that is genuinely among them -- not
// merely a non-empty string coincidentally unrelated to the branch list.
func (h *acceptanceHarness) stepISeeItsBranchesAndDefaultBranch(ctx context.Context) error {
	world := worldFrom(ctx)
	if len(world.lastProbeBranches) == 0 {
		return fmt.Errorf("probe returned no branches")
	}
	if world.lastProbeHead == "" {
		return fmt.Errorf("probe returned no default branch (head)")
	}
	if !slices.Contains(world.lastProbeBranches, world.lastProbeHead) {
		return fmt.Errorf("probe's default branch %q is not among its reported branches %v", world.lastProbeHead, world.lastProbeBranches)
	}
	return nil
}

// stepDefaultBranchOfferedAsIndexed proves the probed default branch is
// genuinely usable as indexed_branch -- not merely present in the
// ProbeRepoResponse -- by actually enrolling with it as both the sole
// target and the indexed branch and requiring that to succeed and echo it
// back. ProbeRepoResponse has no separate "offer" field to read instead;
// this is the strongest available proof that what the probe would have a
// UI pre-fill really is enrollment-ready.
func (h *acceptanceHarness) stepDefaultBranchOfferedAsIndexed(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastProbeHead == "" {
		return fmt.Errorf("no default branch was probed to offer as the indexed branch")
	}
	resp, err := h.newRepoAdminServiceClient().EnrollRepo(ctx, connect.NewRequest(&adminv1.EnrollRepoRequest{
		UpstreamUrl:    world.upstreamURL,
		TargetBranches: []string{world.lastProbeHead},
		IndexedBranch:  world.lastProbeHead,
	}))
	if err != nil {
		return fmt.Errorf("the probed default branch %q was not accepted as the indexed branch on enrollment: %w", world.lastProbeHead, err)
	}
	if resp.Msg.GetRepo().GetIndexedBranch() != world.lastProbeHead {
		return fmt.Errorf("enrolled with indexed_branch %q, want the probed default %q", resp.Msg.GetRepo().GetIndexedBranch(), world.lastProbeHead)
	}
	repoID, err := h.repoIDByName(ctx, world.repo())
	if err != nil {
		return err
	}
	world.repoID = repoID
	world.lastEnrolledRepo = resp.Msg.GetRepo()
	world.mirrorDir = mirrorpath.Dir(h.server.dataDir, world.repo())
	return nil
}

// stepRepoIsEnrolledWithTwoTargetBranches is the Given "<repo> is enrolled
// with target branches <a> and <b> and indexed branch <indexed>"
// (scenario "Changing the indexed branch triggers a full ingest"). Both
// upstream branches carry the SAME real, indexable fixture content
// (branchB is created at branchA's own tip, sharing its tree), which is
// what lets the later ingest-of-release assertions find real symbols
// rather than an empty tree. Like stepRepoIsEnrolled (acceptance_steps_
// test.go), this seeds the repos/repo_target_branches rows directly
// rather than through EnrollRepo -- this scenario's own subject is
// SetTargetBranches, not enrollment -- but unlike that lighter fixture, it
// also clones a real mirror immediately: this scenario's Then steps need
// one to ingest from.
func (h *acceptanceHarness) stepRepoIsEnrolledWithTwoTargetBranches(ctx context.Context, repo, branchA, branchB, indexedBranch string) error {
	world := worldFrom(ctx)
	group, name, ok := strings.Cut(repo, "/")
	if !ok {
		return fmt.Errorf("repo %q must be shaped like <group>/<repo_name>", repo)
	}
	world.repoGroup, world.repoName, world.targetBranch = group, name, indexedBranch
	if err := h.forge.SeedRepoFiles(ctx, world.repo(), acceptanceUpstreamFiles(world.repo()), fakeforge.SeedOptions{DefaultBranch: branchA}); err != nil {
		return fmt.Errorf("seeding upstream repo %s: %w", world.repo(), err)
	}
	world.upstreamSeeded = true
	world.upstreamURL = h.forge.GitURL(world.repo())
	if err := h.forge.CreateBranch(ctx, world.repo(), branchB, ""); err != nil {
		return fmt.Errorf("creating upstream branch %s: %w", branchB, err)
	}
	repoID := uuid.Must(uuid.NewV7())
	if _, err := h.server.pool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, $3, $4, $5)`,
		repoID, world.repo(), world.upstreamURL, h.forgeHost, indexedBranch); err != nil {
		return fmt.Errorf("seeding repos row for %s: %w", world.repo(), err)
	}
	for _, branch := range []string{branchA, branchB} {
		if _, err := h.server.pool.Exec(ctx, `INSERT INTO repo_target_branches (repo_id, branch) VALUES ($1, $2)`, repoID, branch); err != nil {
			return fmt.Errorf("seeding repo_target_branches row for %s: %w", branch, err)
		}
	}
	world.repoID = repoID
	return h.ensureMirrorFromUpstream(ctx, world)
}

// stepISetTargetBranchesToTwo is "When I set its target branches to <a>
// and <b>", scenario "Updating the target branches". indexed_branch is
// read back and round-tripped unchanged (mirroring acceptance_roles_
// test.go's stepTheRoleHasInstructionsConfigured convention), since this
// scenario's own subject is the target-branch SET, not the indexed
// branch.
func (h *acceptanceHarness) stepISetTargetBranchesToTwo(ctx context.Context, a, b string) error {
	world := worldFrom(ctx)
	current, err := h.getRepoAsAdmin(ctx, world.repo())
	if err != nil {
		return err
	}
	resp, err := h.newRepoAdminServiceClient().SetTargetBranches(ctx, connect.NewRequest(&adminv1.SetTargetBranchesRequest{
		Repo:           world.repo(),
		TargetBranches: []string{a, b},
		IndexedBranch:  current.GetIndexedBranch(),
	}))
	if err != nil {
		return fmt.Errorf("setting target branches to %s and %s: %w", a, b, err)
	}
	world.lastEnrolledRepo = resp.Msg.GetRepo()
	return nil
}

// stepBothAreEligible asserts the repo's persisted target_branches are
// EXACTLY {a, b} -- not merely that both are present among some larger or
// stale set -- read back independently through GetRepo rather than only
// off the SetTargetBranches response.
func (h *acceptanceHarness) stepBothAreEligible(ctx context.Context, a, b string) error {
	world := worldFrom(ctx)
	repo, err := h.getRepoAsAdmin(ctx, world.repo())
	if err != nil {
		return err
	}
	targets := repo.GetTargetBranches()
	if len(targets) != 2 || !slices.Contains(targets, a) || !slices.Contains(targets, b) {
		return fmt.Errorf("repo %s target branches are %v, want exactly [%s %s]", world.repo(), targets, a, b)
	}
	return nil
}

// stepITryToDesignateAsIndexedBranch is "When I try to designate <branch>
// as the indexed branch", scenario "The indexed branch must be a target
// branch". The current target_branches set is left unchanged; only
// indexed_branch names something outside it, which is the one thing this
// scenario means to provoke a rejection on.
func (h *acceptanceHarness) stepITryToDesignateAsIndexedBranch(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	repo, err := h.getRepoAsAdmin(ctx, world.repo())
	if err != nil {
		return err
	}
	_, err = h.newRepoAdminServiceClient().SetTargetBranches(ctx, connect.NewRequest(&adminv1.SetTargetBranchesRequest{
		Repo:           world.repo(),
		TargetBranches: repo.GetTargetBranches(),
		IndexedBranch:  branch,
	}))
	world.lastRPCErr = err
	world.rpcAttempted = true
	return nil
}

// stepTheChangeIsRejected asserts BOTH the RPC-level rejection
// (InvalidArgument, matching repoadmin.SetTargetBranches's own "indexed_
// branch must be one of target_branches" validation) and that the repo's
// PERSISTED indexed_branch is unchanged -- a handler that returned an
// error after already writing the new value would still fail this.
func (h *acceptanceHarness) stepTheChangeIsRejected(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := requireRPCRejected(world.lastRPCErr, "the SetTargetBranches attempt", connect.CodeInvalidArgument); err != nil {
		return err
	}
	repo, err := h.getRepoAsAdmin(ctx, world.repo())
	if err != nil {
		return err
	}
	if repo.GetIndexedBranch() != world.targetBranch {
		return fmt.Errorf("indexed_branch changed to %q despite the rejection; want it to remain %q", repo.GetIndexedBranch(), world.targetBranch)
	}
	return nil
}

// stepIChangeIndexedBranchTo is "When I change the indexed branch to
// <branch>", scenario "Changing the indexed branch triggers a full
// ingest". target_branches are left exactly as the Given set them; only
// indexed_branch moves, which is what production's own SetTargetBranches
// keys its "enqueue a full ingest" branch on (indexedChanged, targets.go).
func (h *acceptanceHarness) stepIChangeIndexedBranchTo(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	current, err := h.getRepoAsAdmin(ctx, world.repo())
	if err != nil {
		return err
	}
	resp, err := h.newRepoAdminServiceClient().SetTargetBranches(ctx, connect.NewRequest(&adminv1.SetTargetBranchesRequest{
		Repo:           world.repo(),
		TargetBranches: current.GetTargetBranches(),
		IndexedBranch:  branch,
	}))
	if err != nil {
		return fmt.Errorf("changing the indexed branch to %s: %w", branch, err)
	}
	world.lastEnrolledRepo = resp.Msg.GetRepo()
	return nil
}

// stepAFullIngestJobRunsFor drains the real ingest job SetTargetBranches
// just enqueued (production's own indexedChanged branch, not anything
// this harness enqueued directly) and requires it to have actually
// succeeded, as kind FULL, for branch specifically -- not merely that
// SOME job for this repo succeeded, which a stale row from the initial
// enrollment could otherwise satisfy vacuously.
func (h *acceptanceHarness) stepAFullIngestJobRunsFor(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	if err := h.ingestHarness.DrainIngestQueue(ctx, mirrorsync.RepoID(world.repo())); err != nil {
		return fmt.Errorf("draining the ingest queue for %s: %w", world.repo(), err)
	}
	var status, kind, jobError string
	err := h.server.pool.QueryRow(ctx,
		`SELECT status, kind, COALESCE(error, '') FROM ingest_jobs WHERE repo_id = $1 AND target_branch = $2 ORDER BY queued_at DESC LIMIT 1`,
		world.repoID, branch).Scan(&status, &kind, &jobError)
	if err != nil {
		return fmt.Errorf("reading the latest ingest job for %s/%s: %w", world.repo(), branch, err)
	}
	if kind != "full" {
		return fmt.Errorf("latest ingest job for %s/%s is kind %q, want full", world.repo(), branch, kind)
	}
	if status != "succeeded" {
		return fmt.Errorf("the ingest job for %s/%s finished as %q: %s", world.repo(), branch, status, jobError)
	}
	return nil
}

// ingestedRefForBranch reads repo_target_branches.ingested_ref for an
// arbitrary branch, the parameterized sibling of acceptance_ingest_test.
// go's ingestedRef (which hardcodes world.targetBranch -- not usable here,
// since this scenario ingests a branch OTHER than whatever world.
// targetBranch happens to hold).
func (h *acceptanceHarness) ingestedRefForBranch(ctx context.Context, repoID uuid.UUID, branch string) (string, error) {
	var ref *string
	if err := h.server.pool.QueryRow(ctx, `SELECT ingested_ref FROM repo_target_branches WHERE repo_id = $1 AND branch = $2`, repoID, branch).Scan(&ref); err != nil {
		return "", fmt.Errorf("reading ingested_ref for %s/%s: %w", repoID, branch, err)
	}
	if ref == nil {
		return "", nil
	}
	return *ref, nil
}

// stepGraphAndSearchQueriesReflect is "Then ... graph and search queries
// reflect <branch>": both a graph and a search query must return at least
// one result, and every ingested entry in each envelope must name branch
// and the exact commit repo_target_branches recorded -- the same
// non-vacuous double proof acceptance_ingest_test.go's
// stepGraphAndSearchReturnResults uses, generalized to an arbitrary
// branch rather than world.targetBranch.
func (h *acceptanceHarness) stepGraphAndSearchQueriesReflect(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	ref, err := h.ingestedRefForBranch(ctx, world.repoID, branch)
	if err != nil {
		return err
	}
	if ref == "" {
		return fmt.Errorf("repo %s reports no ingested commit for %s", world.repo(), branch)
	}
	graph, err := h.runQuery(world, append([]string{"graph", "def", acceptanceDefinedSymbol}, world.scopeArgs()...)...)
	if err != nil {
		return err
	}
	if len(graph.Results) == 0 {
		return fmt.Errorf("graph def %s returned no results for %s", acceptanceDefinedSymbol, branch)
	}
	if err := assertEnvelopeRef(graph, world.repo(), branch, ref); err != nil {
		return fmt.Errorf("graph def %s: %w", acceptanceDefinedSymbol, err)
	}
	search, err := h.runQuery(world, append([]string{"search", "how is authentication handled"}, world.scopeArgs()...)...)
	if err != nil {
		return err
	}
	if len(search.Results) == 0 {
		return fmt.Errorf("search returned no results for %s", branch)
	}
	return assertEnvelopeRef(search, world.repo(), branch, ref)
}

// stepAWorkBranchTargetingIsUnderReview is the ENTIRE Given for "Removing
// a target branch does not end work in flight" -- it names no repo at
// all, so it enrolls acceptanceEnrollmentRepo itself (with target both the
// stated branch and "main", so removing the former later leaves a valid
// indexed branch behind) before seeding the work branch, matching this
// feature's own convention that every scenario concerns one repo.
func (h *acceptanceHarness) stepAWorkBranchTargetingIsUnderReview(ctx context.Context, target string) error {
	world := worldFrom(ctx)
	if err := h.stepRepoIsEnrolledWithTwoTargetBranches(ctx, acceptanceEnrollmentRepo, "main", target, "main"); err != nil {
		return err
	}
	name := world.claimWorkBranch()
	return h.insertWorkBranchRow(ctx, world.repoID, name, target, "reviewable", world.agentIdentifier())
}

// stepIRemoveFromTargetBranches is "When I remove <branch> from the
// target branches", implemented as a real SetTargetBranches call that
// drops exactly branch from the CURRENT set and leaves indexed_branch
// untouched.
func (h *acceptanceHarness) stepIRemoveFromTargetBranches(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	current, err := h.getRepoAsAdmin(ctx, world.repo())
	if err != nil {
		return err
	}
	remaining := make([]string, 0, len(current.GetTargetBranches()))
	for _, b := range current.GetTargetBranches() {
		if b != branch {
			remaining = append(remaining, b)
		}
	}
	if len(remaining) == len(current.GetTargetBranches()) {
		return fmt.Errorf("branch %q was not among the repo's current target branches %v", branch, current.GetTargetBranches())
	}
	if current.GetIndexedBranch() == branch {
		return fmt.Errorf("test fixture error: %q is the indexed branch and cannot be removed without reassigning indexed_branch too", branch)
	}
	resp, err := h.newRepoAdminServiceClient().SetTargetBranches(ctx, connect.NewRequest(&adminv1.SetTargetBranchesRequest{
		Repo:           world.repo(),
		TargetBranches: remaining,
		IndexedBranch:  current.GetIndexedBranch(),
	}))
	if err != nil {
		return fmt.Errorf("removing target branch %s: %w", branch, err)
	}
	world.lastEnrolledRepo = resp.Msg.GetRepo()
	return nil
}

// stepNoNewWorkBranchesCanStartFrom proves eligibility the REAL way: it
// drives the compiled loam CLI's `work start` against the delisted
// branch, as the scenario's own default author identity (which DOES carry
// work.start -- migration 0001_init's built-in author role -- so a
// rejection here can only be internal/handler/workbranch.CreateWorkBranch's
// own hasTargetBranch check, never a missing capability), and requires the
// CLI's own structured rejection to be exactly the "usage" (InvalidArgument)
// class docs/cli-spec.md maps that check to.
func (h *acceptanceHarness) stepNoNewWorkBranchesCanStartFrom(ctx context.Context, branch string) error {
	world := worldFrom(ctx)
	result := h.runLoamCLI(world, "work", "start", world.repo(), branch)
	return requireLoamRejected(result, fmt.Sprintf("work start from the delisted branch %s", branch), "usage", 2)
}

// stepExistingWorkBranchKeepsLifecycle reads the work branch
// stepAWorkBranchTargetingIsUnderReview seeded back from work_branches
// directly, and requires BOTH its state and its recorded target to be
// exactly what that Given set -- proving SetTargetBranches never touched
// the row at all, not merely that it is still present.
func (h *acceptanceHarness) stepExistingWorkBranchKeepsLifecycle(ctx context.Context) error {
	world := worldFrom(ctx)
	var state, target string
	if err := h.server.pool.QueryRow(ctx, `SELECT state, target FROM work_branches WHERE repo_id = $1 AND name = $2`, world.repoID, world.workBranch).Scan(&state, &target); err != nil {
		return fmt.Errorf("reading back work branch %s: %w", world.workBranch, err)
	}
	if state != "reviewable" {
		return fmt.Errorf("work branch %s state changed to %q, want its original reviewable lifecycle untouched", world.workBranch, state)
	}
	if target != "release" {
		return fmt.Errorf("work branch %s recorded target changed to %q, want its original release untouched", world.workBranch, target)
	}
	return nil
}

// stepItHasNoOpenWorkBranches asserts the fixture's own precondition (zero
// non-terminal work branches for this repo) and ALSO drives a real full
// ingest, so the following "mirror, graph, and vector data are dropped"
// Then has genuine symbols/chunks rows to prove were dropped, rather than
// checking that a query against data that never existed returns nothing.
func (h *acceptanceHarness) stepItHasNoOpenWorkBranches(ctx context.Context) error {
	world := worldFrom(ctx)
	var count int
	if err := h.server.pool.QueryRow(ctx, `SELECT count(*) FROM work_branches WHERE repo_id = $1 AND state NOT IN ('complete', 'closed')`, world.repoID).Scan(&count); err != nil {
		return fmt.Errorf("checking open work branches for %s: %w", world.repo(), err)
	}
	if count != 0 {
		return fmt.Errorf("repo %s has %d open work branch(es); this fixture expected none", world.repo(), count)
	}
	return h.ingestIndexedBranch(ctx, world)
}

// stepIViewTheRepo is "When I view the repo", a plain GetRepo read.
func (h *acceptanceHarness) stepIViewTheRepo(ctx context.Context) error {
	world := worldFrom(ctx)
	repo, err := h.getRepoAsAdmin(ctx, world.repo())
	if err != nil {
		return fmt.Errorf("viewing repo %s: %w", world.repo(), err)
	}
	world.lastEnrolledRepo = repo
	return nil
}

// stepShowsSyncStateAndLastSync requires a REAL (non-UNSPECIFIED) sync
// state and a non-empty last_synced_at -- genuinely populated here because
// the Given behind this scenario used the real EnrollRepo RPC (this file's
// "<repo> is enrolled$" step), whose own clone step always sets both
// before returning.
func (h *acceptanceHarness) stepShowsSyncStateAndLastSync(ctx context.Context) error {
	world := worldFrom(ctx)
	sync := world.lastEnrolledRepo.GetSync()
	if sync.GetState() == adminv1.SyncState_SYNC_STATE_UNSPECIFIED {
		return fmt.Errorf("repo %s reports no sync state at all", world.repo())
	}
	if sync.GetLastSyncedAt() == "" {
		return fmt.Errorf("repo %s reports no time of last successful sync", world.repo())
	}
	return nil
}

// stepIRemoveIt is "When I remove it", the unconditional RemoveRepo call
// scenario "Removing a repo drops its data" expects to succeed.
func (h *acceptanceHarness) stepIRemoveIt(ctx context.Context) error {
	world := worldFrom(ctx)
	if _, err := h.newRepoAdminServiceClient().RemoveRepo(ctx, connect.NewRequest(&adminv1.RemoveRepoRequest{Repo: world.repo()})); err != nil {
		return fmt.Errorf("removing %s: %w", world.repo(), err)
	}
	return nil
}

// stepItIsNoLongerEnrolled asserts GetRepo now answers NotFound -- not
// merely that RemoveRepo itself returned no error.
func (h *acceptanceHarness) stepItIsNoLongerEnrolled(ctx context.Context) error {
	world := worldFrom(ctx)
	_, err := h.getRepoAsAdmin(ctx, world.repo())
	if err == nil {
		return fmt.Errorf("repo %s is still enrolled after removal", world.repo())
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		return fmt.Errorf("expected NotFound reading %s after removal, got %v", world.repo(), err)
	}
	return nil
}

// stepMirrorGraphVectorDataDropped asserts the mirror directory is
// genuinely gone from disk (internal/reporemove.Remover renames it aside
// and deletes it -- a bare os.Stat after a successful RemoveRepo must
// report IsNotExist) and that symbols/chunks -- populated for real by
// stepItHasNoOpenWorkBranches's ingest, never faked -- now report zero
// rows for this repo, proving the cascade actually ran rather than
// checking a table that was always empty.
func (h *acceptanceHarness) stepMirrorGraphVectorDataDropped(ctx context.Context) error {
	world := worldFrom(ctx)
	if _, err := os.Stat(world.mirrorDir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("mirror directory %s still exists (or Stat errored unexpectedly): %v", world.mirrorDir, err)
	}
	var symbolCount, chunkCount int
	if err := h.server.pool.QueryRow(ctx, `SELECT count(*) FROM symbols WHERE repo_id = $1`, world.repoID).Scan(&symbolCount); err != nil {
		return fmt.Errorf("counting symbols for the removed repo: %w", err)
	}
	if err := h.server.pool.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE repo_id = $1`, world.repoID).Scan(&chunkCount); err != nil {
		return fmt.Errorf("counting chunks for the removed repo: %w", err)
	}
	if symbolCount != 0 || chunkCount != 0 {
		return fmt.Errorf("removed repo %s still has %d symbols and %d chunks", world.repo(), symbolCount, chunkCount)
	}
	return nil
}

// stepWorkBranchHistoryIsGone counts work_branches for the removed repo's
// id -- necessarily zero once the repos row itself is gone, via ON DELETE
// CASCADE, but asserted directly rather than assumed.
//
// It is also the last step of this scenario, and the one place world.
// repoID is reset to the zero UUID once the assertion has read it: the
// shared afterScenario hook (acceptance_world_test.go) treats a non-zero
// world.repoID as "this scenario's repos row still needs draining and
// deleting", which is exactly wrong here -- RemoveRepo already deleted it
// for real, so that generic cleanup would fail resolving an already-gone
// repo by name (observed directly while building this suite) and, worse,
// return before reaching its OWN later step that removes the fake-forge
// upstream repo, leaving it behind to collide with the next scenario
// naming the same literal repo. Every earlier Then in this scenario reads
// world.repoID before this point, so none of them are affected by
// clearing it here last.
func (h *acceptanceHarness) stepWorkBranchHistoryIsGone(ctx context.Context) error {
	world := worldFrom(ctx)
	var count int
	if err := h.server.pool.QueryRow(ctx, `SELECT count(*) FROM work_branches WHERE repo_id = $1`, world.repoID).Scan(&count); err != nil {
		return fmt.Errorf("counting work_branches for the removed repo: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("removed repo %s still has %d work_branches row(s)", world.repo(), count)
	}
	world.repoID = uuid.UUID{}
	return nil
}

// stepAWorkBranchOnItIsInState is "And a work branch on it is in state
// <state>", scenario "Removal is blocked by open work branches".
func (h *acceptanceHarness) stepAWorkBranchOnItIsInState(ctx context.Context, state string) error {
	world := worldFrom(ctx)
	name := world.claimWorkBranch()
	return h.insertWorkBranchRow(ctx, world.repoID, name, world.targetBranch, state, world.agentIdentifier())
}

// stepITryToRemoveIt is "When I try to remove it", the REJECTED-outcome
// sibling of stepIRemoveIt: the RPC's outcome is recorded rather than
// asserted here, so the Then steps decide whether it was rejected and how.
func (h *acceptanceHarness) stepITryToRemoveIt(ctx context.Context) error {
	world := worldFrom(ctx)
	_, err := h.newRepoAdminServiceClient().RemoveRepo(ctx, connect.NewRequest(&adminv1.RemoveRepoRequest{Repo: world.repo()}))
	world.lastRPCErr = err
	world.rpcAttempted = true
	return nil
}

// stepRemovalIsRejectedAsFailedPrecondition asserts the RemoveRepo attempt
// was rejected with exactly CodeFailedPrecondition, per repoadmin.RemoveRepo's
// own guard contract.
func (h *acceptanceHarness) stepRemovalIsRejectedAsFailedPrecondition(ctx context.Context) error {
	world := worldFrom(ctx)
	return requireRPCRejected(world.lastRPCErr, "the RemoveRepo attempt", connect.CodeFailedPrecondition)
}

// stepIAmToldWhichWorkBranchesBlockRemoval decodes RemoveRepo's typed
// RemovalBlocked error detail (never just the message string --
// docs/web-spec.md's own requirement, and internal/handler/repoadmin/
// remove_test.go's own mutation-kill for it) and requires it to name
// EXACTLY the one work branch stepAWorkBranchOnItIsInState seeded.
func (h *acceptanceHarness) stepIAmToldWhichWorkBranchesBlockRemoval(ctx context.Context) error {
	world := worldFrom(ctx)
	var connErr *connect.Error
	if !errors.As(world.lastRPCErr, &connErr) {
		return fmt.Errorf("RemoveRepo's rejection did not carry a *connect.Error at all: %v", world.lastRPCErr)
	}
	if len(connErr.Details()) != 1 {
		return fmt.Errorf("want exactly one typed error detail on the rejection, got %d", len(connErr.Details()))
	}
	detailMsg, err := connErr.Details()[0].Value()
	if err != nil {
		return fmt.Errorf("decoding the RemovalBlocked error detail: %w", err)
	}
	blocked, ok := detailMsg.(*adminv1.RemovalBlocked)
	if !ok {
		return fmt.Errorf("error detail decoded as %T, want *adminv1.RemovalBlocked", detailMsg)
	}
	if len(blocked.GetBlockers()) != 1 {
		return fmt.Errorf("want exactly 1 blocking work branch named, got %d: %v", len(blocked.GetBlockers()), blocked.GetBlockers())
	}
	if blocked.GetBlockers()[0].GetName() != world.workBranch {
		return fmt.Errorf("blocker named %q, want %q", blocked.GetBlockers()[0].GetName(), world.workBranch)
	}
	return nil
}
