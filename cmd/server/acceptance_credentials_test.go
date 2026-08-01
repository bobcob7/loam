//go:build acceptance

// Step definitions for features/credentials.feature (loam-317m):
// CredentialService's SetUpstreamToken/GetCredentialStatus, plus the one
// leg of RepoAdminService.EnrollRepo (internal/handler/repoadmin's
// upstreamChecker.CheckRepo) that proves a stored token actually works
// over BOTH channels a real forge token has -- REST (opening PRs) and git
// (clone/fetch/push) -- rather than only the REST side SetUpstreamToken
// itself already exercises.
//
// # Why this file builds its own identity-preserving proxy
//
// Every scenario here names a literal host or URL this suite can never
// reach ("github.com", "https://github.com/bobcob7/doc-server",
// "forgejo.example.com"). acceptance_enrollment_test.go solves the exact
// same problem for features/enrollment.feature with a private
// httptest.Server that fronts the shared fakeforge instance and re-injects
// its fixed "/git" mount segment on the way in (newEnrollmentIdentityProxy)
// -- every step below that needs a genuinely reachable stand-in for a
// stated host goes through a SECOND, independent instance of that same
// design (newCredentialsIdentityProxy), never the enrollment file's own
// proxy: this file has to reset its own request-hit counters between
// steps, and sharing a proxy would let enrollment.feature's unrelated
// traffic (a different TestFeatures run never overlaps these two files,
// but nothing enforces that a future scenario in this file couldn't) pollute
// them.
//
// world.credentialHostFor (acceptance_world_test.go) records, per scenario,
// which stated literal host names actually got mapped to that proxy: only
// steps that stand up a WORKING credential populate an entry ("a credential
// exists for ...", "I set an upstream token for ...", "I enroll two repos
// hosted on ..."). "Credentials are scoped per host"'s second host
// ("forgejo.example.com") is deliberately never mapped: it must be used
// exactly as written, unreachable and all, because a pure GetCredentialStatus
// read never dials it -- the entire point of that scenario is that this
// host was NEVER configured, so credentialHost falling back to the literal
// stated string when no mapping exists is what lets that scenario prove
// scoping instead of accidentally validating reachability.
//
// # Why REST and git evidence is a request-hit count, not just an outcome
//
// "One token covers REST and git" (loam-317m's own NOTES) would pass
// vacuously if it only checked that SetUpstreamToken and EnrollRepo both
// returned success: this credential DOES have git write access, so
// CheckRepo's write probe silently short-circuited would still leave
// enrollment succeeding by way of the clone that follows. credentialsProxyHits
// records which upstream request shapes actually reached the fake forge
// through this file's own proxy, so the Then step can require BOTH a REST
// pulls-probe request (ValidateToken, during the Given) AND a real
// info/refs?service=git-receive-pack request (CheckRepo's write probe,
// during the enroll) to have happened, not merely infer them from a
// success return.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/cucumber/godog"

	"github.com/bobcob7/loam/internal/fakeforge"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
)

// The "A token without git access fails enrollment" scenario that used to
// live here (and the acceptanceReadOnlyForgeToken const + AddReadOnlyToken
// registration it needed) was removed by loam-2uy: it relied on
// fakeforge.AddReadOnlyToken producing a token that validates fine (REST
// PR-opening scope intact) but is denied at git push, and loam-2uy's live
// verification against Forgejo 9.0.3 found that state unreachable -- git
// push and the REST scope probe SetUpstreamToken's ValidateToken uses are
// gated on the identical write:repository scope, so a token that clears
// SetUpstreamToken can never then fail EnrollRepo's CheckRepo for a scope
// reason. The generic "CheckRepo fails -> CodeFailedPrecondition" path this
// scenario exercised is still covered without a real (or fake) forge at
// all, via a mocked checker, in
// internal/handler/repoadmin/enroll_test.go's
// TestEnrollRepo_CheckRepoFails_FailedPreconditionAndNoClone.

// acceptanceCredSharedGroup/RepoA/RepoB name the two repos "Credentials are
// shared by all repos on a host" enrolls. The Gherkin names no repos at
// all (only "two repos hosted on <host>"), unlike the enroll scenarios that
// reuse enrollment.feature's own literal "bobcob7/doc-server" -- this
// scenario is not about IDENTIFYING these repos, only about there being
// two of them sharing one host's credential, so distinct, obviously
// scoped-to-this-feature names are used instead of borrowing the shared
// literal.
const (
	acceptanceCredSharedGroup = "bobcob7"
	acceptanceCredSharedRepoA = "cred-shared-a"
	acceptanceCredSharedRepoB = "cred-shared-b"
)

// credentialsProxyHits records which upstream request shapes this
// scenario's SetUpstreamToken/EnrollRepo calls actually produced against
// the shared fake forge, keyed by the same request-shape predicates
// internal/fakeforge/git.go's own isReceivePackRequest and
// internal/forge/forgejo.go's ValidateToken probe use: a POST to
// .../pulls (the REST scope probe), and info/refs with
// service=git-upload-pack (git read) or service=git-receive-pack (git
// write). See this file's own doc comment for why this exists instead of
// inferring channel use from RPC success alone.
type credentialsProxyHits struct {
	mu          sync.Mutex
	restPulls   int
	uploadPack  int
	receivePack int
}

// record classifies one proxied request. Requests matching none of the
// three shapes (the provider REST surface's other routes, the control
// API) are silently ignored -- this file only ever needs to observe these
// three.
func (h *credentialsProxyHits) record(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.URL.Query().Get("service") == "git-receive-pack", strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		h.receivePack++
	case r.URL.Query().Get("service") == "git-upload-pack", strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		h.uploadPack++
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
		h.restPulls++
	}
}

// reset zeroes every counter, called at the start of every Given/When step
// in this file that is about to make a fresh, scenario-scoped upstream
// call -- so a later Then step's hit counts describe THIS scenario's own
// traffic, never a previous scenario's leftover count against this file's
// long-lived, whole-suite proxy instance.
func (h *credentialsProxyHits) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.restPulls, h.uploadPack, h.receivePack = 0, 0, 0
}

// snapshot returns the three counters' current values.
func (h *credentialsProxyHits) snapshot() (restPulls, uploadPack, receivePack int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.restPulls, h.uploadPack, h.receivePack
}

// newCredentialsIdentityProxy builds this file's own private
// httptest.Server, structurally identical to acceptance_enrollment_test.go's
// newEnrollmentIdentityProxy (re-injecting fakeforge's fixed "/git" mount
// segment for any path that is not one of its REST/control mounts) but
// additionally recording every request into hits before forwarding it.
func newCredentialsIdentityProxy(forge *fakeforge.Server, hits *credentialsProxyHits) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.record(r)
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/provider/") && !strings.HasPrefix(r.URL.Path, "/control/") {
			r.URL.Path = "/git" + r.URL.Path
		}
		forge.ServeHTTP(w, r)
	}))
}

// newCredentialServiceClient builds the Admin actor's connect-go client for
// loam.admin.v1.CredentialService.
func (h *acceptanceHarness) newCredentialServiceClient() adminv1connect.CredentialServiceClient {
	return adminv1connect.NewCredentialServiceClient(h.adminHTTPClient(), h.server.baseURL)
}

// registerCredentialsSteps wires every step features/credentials.feature
// needs. Like registerEnrollmentSteps, it builds this file's one
// suite-lifetime proxy, since this function itself runs exactly once
// (initializeScenario's own doc comment).
func (h *acceptanceHarness) registerCredentialsSteps(sc *godog.ScenarioContext) {
	hits := &credentialsProxyHits{}
	proxy := newCredentialsIdentityProxy(h.forge, hits)
	h.t.Cleanup(proxy.Close)

	sc.Step(`^I set an upstream token for "([^"]*)"$`, func(ctx context.Context, statedHost string) error {
		return h.stepISetUpstreamTokenFor(ctx, proxy.URL, statedHost)
	})
	sc.Step(`^the server validates the token$`, h.stepTheServerValidatesTheToken)
	sc.Step(`^the credential status for "([^"]*)" shows a token is present$`, h.stepTheCredentialStatusShowsTokenPresent)
	sc.Step(`^I set an invalid upstream token for "([^"]*)"$`, func(ctx context.Context, statedHost string) error {
		return h.stepISetInvalidUpstreamTokenFor(ctx, proxy.URL, statedHost)
	})
	sc.Step(`^the credential is rejected as invalid$`, h.stepTheCredentialIsRejectedAsInvalid)
	sc.Step(`^a credential exists for "([^"]*)"$`, func(ctx context.Context, statedHost string) error {
		return h.stepACredentialExistsFor(ctx, proxy.URL, hits, statedHost, acceptanceForgeToken)
	})
	sc.Step(`^I enroll "([^"]*)"$`, func(ctx context.Context, literalUpstreamURL string) error {
		return h.stepICredEnroll(ctx, proxy.URL, literalUpstreamURL)
	})
	sc.Step(`^the server proves git read and write access with the token before cloning$`, func(ctx context.Context) error {
		return h.stepServerProvesGitReadWriteAccess(ctx, hits)
	})
	sc.Step(`^I enroll two repos hosted on "([^"]*)"$`, func(ctx context.Context, statedHost string) error {
		return h.stepIEnrollTwoReposHostedOn(ctx, proxy.URL, statedHost)
	})
	sc.Step(`^both use the "([^"]*)" credential$`, h.stepBothUseTheCredential)
	sc.Step(`^I view the credential status for "([^"]*)"$`, h.stepIViewCredentialStatusFor)
	sc.Step(`^it shows no credential is present$`, h.stepItShowsNoCredentialIsPresent)
}

// getCredentialStatus reads host's status through the real, admin-facing
// CredentialService.GetCredentialStatus RPC.
func (h *acceptanceHarness) getCredentialStatus(ctx context.Context, host string) (*adminv1.CredentialStatus, error) {
	resp, err := h.newCredentialServiceClient().GetCredentialStatus(ctx, connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: host}))
	if err != nil {
		return nil, fmt.Errorf("getting credential status for %s: %w", host, err)
	}
	return resp.Msg.GetStatus(), nil
}

// stepISetUpstreamTokenFor is "When I set an upstream token for <host>"
// (scenario "Setting a token for a forge host"), driven as a real
// SetUpstreamToken call with a genuinely working token against proxyURL --
// the reachable stand-in for statedHost -- so the following Then steps
// observe production's real validate-then-store sequence, not a shortcut
// around it.
func (h *acceptanceHarness) stepISetUpstreamTokenFor(ctx context.Context, proxyURL, statedHost string) error {
	world := worldFrom(ctx)
	resp, err := h.newCredentialServiceClient().SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: proxyURL, Token: acceptanceForgeToken}))
	world.lastCredentialErr = err
	world.rpcAttempted = true
	world.lastCredentialHost = proxyURL
	if err != nil {
		return nil
	}
	world.lastCredentialStatus = resp.Msg.GetStatus()
	world.rememberCredentialHost(statedHost, proxyURL)
	return nil
}

// stepTheServerValidatesTheToken asserts the SetUpstreamToken call from
// stepISetUpstreamTokenFor both succeeded and reports validated=true --
// not merely that no error was returned, since a handler that stored the
// token without ever calling the forge's ValidateToken would also satisfy
// a bare "no error" check.
func (h *acceptanceHarness) stepTheServerValidatesTheToken(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastCredentialErr != nil {
		return fmt.Errorf("setting the upstream token failed: %w", world.lastCredentialErr)
	}
	if world.lastCredentialStatus == nil || !world.lastCredentialStatus.GetValidated() {
		return fmt.Errorf("SetUpstreamToken response reports validated=%v, want true", world.lastCredentialStatus.GetValidated())
	}
	return nil
}

// stepTheCredentialStatusShowsTokenPresent is "Then the credential status
// for <host> shows a token is present", read back through a FRESH
// GetCredentialStatus call (never off the SetUpstreamToken response
// itself), so this proves the token was actually persisted, not merely
// echoed back in the write response.
func (h *acceptanceHarness) stepTheCredentialStatusShowsTokenPresent(ctx context.Context, statedHost string) error {
	world := worldFrom(ctx)
	host := world.credentialHost(statedHost)
	status, err := h.getCredentialStatus(ctx, host)
	if err != nil {
		return err
	}
	if !status.GetHasToken() {
		return fmt.Errorf("credential status for %s reports has_token=false, want true", statedHost)
	}
	return nil
}

// stepISetInvalidUpstreamTokenFor is "When I set an invalid upstream token
// for <host>" (scenario "A rejected token is reported"): the submitted
// token is a fresh, per-scenario literal never registered with the shared
// fake forge (h.forge.AddToken et al.), so the forge's own scope probe
// genuinely rejects it with 401 rather than this step faking a rejection.
// world.lastSubmittedToken records the exact literal, for
// stepTheCredentialIsRejectedAsInvalid's leak check.
func (h *acceptanceHarness) stepISetInvalidUpstreamTokenFor(ctx context.Context, proxyURL, statedHost string) error {
	world := worldFrom(ctx)
	token := "loam-acceptance-invalid-token-" + world.repoName
	_, err := h.newCredentialServiceClient().SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: proxyURL, Token: token}))
	world.lastCredentialErr = err
	world.lastSubmittedToken = token
	world.lastCredentialHost = proxyURL
	world.rpcAttempted = true
	world.rememberCredentialHost(statedHost, proxyURL)
	return nil
}

// stepTheCredentialIsRejectedAsInvalid asserts three things a naive
// implementation could each fail independently: the RPC was rejected with
// exactly CodeInvalidArgument (internal/handler/credential's
// validateToken maps forge.ErrInvalidToken there, distinct from
// CodeFailedPrecondition's underscoped case -- loam-317m's own "assert on
// the specific failure" warning), the rejection's error message never
// echoes the submitted token verbatim (the trap this scenario exists to
// catch), and the token was never persisted (a fresh GetCredentialStatus
// read reports has_token=false).
func (h *acceptanceHarness) stepTheCredentialIsRejectedAsInvalid(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := requireRPCRejected(world.lastCredentialErr, "the SetUpstreamToken attempt", connect.CodeInvalidArgument); err != nil {
		return err
	}
	message := world.lastCredentialErr.Error()
	if world.lastSubmittedToken != "" && strings.Contains(message, world.lastSubmittedToken) {
		return fmt.Errorf("the rejection error leaked the submitted token verbatim: %q", message)
	}
	status, err := h.getCredentialStatus(ctx, world.lastCredentialHost)
	if err != nil {
		return err
	}
	if status.GetHasToken() {
		return fmt.Errorf("a rejected token must never be persisted, but credential status for %s reports has_token=true", world.lastCredentialHost)
	}
	return nil
}

// stepACredentialExistsFor is the shared Given behind "a credential exists
// for <host>". Driven through the real SetUpstreamToken RPC, exactly like
// enrollment.feature's own analogous Background step, so this also proves
// the REST validation round trip genuinely ran with this token -- the REST
// half of "One token covers REST and git". hits.reset() gives the following
// steps a clean baseline attributable to this scenario alone.
func (h *acceptanceHarness) stepACredentialExistsFor(ctx context.Context, proxyURL string, hits *credentialsProxyHits, statedHost, token string) error {
	hits.reset()
	world := worldFrom(ctx)
	resp, err := h.newCredentialServiceClient().SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: proxyURL, Token: token}))
	if err != nil {
		return fmt.Errorf("seeding a credential for stated host %s (actual host %s): %w", statedHost, proxyURL, err)
	}
	if !resp.Msg.GetStatus().GetHasToken() {
		return fmt.Errorf("SetUpstreamToken for %s reported has_token=false immediately after a successful call", proxyURL)
	}
	world.rememberCredentialHost(statedHost, proxyURL)
	world.lastCredentialHost = proxyURL
	return nil
}

// stepICredEnroll is "When I enroll <url>" (scenario "One token covers REST
// and git"), recording the outcome on world rather than judging it here --
// like this package's other "When I try to ..." steps (e.g.
// stepITryToDesignateAsIndexedBranch) -- so the Then step decides pass or
// fail. enrollmentPathIdentifier (acceptance_enrollment_test.go) derives
// the "<group>/<repo_name>" identifier from literalUpstreamURL's own path,
// discarding its unreachable domain, exactly as that file's own
// stepIEnroll does -- here that identifier is literally
// "bobcob7/doc-server", the same literal enrollment.feature itself uses,
// which is safe to reuse here for the same reason that file's own doc
// comment gives: every scenario naming it cleans it up in afterScenario
// before the next one runs.
func (h *acceptanceHarness) stepICredEnroll(ctx context.Context, proxyURL, literalUpstreamURL string) error {
	world := worldFrom(ctx)
	repoIdentifier, err := enrollmentPathIdentifier(literalUpstreamURL)
	if err != nil {
		return err
	}
	world.lastRPCErr = h.enrollRepoForReal(ctx, world, proxyURL, repoIdentifier, "main")
	world.rpcAttempted = true
	return nil
}

// stepServerProvesGitReadWriteAccess is "Then the server proves git read
// and write access with the token before cloning" (scenario "One token
// covers REST and git"). It requires FOUR independent things, each of
// which a narrower check could miss: the enroll itself succeeded, a real
// bare mirror with the target branch's tip landed on disk (proving a
// clone genuinely happened, not just a database row), and -- the load-
// bearing checks per this file's own doc comment -- a REST pulls-probe
// request reached the fake forge (from the Given's SetUpstreamToken) AND a
// genuine git-receive-pack advertisement request reached it too (from
// CheckRepo's write probe during THIS enroll). The last two are what stop
// this scenario from passing vacuously if CheckRepo's write probe, or
// SetUpstreamToken's REST validation, were silently skipped while the
// surrounding RPCs still returned success.
func (h *acceptanceHarness) stepServerProvesGitReadWriteAccess(ctx context.Context, hits *credentialsProxyHits) error {
	world := worldFrom(ctx)
	if world.lastRPCErr != nil {
		return fmt.Errorf("enrolling failed, but this scenario expects the token's git read+write access to be proven and enrollment to succeed: %w", world.lastRPCErr)
	}
	if world.lastEnrolledRepo.GetSync().GetState() != adminv1.SyncState_SYNC_STATE_IDLE {
		return fmt.Errorf("enrolled repo's sync state is %s, want SYNC_STATE_IDLE", world.lastEnrolledRepo.GetSync().GetState())
	}
	tip, err := mirrorRefSHA(world.mirrorDir, "refs/heads/main")
	if err != nil {
		return fmt.Errorf("the mirror for %s has no refs/heads/main ref on disk: %w", world.repo(), err)
	}
	if tip == "" {
		return fmt.Errorf("the mirror for %s reports an empty SHA for refs/heads/main", world.repo())
	}
	restPulls, uploadPack, receivePack := hits.snapshot()
	if restPulls == 0 {
		return fmt.Errorf("no REST pulls-probe request ever reached the upstream; the REST channel was never actually exercised with this token")
	}
	if uploadPack == 0 {
		return fmt.Errorf("no git read (upload-pack) request ever reached the upstream; the git channel was never actually exercised with this token")
	}
	if receivePack == 0 {
		return fmt.Errorf("no git write (receive-pack) probe request ever reached the upstream; git write access was never actually proven with this token")
	}
	return nil
}

// stepIEnrollTwoReposHostedOn is "When I enroll two repos hosted on
// <host>" (scenario "Credentials are shared by all repos on a host"). The
// FIRST repo is enrolled onto world itself (so afterScenario's generic
// per-repo cleanup handles it), the SECOND onto a fresh, independent
// *acceptanceWorld recorded as world.secondRepo (so
// afterScenario's teardownSecondRepo handles it) -- the same split
// acceptance_ingest_test.go's ensureSecondEnrolledRepo already
// establishes for a two-repo scenario. Both go through the SAME proxyURL,
// and therefore resolve credentials under the SAME forge_host string; only
// one credential was ever set (the Given), so both succeeding is itself
// the proof this scenario is about.
func (h *acceptanceHarness) stepIEnrollTwoReposHostedOn(ctx context.Context, proxyURL, statedHost string) error {
	world := worldFrom(ctx)
	repoA := fmt.Sprintf("%s/%s-%s", acceptanceCredSharedGroup, acceptanceCredSharedRepoA, world.repoName)
	repoB := fmt.Sprintf("%s/%s-%s", acceptanceCredSharedGroup, acceptanceCredSharedRepoB, world.repoName)
	if err := h.enrollRepoForReal(ctx, world, proxyURL, repoA, "main"); err != nil {
		return fmt.Errorf("enrolling the first repo %s hosted on %s: %w", repoA, statedHost, err)
	}
	second := &acceptanceWorld{}
	if err := h.enrollRepoForReal(ctx, second, proxyURL, repoB, "main"); err != nil {
		return fmt.Errorf("enrolling the second repo %s hosted on %s: %w", repoB, statedHost, err)
	}
	world.secondRepo = second
	return nil
}

// stepBothUseTheCredential is "Then both use the <host> credential":
// both enrollments from stepIEnrollTwoReposHostedOn reached
// SYNC_STATE_IDLE, which EnrollRepo only reports after a successful
// GetByHost credential resolution AND a passing CheckRepo -- for the
// SECOND repo in particular, since no second credential was ever set,
// that is only possible if the one credential the Given step wrote is
// genuinely resolved by HOST, shared across both repos, rather than
// scoped to whichever repo first resolved it.
func (h *acceptanceHarness) stepBothUseTheCredential(ctx context.Context, statedHost string) error {
	world := worldFrom(ctx)
	if world.lastEnrolledRepo.GetSync().GetState() != adminv1.SyncState_SYNC_STATE_IDLE {
		return fmt.Errorf("the first repo %s did not finish enrolling (sync state %s), so it cannot be shown to share the %s credential", world.repo(), world.lastEnrolledRepo.GetSync().GetState(), statedHost)
	}
	if world.secondRepo == nil || world.secondRepo.lastEnrolledRepo == nil {
		return fmt.Errorf("no second repo was enrolled in this scenario")
	}
	if world.secondRepo.lastEnrolledRepo.GetSync().GetState() != adminv1.SyncState_SYNC_STATE_IDLE {
		return fmt.Errorf("the second repo %s did not finish enrolling (sync state %s): without a SECOND credential ever being set, this proves the %s credential was not actually shared", world.secondRepo.repo(), world.secondRepo.lastEnrolledRepo.GetSync().GetState(), statedHost)
	}
	return nil
}

// stepIViewCredentialStatusFor is "When I view the credential status for
// <host>" (scenario "Credentials are scoped per host"). statedHost here is
// deliberately resolved through world.credentialHost, which falls back to
// the literal string when (as here) it was never mapped by an earlier
// Given/When -- this host must stay genuinely unconfigured, not be
// silently redirected to the working proxy the OTHER host in this same
// scenario ("github.com") was mapped to.
func (h *acceptanceHarness) stepIViewCredentialStatusFor(ctx context.Context, statedHost string) error {
	world := worldFrom(ctx)
	host := world.credentialHost(statedHost)
	status, err := h.getCredentialStatus(ctx, host)
	if err != nil {
		return err
	}
	world.lastCredentialStatus = status
	world.lastCredentialHost = host
	return nil
}

// stepItShowsNoCredentialIsPresent is "Then it shows no credential is
// present": the decisive assertion for "Credentials are scoped per host"
// (loam-317m's own NOTES) -- a lookup that ignored host entirely would
// find the OTHER host's row this same scenario's Given just wrote and
// report has_token=true here instead.
func (h *acceptanceHarness) stepItShowsNoCredentialIsPresent(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastCredentialStatus == nil {
		return fmt.Errorf("no credential status was read yet")
	}
	if world.lastCredentialStatus.GetHasToken() {
		return fmt.Errorf("credential status for %s reports has_token=true, want false (no credential was ever configured for this host)", world.lastCredentialHost)
	}
	return nil
}
