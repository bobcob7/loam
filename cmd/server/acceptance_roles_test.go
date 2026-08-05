//go:build acceptance

// Step definitions for features/roles.feature's "A role's instructions
// reach its agents" (loam-ofg.11). The scenario drives two actors: the
// Admin actor (loam.admin.v1.RoleService, over a basic-auth connect-go
// client -- see acceptance_proposal_test.go's adminHTTPClient) configures
// a built-in role's instructions text, and an agent actor (the compiled
// loam CLI, over runLoamAs -- see acceptance_review_test.go) asks
// loam.v1.MetaService.GetInstructions for its orientation.
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/cucumber/godog"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/meta"
	"github.com/bobcob7/loam/internal/refnames"
)

// registerRoleSteps wires every step features/roles.feature's "A role's
// instructions reach its agents" scenario needs. The scenario's Background
// ("I am signed in to the web interface as the admin", "the repo ... is
// enrolled ...") is already registered by registerSyncSteps and
// registerCloneAndPushSteps respectively.
func (h *acceptanceHarness) registerRoleSteps(sc *godog.ScenarioContext) {
	sc.Step(`^the "([^"]*)" role has instructions configured$`, h.stepTheRoleHasInstructionsConfigured)
	sc.Step(`^a "([^"]*)" agent asks for its instructions$`, h.stepAnAgentAsksForItsInstructions)
	sc.Step(`^it receives the reviewer instructions$`, h.stepItReceivesTheReviewerInstructions)
	sc.Step(`^only the commands its role permits$`, h.stepOnlyTheCommandsItsRolePermits)
	sc.Step(`^I list roles$`, h.stepIListRoles)
	sc.Step(`^a built-in "([^"]*)" role and a built-in "([^"]*)" role exist$`, h.stepBuiltinRolesExist)
	sc.Step(`^the built-in "([^"]*)" role grants only "([^"]*)" and "([^"]*)"$`, h.stepTheBuiltinRoleGrantsOnly)
	sc.Step(`^it holds no work-branch capability$`, h.stepItHoldsNoWorkBranchCapability)
	sc.Step(`^I am an agent with the "([^"]*)" role$`, h.stepIAmAnAgentWithTheRole)
	sc.Step(`^I try to submit a verdict$`, h.stepITryToSubmitAVerdict)
	sc.Step(`^the operation is denied$`, h.stepTheOperationIsDenied)
	sc.Step(`^I try to start a work branch$`, h.stepITryToStartAWorkBranch)
	sc.Step(`^I try to push$`, h.stepITryToPushAsCurrentActor)
	sc.Step(`^I clone "([^"]*)"$`, h.stepICloneAsCurrentActor)
	sc.Step(`^the clone succeeds$`, h.stepTheCloneSucceeds)
	sc.Step(`^a custom role "([^"]*)" without the push operation$`, h.stepACustomRoleWithoutPush)
	sc.Step(`^I grant it the push operation$`, h.stepIGrantItThePushOperation)
	sc.Step(`^agents with that role may push$`, h.stepAgentsWithThatRoleMayPush)
	sc.Step(`^I try to delete the built-in "([^"]*)" role$`, h.stepITryToDeleteTheBuiltinRole)
	sc.Step(`^the deletion is rejected$`, h.stepTheDeletionIsRejected)
	sc.Step(`^an agent presenting the role "([^"]*)" in its environment$`, h.stepAnAgentPresentingTheRoleInItsEnvironment)
	sc.Step(`^the server treats it as a reviewer without further authentication$`, h.stepTheServerTreatsItAsThatRoleWithoutFurtherAuthentication)
}

// newRoleServiceClient builds the Admin actor's connect-go client for
// loam.admin.v1.RoleService, mirroring acceptance_proposal_test.go's own
// per-service client constructors.
func (h *acceptanceHarness) newRoleServiceClient() adminv1connect.RoleServiceClient {
	return adminv1connect.NewRoleServiceClient(h.adminHTTPClient(), h.server.baseURL)
}

// acceptanceInstructionsCommand and acceptanceInstructionsOutput mirror
// internal/cli/commands_root.go's instructionsCommandOutput/
// instructionsOutput -- `loam instructions`' JSON shape -- reproduced here
// rather than imported, since both types are unexported.
type acceptanceInstructionsCommand struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

type acceptanceInstructionsOutput struct {
	Usage            string                          `json:"usage"`
	Commands         []acceptanceInstructionsCommand `json:"commands"`
	RoleInstructions string                          `json:"role_instructions"`
}

// acceptanceCommandCapability is the pairing of each GATED CLI command to
// the single capability that gates it (docs/cli-spec.md's Commands
// table), used to predict which commands a role's `loam instructions`
// response should list.
//
// This used to be a hand-maintained literal map, reproduced here because
// internal/handler/meta/catalog.go's own table was unexported. loam-hi5o.4's
// review round found that out the hard way: this file still said
// `"graph": handler.CapabilityGraphQuery` after catalog.go split "graph"
// into five separate commands, so `wantPresent` and `present` disagreed on
// "graph" in four scenarios, and no build-time check caught it -- the
// exact drift trap the bead's own cross-package test
// (internal/cli/synopsis_test.go) was written to avoid, one package over
// and unguarded.
//
// meta.AllCommands() is exported precisely so this can be DISCOVERED
// instead of restated: every entry with a non-empty Capability, keyed by
// name. Ungated entries (Capability == "") are excluded -- every step
// below already checks instructions/whoami as "always present regardless
// of role" on its own, separately from this map. Building it from
// meta.AllCommands() at package-var-init time (not a func init(), just a
// plain function call in a var initializer) means a future catalog rename
// or split, like the one that broke this, updates this map automatically
// with no second edit required.
var acceptanceCommandCapability = buildAcceptanceCommandCapability()

// buildAcceptanceCommandCapability does the discovery acceptanceCommandCapability's
// doc comment describes.
func buildAcceptanceCommandCapability() map[string]handler.Capability {
	out := make(map[string]handler.Capability)
	for _, entry := range meta.AllCommands() {
		if entry.Capability == "" {
			continue
		}
		out[entry.Name] = entry.Capability
	}
	return out
}

// stepTheRoleHasInstructionsConfigured sets role's instructions text
// through the real admin RoleService.UpdateRole -- the one surface that
// writes roles.instructions (internal/handler/role.go's UpdateRole doc:
// built-ins ship with instructions set to the empty string, and updating
// them is deliberately allowed for exactly this reason). It reads the
// role's CURRENT operations first and round-trips them unchanged, so this
// step configures instructions without altering what the role may do --
// which "only the commands its role permits" later depends on still
// reflecting the built-in's real, migration-seeded grant.
func (h *acceptanceHarness) stepTheRoleHasInstructionsConfigured(ctx context.Context, role string) error {
	world := worldFrom(ctx)
	client := h.newRoleServiceClient()
	getResp, err := client.GetRole(ctx, connect.NewRequest(&adminv1.GetRoleRequest{Name: role}))
	if err != nil {
		return fmt.Errorf("reading role %s before configuring its instructions: %w", role, err)
	}
	instructions := fmt.Sprintf("acceptance: %s role instructions", role)
	_, err = client.UpdateRole(ctx, connect.NewRequest(&adminv1.UpdateRoleRequest{Role: &adminv1.Role{
		Name:         role,
		Operations:   getResp.Msg.GetRole().GetOperations(),
		Instructions: instructions,
	}}))
	if err != nil {
		return fmt.Errorf("configuring instructions for role %s: %w", role, err)
	}
	world.configuredRoleInstructions = instructions
	return nil
}

// stepAnAgentAsksForItsInstructions runs `loam instructions` as a fresh
// agent identity presenting role, and decodes its JSON response into
// world.lastInstructions for the following Then steps. The agent has no
// prior identity in this scenario (the Background never names one), so
// one is fabricated here -- exactly what "a 'reviewer' agent" (an
// indefinite article, not a named actor) calls for.
func (h *acceptanceHarness) stepAnAgentAsksForItsInstructions(ctx context.Context, role string) error {
	world := worldFrom(ctx)
	actor := acceptanceActor{name: "acceptance-meta-agent", id: world.agentID, role: role}
	res := h.runLoamAs(world, actor, "", "instructions")
	var out acceptanceInstructionsOutput
	if err := decodeLoamJSON(res, "instructions", &out); err != nil {
		return err
	}
	world.lastInstructions = out
	return nil
}

// stepItReceivesTheReviewerInstructions asserts the CLI's role_instructions
// field is byte-identical to the text stepTheRoleHasInstructionsConfigured
// configured earlier in this scenario -- not merely non-empty, so a
// handler bug that returned some OTHER role's instructions, or the static
// usage text, would fail this rather than pass on a vacuous "it received
// something" check.
func (h *acceptanceHarness) stepItReceivesTheReviewerInstructions(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.configuredRoleInstructions == "" {
		return fmt.Errorf("no role instructions were configured earlier in this scenario")
	}
	if world.lastInstructions.RoleInstructions != world.configuredRoleInstructions {
		return fmt.Errorf("got role_instructions %q, want %q", world.lastInstructions.RoleInstructions, world.configuredRoleInstructions)
	}
	return nil
}

// stepOnlyTheCommandsItsRolePermits asserts the CLI's command list is
// EXACTLY what the reviewer role's real, currently-granted operations
// predict: every ungated command present, and every gated command present
// if and only if its capability is in the role's live GetRole response.
// The expected set comes from a fresh GetRole call, not from a literal
// list of what migration 0001_init seeds, so this step tracks the role's
// actual server-side grant rather than an assumption frozen at write time.
func (h *acceptanceHarness) stepOnlyTheCommandsItsRolePermits(ctx context.Context) error {
	world := worldFrom(ctx)
	getResp, err := h.newRoleServiceClient().GetRole(ctx, connect.NewRequest(&adminv1.GetRoleRequest{Name: "reviewer"}))
	if err != nil {
		return fmt.Errorf("reading the reviewer role's granted operations: %w", err)
	}
	granted := make(map[string]bool, len(getResp.Msg.GetRole().GetOperations()))
	for _, op := range getResp.Msg.GetRole().GetOperations() {
		granted[op] = true
	}
	present := make(map[string]bool, len(world.lastInstructions.Commands))
	for _, c := range world.lastInstructions.Commands {
		present[c.Name] = true
	}
	if !present["instructions"] || !present["whoami"] {
		return fmt.Errorf("instructions/whoami must always be present regardless of role, got %v", world.lastInstructions.Commands)
	}
	for name, capability := range acceptanceCommandCapability {
		wantPresent := granted[string(capability)]
		if present[name] != wantPresent {
			return fmt.Errorf("command %q present=%v, but the reviewer role's grant of capability %q is %v", name, present[name], capability, wantPresent)
		}
	}
	return nil
}

// --- "Built-in roles ship with sane defaults" ---

// stepIListRoles runs the Admin actor's RoleService.ListRoles for real,
// recording the response for the following Then to check.
func (h *acceptanceHarness) stepIListRoles(ctx context.Context) error {
	world := worldFrom(ctx)
	resp, err := h.newRoleServiceClient().ListRoles(ctx, connect.NewRequest(&adminv1.ListRolesRequest{}))
	if err != nil {
		return fmt.Errorf("listing roles: %w", err)
	}
	world.lastRoles = resp.Msg.GetRoles()
	return nil
}

// findRoleByName returns the role named name from roles, or nil.
func findRoleByName(roles []*adminv1.Role, name string) *adminv1.Role {
	for _, role := range roles {
		if role.GetName() == name {
			return role
		}
	}
	return nil
}

// stepBuiltinRolesExist asserts both migration 0001_init's seeded roles are
// present in the live ListRoles response AND still carry builtin=true --
// not merely that two roles with these names exist, which an admin could
// have created fresh (CreateRole explicitly refuses builtin=true in the
// request, so only migration 0001_init can ever produce a true one; see
// internal/handler/role.CreateRole's own doc comment).
func (h *acceptanceHarness) stepBuiltinRolesExist(ctx context.Context, first, second string) error {
	world := worldFrom(ctx)
	for _, name := range []string{first, second} {
		role := findRoleByName(world.lastRoles, name)
		if role == nil {
			return fmt.Errorf("no role named %q in the ListRoles response (%d roles returned)", name, len(world.lastRoles))
		}
		if !role.GetBuiltin() {
			return fmt.Errorf("role %q was returned but is not marked builtin", name)
		}
	}
	return nil
}

// --- "The orchestrator role supervises but cannot act" ---

// stepTheBuiltinRoleGrantsOnly asserts the named role is built-in and its
// granted operations are EXACTLY the two the scenario names -- set
// equality, not containment, so a seed that granted a third would fail here
// (loam-hi5o.31). It reads the live ListRoles response the previous When
// captured, so this is what an admin would actually see in the console.
func (h *acceptanceHarness) stepTheBuiltinRoleGrantsOnly(ctx context.Context, name, first, second string) error {
	world := worldFrom(ctx)
	role := findRoleByName(world.lastRoles, name)
	if role == nil {
		return fmt.Errorf("no role named %q in the ListRoles response (%d roles returned)", name, len(world.lastRoles))
	}
	if !role.GetBuiltin() {
		return fmt.Errorf("role %q was returned but is not marked builtin, so it could be deleted", name)
	}
	got := append([]string(nil), role.GetOperations()...)
	want := []string{first, second}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return fmt.Errorf("role %q grants %v, want exactly %v", name, got, want)
	}
	return nil
}

// stepItHoldsNoWorkBranchCapability is the same claim from the other
// direction, and it exists because the exact-set check above would still
// pass if the vocabulary itself were renamed underneath it. This one names
// the forbidden capabilities individually, so a failure says WHICH one
// leaked in. The orchestrator supervises; the agents act.
func (h *acceptanceHarness) stepItHoldsNoWorkBranchCapability(ctx context.Context) error {
	world := worldFrom(ctx)
	role := findRoleByName(world.lastRoles, "orchestrator")
	if role == nil {
		return fmt.Errorf("no orchestrator role in the ListRoles response (%d roles returned)", len(world.lastRoles))
	}
	forbidden := []handler.Capability{
		handler.CapabilityWorkStart, handler.CapabilityWorkSet, handler.CapabilityWorkRequestReview,
		handler.CapabilityWorkReply, handler.CapabilityWorkVerdict, handler.CapabilityWorkRead,
		handler.CapabilityGitClone, handler.CapabilityGitPush,
	}
	for _, capability := range forbidden {
		if slices.Contains(role.GetOperations(), string(capability)) {
			return fmt.Errorf("the orchestrator role holds %q; it supervises, the agents act", capability)
		}
	}
	return nil
}

// --- "An author may not submit a verdict" / "A reviewer may not start a
// work branch or push" ---

// stepIAmAnAgentWithTheRole sets world.currentActor to a freshly fabricated
// identity presenting role -- these two scenarios name only a role, never a
// literal agent identifier, unlike features/instructions.feature's
// Background.
func (h *acceptanceHarness) stepIAmAnAgentWithTheRole(ctx context.Context, role string) error {
	world := worldFrom(ctx)
	world.currentActor = acceptanceActor{name: "acceptance-roles-agent", id: world.agentID, role: role}
	return nil
}

// stepITryToSubmitAVerdict runs `loam work verdict` as world.currentActor
// against an UNREGISTERED work branch name and leaves world.lastDenialCheck
// set to the DECISIVE assertion the following "the operation is denied"
// consumes: the CLI's own "unauthorized" error class (exit 2), which is
// exactly what classifyConnectError (internal/cli/errormapper.go) produces
// from a connect.CodePermissionDenied -- and nothing else maps to it. This
// is safe against an unregistered branch specifically because
// SubmitVerdict's capability check (internal/handler/workbranch/review.go)
// runs BEFORE the handler ever resolves the work branch, so the permission
// denial is reached deterministically regardless of whether the branch is
// real; a scenario that instead needed a real reviewable branch to reach
// this code path would risk the exact vacuous-pass trap this bead warns
// about (denied because the branch does not exist, not because the role
// lacks work.verdict).
func (h *acceptanceHarness) stepITryToSubmitAVerdict(ctx context.Context) error {
	world := worldFrom(ctx)
	actor := world.currentActor
	res := h.runLoamAs(world, actor, "", "work", "verdict", world.repo(), world.workBranch, "--outcome", "approve")
	world.lastDenialCheck = func() error {
		return requireLoamRejected(res, fmt.Sprintf("loam work verdict (as %s)", actor.identifier()), "unauthorized", 2)
	}
	return nil
}

// stepITryToStartAWorkBranch runs `loam work start` as world.currentActor
// against the enrolled repo's own real target branch, for the same reason
// stepITryToSubmitAVerdict needs no real work branch: CreateWorkBranch's
// capability check (internal/handler/workbranch/workbranch.go) runs before
// it ever touches the repo row or the mirror, so this denial is reached
// deterministically with no mirror fixture at all.
func (h *acceptanceHarness) stepITryToStartAWorkBranch(ctx context.Context) error {
	world := worldFrom(ctx)
	actor := world.currentActor
	res := h.runLoamAs(world, actor, "", "work", "start", world.repo(), world.targetBranch)
	world.lastDenialCheck = func() error {
		return requireLoamRejected(res, fmt.Sprintf("loam work start (as %s)", actor.identifier()), "unauthorized", 2)
	}
	return nil
}

// acceptanceRolesPushAttemptRef is the arbitrary, never-registered work
// branch ref stepITryToPushAsCurrentActor pushes toward. It does not need
// to be a real work branch: internal/handler.GitRoleGate's capability check
// wraps EVERY /git/* request (including the info/refs discovery GET a
// plain `git push` issues first) and denies it before the request ever
// reaches internal/refpolicy's own per-ref rules, so this push is refused
// on the missing git.push capability alone, regardless of the ref's
// validity.
const acceptanceRolesPushAttemptRef = "wb-roles-push-attempt"

// stepITryToPushAsCurrentActor clones world's repo as world.currentActor
// (lazily, once) -- a real, successful clone, since this scenario's actor
// (reviewer) genuinely holds git.clone; this is the "positive control" the
// bead's own guidance calls for: the SAME actor that is about to be denied
// a push is first shown to succeed at an operation its role does grant, so
// the coming denial cannot be blamed on a broken identity or a fixture
// error. It then commits and attempts to push, leaving
// world.lastDenialCheck set to the decisive check: the git transport's own
// "role %q may not push (missing git.push capability)" reason
// (internal/handler/gitrolegate.go's gitRoleGateReason), not merely a
// non-zero exit -- a push that failed for an unrelated reason (a broken
// fixture, an unreachable server) would not prove anything about roles.
func (h *acceptanceHarness) stepITryToPushAsCurrentActor(ctx context.Context) error {
	world := worldFrom(ctx)
	actor := world.currentActor
	if world.clonePath == "" {
		if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
			return err
		}
		cloneRes := h.runLoamAs(world, actor, "", "clone", world.repo(), world.targetBranch)
		if err := requireLoamOK(cloneRes, fmt.Sprintf("loam clone (as %s, the positive control before the push denial)", actor.identifier())); err != nil {
			return err
		}
		world.clonePath = filepath.Join(world.workspace, world.repoName)
	}
	if err := world.writeCommitAndPush("roles-push-attempt.txt", "roles feature push attempt", "acceptance: push attempt under role "+actor.role, "HEAD:"+refnames.WorkBranch(acceptanceRolesPushAttemptRef)); err != nil {
		return err
	}
	world.lastDenialCheck = func() error {
		if world.lastGitErr == nil {
			return fmt.Errorf("push succeeded as role %q, want a permission denial\n%s", actor.role, world.lastGitOutput)
		}
		wantReason := fmt.Sprintf("role %q may not push (missing %s capability)", actor.role, handler.CapabilityGitPush)
		if !strings.Contains(world.lastGitOutput, wantReason) {
			return fmt.Errorf("push failed, but not with the specific permission-denied reason %q -- a failure for any other reason would not prove this role lacks git.push:\n%s", wantReason, world.lastGitOutput)
		}
		return nil
	}
	return nil
}

// stepTheOperationIsDenied consumes world.lastDenialCheck, the DECISIVE
// assertion left behind by whichever "When I try to ..." step ran
// immediately before it -- necessary because this one Gherkin sentence is
// reused, in this file, for two physically different failure channels (a
// JSON RPC error from the compiled CLI, and a git smart-HTTP 403), and only
// the step that just drove one of them knows which fingerprint to check.
// The check is nilled out the moment it is consumed, so a scenario with two
// denials in a row (start, then push) can never let the second "Then"
// vacuously re-check the first attempt's outcome; a missing check (this
// step reached with none pending) fails loudly rather than passing on
// nothing.
func (h *acceptanceHarness) stepTheOperationIsDenied(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.lastDenialCheck == nil {
		return fmt.Errorf(`"the operation is denied" has nothing to check: no "When I try to ..." denial step ran immediately before it`)
	}
	check := world.lastDenialCheck
	world.lastDenialCheck = nil
	return check()
}

// --- "A reviewer may clone the repo" ---

// stepICloneAsCurrentActor is "When I clone \"...\"": the single-argument
// form (repo only, no explicit branch), distinct from clone-and-push.
// feature's "I clone \"...\" at \"...\"" -- this scenario clones the
// enrolled repo at its own real target branch, the only branch the
// Background establishes. It builds the mirror from upstream first (never
// built by this feature file's own Background), so a successful clone here
// reflects the reviewer role's real git.clone grant, not a fixture that
// happened to have nothing to serve.
func (h *acceptanceHarness) stepICloneAsCurrentActor(ctx context.Context, repo string) error {
	world := worldFrom(ctx)
	if repo != world.repo() {
		return fmt.Errorf("scenario names repo %q, but the enrolled repo is %q", repo, world.repo())
	}
	if err := h.ensureMirrorFromUpstream(ctx, world); err != nil {
		return err
	}
	actor := world.currentActor
	world.lastCLI = h.runLoamAs(world, actor, "", "clone", repo, world.targetBranch)
	world.clonePath = filepath.Join(world.workspace, world.repoName)
	return nil
}

// stepTheCloneSucceeds asserts the clone exited 0 and actually landed on
// disk -- the positive control this scenario exists to be: proof the
// reviewer role's git.clone grant genuinely works, which is what makes the
// OTHER scenarios' push/start denials mean something about ROLES rather
// than about a broken actor.
func (h *acceptanceHarness) stepTheCloneSucceeds(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := requireLoamOK(world.lastCLI, fmt.Sprintf("loam clone (as %s)", world.currentActor.identifier())); err != nil {
		return err
	}
	return assertDirExists(world.clonePath)
}

// --- "Updating a role changes what its agents may do" ---

// stepACustomRoleWithoutPush creates a real admin-defined role via
// CreateRole, granting git.clone/work.start/work.read but deliberately
// NOT git.push -- the precondition "I grant it the push operation" then
// changes. world.customRole records the name for afterScenario
// (acceptance_world_test.go's deleteCustomRole) to remove, and this step
// re-reads the role back to confirm git.push is genuinely absent, so the
// later grant is a real change rather than a no-op the fixture only
// claims to be testing.
func (h *acceptanceHarness) stepACustomRoleWithoutPush(ctx context.Context, name string) error {
	world := worldFrom(ctx)
	client := h.newRoleServiceClient()
	createResp, err := client.CreateRole(ctx, connect.NewRequest(&adminv1.CreateRoleRequest{Role: &adminv1.Role{
		Name:       name,
		Operations: []string{string(handler.CapabilityGitClone), string(handler.CapabilityWorkStart), string(handler.CapabilityWorkRead)},
	}}))
	if err != nil {
		return fmt.Errorf("creating custom role %s: %w", name, err)
	}
	world.customRole = name
	if slices.Contains(createResp.Msg.GetRole().GetOperations(), string(handler.CapabilityGitPush)) {
		return fmt.Errorf("role %s was created WITH git.push already granted; this scenario's own fixture is not testing what it claims", name)
	}
	return nil
}

// stepIGrantItThePushOperation reads world.customRole's CURRENT operations
// and UpdateRoles it with git.push added -- a genuine read-modify-write
// through the real admin RoleService, exactly the flow an operator would
// use, rather than a hardcoded operations list that would silently also
// revoke git.clone/work.start/work.read.
func (h *acceptanceHarness) stepIGrantItThePushOperation(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.customRole == "" {
		return fmt.Errorf(`"I grant it the push operation" has no role to grant: no custom role was created earlier in this scenario`)
	}
	client := h.newRoleServiceClient()
	getResp, err := client.GetRole(ctx, connect.NewRequest(&adminv1.GetRoleRequest{Name: world.customRole}))
	if err != nil {
		return fmt.Errorf("reading role %s before granting push: %w", world.customRole, err)
	}
	operations := append(slices.Clone(getResp.Msg.GetRole().GetOperations()), string(handler.CapabilityGitPush))
	_, err = client.UpdateRole(ctx, connect.NewRequest(&adminv1.UpdateRoleRequest{Role: &adminv1.Role{
		Name:         world.customRole,
		Operations:   operations,
		Instructions: getResp.Msg.GetRole().GetInstructions(),
	}}))
	if err != nil {
		return fmt.Errorf("granting git.push to role %s: %w", world.customRole, err)
	}
	return nil
}

// stepAgentsWithThatRoleMayPush proves the grant took effect on a REAL
// agent, not merely on the role row: it seeds a work branch owned by a
// fresh identity presenting world.customRole, clones it (git.clone was
// already granted at creation), commits, and pushes for real -- then reads
// the mirror ref back and compares it against the clone's own HEAD, so a
// push that exited 0 without actually landing (or a handler that silently
// no-ops) cannot satisfy this Then the way a bare exit-code check could.
func (h *acceptanceHarness) stepAgentsWithThatRoleMayPush(ctx context.Context) error {
	world := worldFrom(ctx)
	if world.customRole == "" {
		return fmt.Errorf(`"agents with that role may push" has no role to check: no custom role was created earlier in this scenario`)
	}
	actor := acceptanceActor{name: "acceptance-release-captain", id: world.agentID, role: world.customRole}
	branch := "wb-release-captain-push"
	if err := h.insertWorkBranchRow(ctx, world.repoID, branch, world.targetBranch, "draft", actor.identifier()); err != nil {
		return err
	}
	mirrorDir, err := seedBareMirrorWithBranches(ctx, h.server.dataDir, world.repo(), world.targetBranch, branch)
	if err != nil {
		return err
	}
	world.mirrorDir = mirrorDir
	if err := h.reconcileSeededMirror(ctx, mirrorDir); err != nil {
		return err
	}
	cloneRes := h.runLoamAs(world, actor, "", "clone", world.repo(), branch)
	if err := requireLoamOK(cloneRes, fmt.Sprintf("loam clone (as %s, setting up the push this Then checks)", actor.identifier())); err != nil {
		return err
	}
	world.clonePath = filepath.Join(world.workspace, world.repoName)
	if err := world.writeCommitAndPush("release-captain-change.txt", "release captain push", "acceptance: release captain push after the grant", branch); err != nil {
		return err
	}
	if world.lastGitErr != nil {
		return fmt.Errorf("push failed after granting role %s git.push: %v\n%s", world.customRole, world.lastGitErr, world.lastGitOutput)
	}
	ref := refnames.WorkBranch(branch)
	mirrorSHA, err := mirrorRefSHA(mirrorDir, ref)
	if err != nil {
		return fmt.Errorf("reading mirror ref %s after the push: %w", ref, err)
	}
	cloneSHA, err := cloneHeadSHA(world.clonePath)
	if err != nil {
		return fmt.Errorf("reading the clone's HEAD after the push: %w", err)
	}
	if mirrorSHA != cloneSHA {
		return fmt.Errorf("mirror ref %s is %s after the push, want the pushed commit %s -- the push exited 0 but did not actually land", ref, mirrorSHA, cloneSHA)
	}
	return nil
}

// --- "Built-in roles cannot be deleted" ---

// stepITryToDeleteTheBuiltinRole calls the real admin RoleService.
// DeleteRole for name, recording the outcome on world.lastRPCErr/
// rpcAttempted -- the same pair acceptance_proposal_test.go's admin-RPC
// denial steps use, so the shared requireRPCRejected helper applies here
// unchanged.
func (h *acceptanceHarness) stepITryToDeleteTheBuiltinRole(ctx context.Context, name string) error {
	world := worldFrom(ctx)
	_, err := h.newRoleServiceClient().DeleteRole(ctx, connect.NewRequest(&adminv1.DeleteRoleRequest{Name: name}))
	world.lastRPCErr = err
	world.rpcAttempted = true
	return nil
}

// stepTheDeletionIsRejected asserts the DECISIVE code
// internal/handler/role.DeleteRole documents for a built-in: FailedPrecondition,
// not merely "an error" -- a role that failed to delete for some other
// reason (e.g. a malformed request) would not prove the built-in
// protection this scenario is about.
func (h *acceptanceHarness) stepTheDeletionIsRejected(ctx context.Context) error {
	world := worldFrom(ctx)
	if !world.rpcAttempted {
		return fmt.Errorf(`"the deletion is rejected" has nothing to check: no delete attempt ran before it`)
	}
	return requireRPCRejected(world.lastRPCErr, "DeleteRole for a built-in role", connect.CodeFailedPrecondition)
}

// --- "In the MVP, an agent's role is trusted from its environment" ---

// stepAnAgentPresentingTheRoleInItsEnvironment sets world.currentActor to a
// freshly fabricated identity presenting role -- one that has never been
// seen by this server before, no prior registration or handshake, exactly
// the MVP's trusted-header model (docs/web-spec.md -> Auth).
func (h *acceptanceHarness) stepAnAgentPresentingTheRoleInItsEnvironment(ctx context.Context, role string) error {
	world := worldFrom(ctx)
	world.currentActor = acceptanceActor{name: "acceptance-trust-env", id: world.agentID, role: role}
	return nil
}

// stepTheServerTreatsItAsThatRoleWithoutFurtherAuthentication asserts two
// things about world.currentActor, an identity this process invents fresh
// and presents for the very first time, with no prior request of any kind:
//
//   - `loam whoami` reports back EXACTLY the identity presented (not a
//     substituted default, not an error demanding some other credential)
//     -- proving the server needed nothing beyond the environment to
//     resolve who this is.
//   - `loam instructions`' command list matches the role's real, live
//     RoleService grant in BOTH directions (present iff granted) -- proving
//     the server did not merely ACCEPT the presented role but genuinely
//     APPLIED its actual capabilities. Without this half, a server that
//     silently treated every unrecognized identity as a full admin (or as
//     no one at all) would satisfy the whoami check alone while getting
//     "authorization" completely wrong in either direction.
//
// This deliberately reuses commandPresenceSet/acceptanceCommandCapability/
// grantedCapabilities (acceptance_instructions_test.go,
// acceptance_roles_test.go's own stepOnlyTheCommandsItsRolePermits) rather
// than re-deriving the vocabulary: those are already this suite's
// established, live-ground-truth way of checking a command list against a
// role's real grant.
func (h *acceptanceHarness) stepTheServerTreatsItAsThatRoleWithoutFurtherAuthentication(ctx context.Context) error {
	world := worldFrom(ctx)
	actor := world.currentActor
	res := h.runLoamAs(world, actor, "", "whoami")
	var who acceptanceWhoamiOutput
	if err := decodeLoamJSON(res, "whoami", &who); err != nil {
		return err
	}
	if who.Role != actor.role || who.Name != actor.name || who.ID != actor.id || who.Identifier != actor.identifier() {
		return fmt.Errorf("whoami reported %+v for an identity that presented name=%q id=%q role=%q ONLY via its environment, with no prior registration -- the server must reflect exactly what was presented", who, actor.name, actor.id, actor.role)
	}
	granted, err := h.grantedCapabilities(ctx, actor.role)
	if err != nil {
		return err
	}
	instrRes := h.runLoamAs(world, actor, "", "instructions")
	var out acceptanceInstructionsOutput
	if err := decodeLoamJSON(instrRes, "instructions", &out); err != nil {
		return err
	}
	present := commandPresenceSet(out.Commands)
	for name, capability := range acceptanceCommandCapability {
		wantPresent := granted[string(capability)]
		if present[name] != wantPresent {
			return fmt.Errorf("command %q present=%v, but role %q's real, live grant of capability %q is %v -- the server did not apply this role's actual capabilities purely from the presented environment", name, present[name], actor.role, capability, wantPresent)
		}
	}
	return nil
}
