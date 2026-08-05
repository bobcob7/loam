//go:build acceptance

// Step definitions for features/instructions.feature (loam-gp31). The
// Background names one agent identity by its full "<name>-<id>-<role>"
// identifier; every step below acts as that identity (world.currentActor)
// against the real, in-process MetaService via the compiled loam CLI
// (runLoamAs, acceptance_review_test.go), never a hand-rolled client, so
// these scenarios exercise the exact binary docs/cli-spec.md documents.
//
// "whoami works without contacting the server" is the one scenario here
// that is NOT just an output check: it has to prove no RPC was even
// ATTEMPTED, not merely that the command succeeded. stepTheServerIsUnreachable
// makes a dial-out observable by pointing LOAM_SERVER_URL at a TCP address
// this process itself just freed (bind, read it, close it) -- guaranteed
// nothing is listening there for the rest of the scenario, and a connection
// attempt against it fails FAST (ECONNREFUSED, not a routing timeout), so a
// whoami that regressed into making an RPC would fail the following step's
// exit-0/identity check loudly rather than hang the run.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"connectrpc.com/connect"
	"github.com/cucumber/godog"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
)

// registerInstructionsSteps wires every step features/instructions.feature
// needs: the Background identity, the three `instructions` scenarios, and
// the two `whoami` scenarios.
func (h *acceptanceHarness) registerInstructionsSteps(sc *godog.ScenarioContext) {
	sc.Step(`^I am the agent "([^"]*)" with the "([^"]*)" role$`, h.stepIAmTheAgentWithRole)
	sc.Step(`^I ask for instructions$`, h.stepIAskForInstructions)
	sc.Step(`^I receive general usage and conventions$`, h.stepIReceiveGeneralUsageAndConventions)
	sc.Step(`^the commands available to my role$`, h.stepTheCommandsAvailableToMyRole)
	sc.Step(`^the instructions configured for my role$`, h.stepTheInstructionsConfiguredForMyRole)
	sc.Step(`^commands my role cannot perform are not listed$`, h.stepCommandsMyRoleCannotPerformAreNotListed)
	sc.Step(`^I ask for instructions for one command$`, h.stepIAskForInstructionsForOneCommand)
	sc.Step(`^I receive only that command's usage$`, h.stepIReceiveOnlyThatCommandsUsage)
	sc.Step(`^no agent identity is configured$`, h.stepNoAgentIdentityIsConfigured)
	sc.Step(`^I receive the orchestrator role's instructions$`, h.stepIReceiveTheOrchestratorRolesInstructions)
	sc.Step(`^only the commands the orchestrator role permits$`, h.stepOnlyTheCommandsTheOrchestratorRolePermits)
	sc.Step(`^I ask who I am$`, h.stepIAskWhoIAm)
	sc.Step(`^I am told my name, id, role, and full identifier$`, h.stepIAmToldMyIdentity)
	sc.Step(`^the server is unreachable$`, h.stepTheServerIsUnreachable)
	sc.Step(`^I still get my identity from the environment$`, h.stepIAmToldMyIdentity)
	sc.Step(`^I ask to verify who I am$`, h.stepIAskToVerifyWhoIAm)
	sc.Step(`^the verification is rejected as unauthorized, naming the role$`, h.stepTheVerificationIsRejectedAsUnauthorizedNamingTheRole)
}

// acceptanceWhoamiOutput mirrors internal/cli/commands_root.go's
// whoamiOutput -- `loam whoami`'s JSON shape -- reproduced here rather
// than imported, since that type is unexported.
type acceptanceWhoamiOutput struct {
	Name       string `json:"name"`
	ID         string `json:"id"`
	Role       string `json:"role"`
	Identifier string `json:"identifier"`
}

// acceptanceHelpCommand and acceptanceHelpCommandSummary are what "Help for
// a single command" asks about: "clone", chosen because the Background's
// "author" role is actually granted git.clone (migration 0001_init), so
// this scenario narrows to a real, GRANTED command rather than one that
// would 404 for this role. The summary mirrors commandCatalog's own
// "clone" entry (internal/handler/meta/catalog.go), reproduced here since
// that table is unexported.
const (
	acceptanceHelpCommand        = "clone"
	acceptanceHelpCommandSummary = "Clone an enrolled repo at a branch and bootstrap it for plain git."
)

// stepIAmTheAgentWithRole sets world.currentActor from the Background's two
// literals: the full agent identifier and, redundantly, its role --
// redundant because the identifier's own trailing segment already encodes
// it (parseAcceptanceActor), which this step checks agree rather than
// silently trusting one over the other.
func (h *acceptanceHarness) stepIAmTheAgentWithRole(ctx context.Context, identifier, role string) error {
	world := worldFrom(ctx)
	actor, err := parseAcceptanceActor(identifier)
	if err != nil {
		return err
	}
	if actor.role != role {
		return fmt.Errorf("identifier %q encodes role %q, but the scenario named role %q", identifier, actor.role, role)
	}
	world.currentActor = actor
	return nil
}

// stepIAskForInstructions runs `loam instructions` as world.currentActor
// and decodes its response into world.lastInstructions for the following
// Then steps.
func (h *acceptanceHarness) stepIAskForInstructions(ctx context.Context) error {
	world := worldFrom(ctx)
	res := h.runLoamAs(world, world.currentActor, "", "instructions")
	var out acceptanceInstructionsOutput
	if err := decodeLoamJSON(res, "instructions", &out); err != nil {
		return err
	}
	world.lastInstructions = out
	return nil
}

// stepIReceiveGeneralUsageAndConventions asserts the response's usage text
// is the real static guide (internal/handler/meta/catalog.go's usageText),
// not merely non-empty -- checked against a phrase specific enough that
// nothing else in this codebase would satisfy it by accident.
func (h *acceptanceHarness) stepIReceiveGeneralUsageAndConventions(ctx context.Context) error {
	world := worldFrom(ctx)
	const wantPhrase = `there are no "loam commit"/"loam push" commands`
	if !strings.Contains(world.lastInstructions.Usage, wantPhrase) {
		return fmt.Errorf("usage %q does not contain the expected static usage guide phrase %q", world.lastInstructions.Usage, wantPhrase)
	}
	return nil
}

// commandPresenceSet indexes an `instructions` response's command list by
// name, for the membership checks below.
func commandPresenceSet(commands []acceptanceInstructionsCommand) map[string]bool {
	present := make(map[string]bool, len(commands))
	for _, c := range commands {
		present[c.Name] = true
	}
	return present
}

// grantedCapabilities reads role's real, currently-granted operations from
// the admin RoleService -- the live ground truth every command-list
// assertion in this file checks the CLI's response against, rather than an
// assumption frozen at write time.
func (h *acceptanceHarness) grantedCapabilities(ctx context.Context, role string) (map[string]bool, error) {
	getResp, err := h.newRoleServiceClient().GetRole(ctx, connect.NewRequest(&adminv1.GetRoleRequest{Name: role}))
	if err != nil {
		return nil, fmt.Errorf("reading role %s's granted operations: %w", role, err)
	}
	granted := make(map[string]bool, len(getResp.Msg.GetRole().GetOperations()))
	for _, op := range getResp.Msg.GetRole().GetOperations() {
		granted[op] = true
	}
	return granted, nil
}

// --- "An agent with no identity configured is answered as the orchestrator" ---

// acceptanceOrchestratorRole is the role name the CLI's well-known identity
// resolves to (internal/cli/config.go's wellKnownAgentRole), seeded as a
// built-in by migration 0009_orchestrator_role.
const acceptanceOrchestratorRole = "orchestrator"

// stepNoAgentIdentityIsConfigured drops the three LOAM_AGENT_* variables
// from every subsequent `loam` invocation in this scenario (runLoamAs reads
// world.omitIdentity), leaving LOAM_SERVER_URL as the only LOAM_* variable
// set. This is the whole precondition loam-hi5o.31 is about: "no identity"
// means no LOAM_AGENT_*, NOT no environment at all -- the CLI cannot invent
// where the server is.
func (h *acceptanceHarness) stepNoAgentIdentityIsConfigured(ctx context.Context) error {
	worldFrom(ctx).omitIdentity = true
	return nil
}

// stepIReceiveTheOrchestratorRolesInstructions asserts the identity-free
// response carried the ORCHESTRATOR role's configured text, read fresh from
// the admin RoleService -- byte-identical, the same standard
// stepTheInstructionsConfiguredForMyRole holds an identified agent to. It
// additionally refuses the Background author's text explicitly: this
// scenario runs after a Background that set currentActor to an author, and
// a regression that ignored world.omitIdentity would answer as that author
// and otherwise look like a success.
func (h *acceptanceHarness) stepIReceiveTheOrchestratorRolesInstructions(ctx context.Context) error {
	world := worldFrom(ctx)
	getResp, err := h.newRoleServiceClient().GetRole(ctx, connect.NewRequest(&adminv1.GetRoleRequest{Name: acceptanceOrchestratorRole}))
	if err != nil {
		return fmt.Errorf("reading the orchestrator role's configured instructions: %w", err)
	}
	want := getResp.Msg.GetRole().GetInstructions()
	if want == "" {
		return fmt.Errorf("the built-in orchestrator role has empty instructions; migration 0009 must seed them")
	}
	if world.lastInstructions.RoleInstructions != want {
		return fmt.Errorf("got role_instructions %q, want the orchestrator role's own text %q", world.lastInstructions.RoleInstructions, want)
	}
	authorResp, err := h.newRoleServiceClient().GetRole(ctx, connect.NewRequest(&adminv1.GetRoleRequest{Name: world.currentActor.role}))
	if err != nil {
		return fmt.Errorf("reading role %s's instructions to contrast: %w", world.currentActor.role, err)
	}
	if world.lastInstructions.RoleInstructions == authorResp.Msg.GetRole().GetInstructions() {
		return fmt.Errorf("the identity-free response returned the Background %q role's instructions, not the orchestrator's", world.currentActor.role)
	}
	return nil
}

// stepOnlyTheCommandsTheOrchestratorRolePermits is loam-hi5o.3's
// superseded-but-not-discarded requirement made checkable. That bead
// refused to let `instructions` run without an identity because an
// UNFILTERED command list could be read by an agent as its own
// permissions; this asserts the identity-free response is filtered to the
// orchestrator's own narrow grant in BOTH directions -- every command its
// capabilities predict present, every other gated command absent. A
// response that had regressed to an unfiltered catalog would list `work
// start` and fail here.
func (h *acceptanceHarness) stepOnlyTheCommandsTheOrchestratorRolePermits(ctx context.Context) error {
	world := worldFrom(ctx)
	granted, err := h.grantedCapabilities(ctx, acceptanceOrchestratorRole)
	if err != nil {
		return err
	}
	present := commandPresenceSet(world.lastInstructions.Commands)
	if !present["instructions"] || !present["whoami"] {
		return fmt.Errorf("instructions/whoami must always be present regardless of role, got %v", world.lastInstructions.Commands)
	}
	for name, capability := range acceptanceCommandCapability {
		wantPresent := granted[string(capability)]
		if present[name] != wantPresent {
			return fmt.Errorf("command %q present=%v, but the orchestrator role's grant of capability %q is %v", name, present[name], capability, wantPresent)
		}
	}
	return nil
}

// stepTheCommandsAvailableToMyRole asserts every command the role's real,
// currently-granted capabilities predict is present in the response --
// PRESENCE only, matching this Then step's plain meaning ("the commands
// available to my role"). stepCommandsMyRoleCannotPerformAreNotListed,
// below, is what checks a withheld command is genuinely ABSENT.
// instructions/whoami are ungated, so they must be present regardless of
// role.
func (h *acceptanceHarness) stepTheCommandsAvailableToMyRole(ctx context.Context) error {
	world := worldFrom(ctx)
	present := commandPresenceSet(world.lastInstructions.Commands)
	if !present["instructions"] || !present["whoami"] {
		return fmt.Errorf("instructions/whoami must always be present regardless of role, got %v", world.lastInstructions.Commands)
	}
	granted, err := h.grantedCapabilities(ctx, world.currentActor.role)
	if err != nil {
		return err
	}
	for name, capability := range acceptanceCommandCapability {
		if granted[string(capability)] && !present[name] {
			return fmt.Errorf("command %q should be listed for role %q (capability %q is granted) but was not; got %v", name, world.currentActor.role, capability, world.lastInstructions.Commands)
		}
	}
	return nil
}

// stepTheInstructionsConfiguredForMyRole asserts the CLI's
// role_instructions field is byte-identical to the role's actual,
// currently-configured instructions text, read fresh from the admin
// RoleService -- not merely non-nil, so a handler bug that returned some
// OTHER role's instructions, the static usage text, or a hardcoded literal
// would fail this rather than pass on a vacuous "it received something"
// check.
func (h *acceptanceHarness) stepTheInstructionsConfiguredForMyRole(ctx context.Context) error {
	world := worldFrom(ctx)
	getResp, err := h.newRoleServiceClient().GetRole(ctx, connect.NewRequest(&adminv1.GetRoleRequest{Name: world.currentActor.role}))
	if err != nil {
		return fmt.Errorf("reading role %s's configured instructions: %w", world.currentActor.role, err)
	}
	want := getResp.Msg.GetRole().GetInstructions()
	if world.lastInstructions.RoleInstructions != want {
		return fmt.Errorf("got role_instructions %q, want %q (the role's actual configured instructions)", world.lastInstructions.RoleInstructions, want)
	}
	return nil
}

// stepCommandsMyRoleCannotPerformAreNotListed asserts the CLI's command
// list is EXACTLY what the role's real, currently-granted operations
// predict: every ungated command present, and every gated command present
// if and only if its capability is in the role's live GetRole response.
// Checking BOTH directions is deliberate (loam-gp31's own trap warning):
// a test that only checked presence would pass identically for an
// unfiltered list, so this also asserts ABSENCE of every command whose
// capability the role does not hold -- for the Background's "author" role,
// that includes "work verdict"/"work comment" (both gated by
// work.verdict, which migration 0001_init does not grant author).
func (h *acceptanceHarness) stepCommandsMyRoleCannotPerformAreNotListed(ctx context.Context) error {
	world := worldFrom(ctx)
	granted, err := h.grantedCapabilities(ctx, world.currentActor.role)
	if err != nil {
		return err
	}
	present := commandPresenceSet(world.lastInstructions.Commands)
	if !present["instructions"] || !present["whoami"] {
		return fmt.Errorf("instructions/whoami must always be present regardless of role, got %v", world.lastInstructions.Commands)
	}
	for name, capability := range acceptanceCommandCapability {
		wantPresent := granted[string(capability)]
		if present[name] != wantPresent {
			return fmt.Errorf("command %q present=%v, but role %q's grant of capability %q is %v", name, present[name], world.currentActor.role, capability, wantPresent)
		}
	}
	return nil
}

// stepIAskForInstructionsForOneCommand runs `loam instructions
// <acceptanceHelpCommand>` as world.currentActor, decoding the narrowed
// response into world.lastInstructions.
func (h *acceptanceHarness) stepIAskForInstructionsForOneCommand(ctx context.Context) error {
	world := worldFrom(ctx)
	res := h.runLoamAs(world, world.currentActor, "", "instructions", acceptanceHelpCommand)
	var out acceptanceInstructionsOutput
	if err := decodeLoamJSON(res, "instructions "+acceptanceHelpCommand, &out); err != nil {
		return err
	}
	world.lastInstructions = out
	return nil
}

// stepIReceiveOnlyThatCommandsUsage asserts the narrowed response's command
// list has EXACTLY one entry, and that it is acceptanceHelpCommand's own
// name and summary -- not a truncation of the full catalog and not some
// other command's entry.
func (h *acceptanceHarness) stepIReceiveOnlyThatCommandsUsage(ctx context.Context) error {
	world := worldFrom(ctx)
	if len(world.lastInstructions.Commands) != 1 {
		return fmt.Errorf("got %d commands, want exactly 1 (%q)\ncommands: %v", len(world.lastInstructions.Commands), acceptanceHelpCommand, world.lastInstructions.Commands)
	}
	got := world.lastInstructions.Commands[0]
	if got.Name != acceptanceHelpCommand {
		return fmt.Errorf("got command %q, want %q", got.Name, acceptanceHelpCommand)
	}
	if got.Summary != acceptanceHelpCommandSummary {
		return fmt.Errorf("got summary %q, want %q", got.Summary, acceptanceHelpCommandSummary)
	}
	return nil
}

// stepIAskWhoIAm runs `loam whoami` as world.currentActor -- under
// whatever LOAM_SERVER_URL is currently in effect, which
// stepTheServerIsUnreachable may have pointed at a guaranteed-dead address
// -- and decodes its response into world.lastWhoami.
func (h *acceptanceHarness) stepIAskWhoIAm(ctx context.Context) error {
	world := worldFrom(ctx)
	res := h.runLoamAs(world, world.currentActor, "", "whoami")
	var out acceptanceWhoamiOutput
	if err := decodeLoamJSON(res, "whoami", &out); err != nil {
		return err
	}
	world.lastWhoami = out
	return nil
}

// stepIAmToldMyIdentity asserts the decoded `whoami` response is EXACTLY
// world.currentActor's identity in every field -- not merely present, so a
// swapped name/role or a fabricated identifier would fail this rather than
// a check that only confirms the fields are non-empty. It backs both "I am
// told my name, id, role, and full identifier" and "I still get my
// identity from the environment" (whoami-offline): the latter's whole
// point is that this SAME correctness holds even when nothing could have
// answered a network request, because decodeLoamJSON above already failed
// the step outright if the invocation did not exit 0 -- which is exactly
// what a whoami that regressed into dialing the (guaranteed-unreachable)
// server would do.
func (h *acceptanceHarness) stepIAmToldMyIdentity(ctx context.Context) error {
	world := worldFrom(ctx)
	actor := world.currentActor
	got := world.lastWhoami
	if got.Name != actor.name || got.ID != actor.id || got.Role != actor.role || got.Identifier != actor.identifier() {
		return fmt.Errorf("got whoami %+v, want name=%q id=%q role=%q identifier=%q", got, actor.name, actor.id, actor.role, actor.identifier())
	}
	return nil
}

// stepIAskToVerifyWhoIAm runs `loam whoami --verify` as world.currentActor
// and stashes the RAW result (world.lastCLI), not a decoded success value:
// unlike stepIAskWhoIAm, this scenario's whole point is that the role does
// not resolve, so the following Then step must be able to observe a
// non-zero exit and a structured error document, which decodeLoamJSON
// (which stepIAskWhoIAm uses) would treat as a hard failure of the step
// itself rather than the outcome under test.
func (h *acceptanceHarness) stepIAskToVerifyWhoIAm(ctx context.Context) error {
	world := worldFrom(ctx)
	world.lastCLI = h.runLoamAs(world, world.currentActor, "", "whoami", "--verify")
	return nil
}

// stepTheVerificationIsRejectedAsUnauthorizedNamingTheRole is loam-0pj.16's
// and loam-a8z's joint acceptance criterion, exercised together: an agent
// whose configured role the server does not recognize (world.currentActor's
// role here is never seeded by any Background or fixture in this suite --
// exactly the live LOAM_AGENT_ROLE=admin reproduction the two beads were
// filed from) must get "unauthorized" (exit 2), not "internal" (exit 1) --
// which is what reached the wire before loam-a8z's handler-boundary mapping
// existed -- and the message must name the offending role, not just say
// "internal error" and leave the operator to guess. requireLoamRejected
// (acceptance_review_test.go) asserts the code and exit class together, so
// a harness typo that merely produces SOME non-zero exit cannot pass this.
func (h *acceptanceHarness) stepTheVerificationIsRejectedAsUnauthorizedNamingTheRole(ctx context.Context) error {
	world := worldFrom(ctx)
	if err := requireLoamRejected(world.lastCLI, "loam whoami --verify", "unauthorized", 2); err != nil {
		return err
	}
	var payload acceptanceCLIError
	if err := json.Unmarshal([]byte(world.lastCLI.stdout), &payload); err != nil {
		return fmt.Errorf("decoding loam whoami --verify error document: %w\nstdout: %s", err, world.lastCLI.stdout)
	}
	if !strings.Contains(payload.Error.Message, world.currentActor.role) {
		return fmt.Errorf("rejection message %q does not name the offending role %q", payload.Error.Message, world.currentActor.role)
	}
	return nil
}

// stepTheServerIsUnreachable makes a dial-out from the CLI OBSERVABLE
// rather than merely absent from the transcript: it binds a TCP listener,
// reads its address, and immediately closes it, so world.unreachableServerURL
// names a port GUARANTEED to have nothing listening on it for the rest of
// this scenario. runLoamAs (acceptance_review_test.go) points
// LOAM_SERVER_URL there instead of the real server whenever this field is
// set. A whoami that regressed into making an RPC would hit ECONNREFUSED
// against this address -- fast, since nothing is listening locally, not a
// multi-second routing timeout -- and fail the following step's exit-0
// check; the real implementation (internal/cli/commands_root.go's
// runWhoami takes no context because it makes no call that could use one)
// never dials out at all, so it is unaffected by the address being
// unreachable.
func (h *acceptanceHarness) stepTheServerIsUnreachable(ctx context.Context) error {
	world := worldFrom(ctx)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("reserving an address to prove unreachable: %w", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return fmt.Errorf("freeing the reserved address: %w", err)
	}
	world.unreachableServerURL = "http://" + addr
	return nil
}
