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

	"connectrpc.com/connect"
	"github.com/cucumber/godog"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
	"github.com/bobcob7/loam/internal/handler"
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

// acceptanceCommandCapability mirrors internal/handler/meta/catalog.go's
// commandCatalog -- the pairing of each GATED CLI command to the single
// capability that gates it -- reproduced here because that table is
// unexported. This is NOT a second copy of the fixed ten-capability
// vocabulary (loam-ofg.13's own concern): every value here is one of the
// handler.Capability constants, never a restated string literal, and the
// FIXED SET of which capabilities exist still comes from
// handler.AllCapabilities()/handler.Capability.Valid alone. What this map
// restates is the separate, stable CLI-surface question "which command
// does this capability gate" (docs/cli-spec.md's Commands table), which
// has no exported form to read instead.
var acceptanceCommandCapability = map[string]handler.Capability{
	"clone":               handler.CapabilityGitClone,
	"work start":          handler.CapabilityWorkStart,
	"work set":            handler.CapabilityWorkSet,
	"work request-review": handler.CapabilityWorkRequestReview,
	"work list":           handler.CapabilityWorkRead,
	"work show":           handler.CapabilityWorkRead,
	"work diff":           handler.CapabilityWorkRead,
	"work comments":       handler.CapabilityWorkRead,
	"work verdicts":       handler.CapabilityWorkRead,
	"work comment":        handler.CapabilityWorkVerdict,
	"work reply":          handler.CapabilityWorkReply,
	"work verdict":        handler.CapabilityWorkVerdict,
	"graph":               handler.CapabilityGraphQuery,
	"search":              handler.CapabilitySearch,
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
