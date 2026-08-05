package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Environment variable names the CLI reads (see docs/cli-spec.md ->
// Environment Variables). This is the CLI's own, much smaller surface —
// distinct from internal/config, which loads the server's 12-var surface.
const (
	envServerURL    = "LOAM_SERVER_URL"
	envAgentName    = "LOAM_AGENT_NAME"
	envAgentID      = "LOAM_AGENT_ID"
	envAgentRole    = "LOAM_AGENT_ROLE"
	envOutputFormat = "LOAM_OUTPUT_FORMAT"
)

// The well-known orchestrator identity `instructions` falls back to when no
// LOAM_AGENT_* variable is configured at all (loam-hi5o.31). It resolves to
// the built-in `orchestrator` role seeded by migration
// 0009_orchestrator_role, which grants graph.query and search and no
// work-branch capability.
//
// WHY AN IDENTITY RATHER THAN AN UNAUTHENTICATED ROUTE: the request then
// travels the ORDINARY authenticated path, carrying Loam-Agent-* headers
// like every other call. That preserves cmd/server/main.go's property that
// RegisterUnauthenticated covers /healthz and /readyz and is "the only such
// exemption" (asserted in docs/server-spec.md), leaves the handler with one
// code path and no no-caller branch, and keeps the call auditable.
//
// It weakens nothing that is not already weak, and docs/orchestration.md
// and README -> Agent Identity & Roles say so where the MVP trust caveat
// already lives, so the two move together: features/roles.feature records
// that an agent's role is trusted exactly as asserted in its environment,
// so a caller who wanted search and graph.query could already have claimed
// role=reviewer and been given them. This is the same trust model, and it
// hardens when that model does.
//
// WHY THESE VALUES, AND WHY THEY ARE FIXED. They are compile-time
// constants, not a fourth environment variable, and an operator cannot
// change them -- the escape hatch is the ordinary one: set LOAM_AGENT_* and
// you get a real identity instead. What genuinely differs between
// deployments is the orchestrator role's TEXT AND CAPABILITIES, and those
// live in an editable role row (see 0009_orchestrator_role.up.sql); a
// variable whose only effect is which synthetic name appears in a log would
// be configuration for its own sake, and a mis-set one would resolve to a
// role that does not exist and fail with a permission denial that named the
// wrong cause.
//
// The resulting identifier is obviously synthetic wherever it is recorded:
// `loam-orchestrator-0-orchestrator`. wellKnownAgentName still satisfies
// requireAgentName's `<first-name>-<last-name>` shape, so it is a legal
// value everywhere a configured name is, but it is plainly not a human name
// of the "ada-lovelace" kind that convention is for, and the id is 0.
//
// Nothing PREVENTS a real agent from being configured with these three
// values: the server never validates the Loam-Agent-* headers at all
// (internal/httpauth's agentIdentityFromHeaders takes them as given), so
// non-collision here is a naming convention, not an enforced guarantee.
// The convention is chosen to be one nobody reaches for by accident, and
// an agent that DID adopt it would gain nothing -- it would resolve to the
// same read-only role.
const (
	wellKnownAgentName = "loam-orchestrator"
	wellKnownAgentID   = "0"
	wellKnownAgentRole = "orchestrator"
)

// envConfig is the loaded, validated LOAM_* configuration (see
// docs/cli-spec.md -> Environment Variables). Immutable once returned by
// loadConfig; there is no package-level mutable state.
type envConfig struct {
	serverURL    string
	agentName    string
	agentID      string
	agentRole    string
	outputFormat string
	identifier   string
}

// OutputFormat returns the active output format: json, yaml, xml, or human.
func (c *envConfig) OutputFormat() string { return c.outputFormat }

// AgentName returns the calling agent's configured name.
func (c *envConfig) AgentName() string { return c.agentName }

// AgentID returns the calling agent's configured id.
func (c *envConfig) AgentID() string { return c.agentID }

// AgentRole returns the calling agent's configured role.
func (c *envConfig) AgentRole() string { return c.agentRole }

// ServerURL returns the base URL of the Loam server.
func (c *envConfig) ServerURL() string { return c.serverURL }

// Identifier returns the resolved "<name>-<id>-<role>" identifier, reused by
// whoami and by the Connect identity headers (see docs/cli-spec.md ->
// Environment Variables).
func (c *envConfig) Identifier() string { return c.identifier }

// resolveOutputFormat reads LOAM_OUTPUT_FORMAT and falls back to "json" for
// an unset or unrecognized value. This never errors, so it can run even
// when the required identity variables below are missing or malformed:
// main() uses it to pick an encoder before it knows whether the rest of the
// config is valid, so a config error can still be reported in the right
// format.
func resolveOutputFormat() string {
	switch format := os.Getenv(envOutputFormat); format {
	case "json", "yaml", "xml", "human":
		return format
	default:
		return "json"
	}
}

// loadConfig reads and validates every LOAM_* environment variable (see
// docs/cli-spec.md -> Environment Variables). A missing or malformed
// required variable (LOAM_SERVER_URL, LOAM_AGENT_NAME, LOAM_AGENT_ID,
// LOAM_AGENT_ROLE) is a usage error (exit 2, per cli-spec -> whoami);
// LOAM_OUTPUT_FORMAT is the sole optional variable and is lenient.
//
// Every one of the four is validated unconditionally, never short-circuited
// on the first failure (loam-dc2v defect 1): an operator setting up a fresh
// workspace with nothing configured at all learns every missing/malformed
// variable from a single run instead of one per run. See joinConfigErrors.
func loadConfig() (*envConfig, error) {
	var errs []error
	serverURL, err := requireServerURL()
	if err != nil {
		errs = append(errs, err)
	}
	agentName, err := requireAgentName()
	if err != nil {
		errs = append(errs, err)
	}
	agentID, err := requireNonEmpty(envAgentID)
	if err != nil {
		errs = append(errs, err)
	}
	agentRole, err := requireNonEmpty(envAgentRole)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, joinConfigErrors(errs)
	}
	return &envConfig{
		serverURL:    serverURL,
		agentName:    agentName,
		agentID:      agentID,
		agentRole:    agentRole,
		outputFormat: resolveOutputFormat(),
		identifier:   fmt.Sprintf("%s-%s-%s", agentName, agentID, agentRole),
	}, nil
}

// loadIdentityConfig validates only the three identity variables --
// LOAM_AGENT_NAME, LOAM_AGENT_ID, LOAM_AGENT_ROLE -- leaving LOAM_SERVER_URL
// unvalidated: ServerURL() reports whatever (possibly malformed) value is
// set, or "" when it is unset entirely.
//
// This exists because `whoami` is the one command docs/cli-spec.md pins as
// "Local only -- no server call" (commands_root.go's runWhoami: bare
// whoami never reaches deps.connect). Identity IS the environment it
// needs, but a server it does not talk to must never gate it (loam-dc2v
// defect 3) -- "require each variable where it is actually used", not the
// union every other command needs. deps.go's NewProductionDeps calls this
// instead of loadConfig specifically for `whoami`, and skips building a
// Connect client at all when the resulting ServerURL() is empty; `whoami
// --verify` (the one whoami path that DOES need a server) checks
// ServerURL() itself before ever touching deps.connect, so it still fails
// cleanly as a usage error rather than dialing a nil client.
func loadIdentityConfig() (*envConfig, error) {
	var errs []error
	agentName, err := requireAgentName()
	if err != nil {
		errs = append(errs, err)
	}
	agentID, err := requireNonEmpty(envAgentID)
	if err != nil {
		errs = append(errs, err)
	}
	agentRole, err := requireNonEmpty(envAgentRole)
	if err != nil {
		errs = append(errs, err)
	}
	serverURL, err := optionalServerURL()
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, joinConfigErrors(errs)
	}
	return &envConfig{
		serverURL:    serverURL,
		agentName:    agentName,
		agentID:      agentID,
		agentRole:    agentRole,
		outputFormat: resolveOutputFormat(),
		identifier:   fmt.Sprintf("%s-%s-%s", agentName, agentID, agentRole),
	}, nil
}

// identityEnvUnset reports whether NONE of the three LOAM_AGENT_* variables
// is set -- the only state loadOrchestratorConfig's fallback applies to. A
// PARTIALLY configured identity (say LOAM_AGENT_ROLE set but NAME and ID
// forgotten) is deliberately NOT this state: it is a configuration mistake,
// and answering it with the orchestrator's instructions would hide it
// behind a plausible-looking success, telling an agent it may do less than
// its real role allows. Those callers fall through to loadConfig and get
// the same per-variable errors they always did.
func identityEnvUnset() bool {
	return os.Getenv(envAgentName) == "" && os.Getenv(envAgentID) == "" && os.Getenv(envAgentRole) == ""
}

// loadOrchestratorConfig builds the configuration for `loam instructions`
// run with no identity at all: LOAM_SERVER_URL from the environment,
// validated exactly as loadConfig validates it, and the three identity
// values from the well-known orchestrator constants above (loam-hi5o.31).
//
// "No identity" means no LOAM_AGENT_*, NOT no environment. LOAM_SERVER_URL
// stays genuinely required, because the CLI cannot invent where the server
// is and this command makes a real RPC -- so with it unset the command must
// still fail, and the error must name ONLY that variable rather than the
// list of four an unconfigured workspace used to get. There is no list to
// join here: the identity values cannot be missing, so requireServerURL's
// own error is the whole answer and is returned unwrapped. It already
// carries codeUsage, errUsage and errMissingEnv (newUsageCLIError), which
// is exactly what joinConfigErrors would have produced for a single member.
//
// This deliberately reuses the loader seam loam-hi5o.dc2v already
// established -- configForArgs picking a per-command strategy, with
// loadConfig and loadIdentityConfig as the existing two -- rather than
// adding a validation body of its own: every variable it does validate goes
// through the same requireServerURL/resolveOutputFormat helpers the other
// two use, and the only thing new here is which values are required.
func loadOrchestratorConfig() (*envConfig, error) {
	serverURL, err := requireServerURL()
	if err != nil {
		return nil, err
	}
	return &envConfig{
		serverURL:    serverURL,
		agentName:    wellKnownAgentName,
		agentID:      wellKnownAgentID,
		agentRole:    wellKnownAgentRole,
		outputFormat: resolveOutputFormat(),
		identifier:   fmt.Sprintf("%s-%s-%s", wellKnownAgentName, wellKnownAgentID, wellKnownAgentRole),
	}, nil
}

// optionalServerURL validates LOAM_SERVER_URL exactly like requireServerURL
// when it is set, but treats it being unset as valid -- an empty string,
// not an error. loadIdentityConfig is its only caller: whoami needs
// LOAM_SERVER_URL only for --verify, which checks ServerURL() == "" itself
// (see runWhoami), so an absent value must not fail config loading. A SET
// but malformed value is still rejected here rather than deferred to
// --verify time, so an operator who typo'd it sees that immediately.
func optionalServerURL() (string, error) {
	if os.Getenv(envServerURL) == "" {
		return "", nil
	}
	return requireServerURL()
}

// joinConfigErrors combines every validation failure loadConfig/
// loadIdentityConfig collected into one usage-class *cliError: the message
// is every failure's own message, semicolon-joined, so all of them are
// visible in a single run's output. The error is built directly (not via
// newUsageCLIError) because that constructor's cause-deduplication -- skip
// wrapping cause when it already errors.Is-matches the sentinel -- is
// tuned for a single cause and would otherwise collapse this list back down
// to just errUsage: every errs[i] here already carries errUsage itself
// (each came from requireNonEmpty/requireAgentName/requireServerURL, all of
// which build through newUsageCLIError), so errors.Is(errors.Join(errs...),
// errUsage) is already true and that dedup would discard the whole list,
// losing the underlying errMissingEnv/errMalformedEnv markers entirely.
// Joining errUsage alongside errs explicitly instead guarantees
// errors.Is/errors.As can still reach both the umbrella sentinel and every
// individual cause.
func joinConfigErrors(errs []error) error {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return &cliError{
		code:    codeUsage,
		message: strings.Join(msgs, "; "),
		unwrap:  errors.Join(append([]error{errUsage}, errs...)...),
	}
}

// requireNonEmpty returns the value of the named required environment
// variable, or a usage error wrapping errMissingEnv if it is unset or
// empty.
func requireNonEmpty(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", newUsageCLIError(fmt.Sprintf("%s is required but not set", name), errMissingEnv)
	}
	return v, nil
}

// requireAgentName validates LOAM_AGENT_NAME is set and shaped like
// "<first-name>-<last-name>" (see docs/cli-spec.md -> Environment
// Variables).
func requireAgentName() (string, error) {
	v, err := requireNonEmpty(envAgentName)
	if err != nil {
		return "", err
	}
	first, last, ok := strings.Cut(v, "-")
	if !ok || first == "" || last == "" {
		return "", newUsageCLIError(fmt.Sprintf("%s %q: expected <first-name>-<last-name>", envAgentName, v), errMalformedEnv)
	}
	return v, nil
}

// requireServerURL validates LOAM_SERVER_URL is set and parses as an
// absolute URL (scheme and host present) — validated by parsing, never by
// connecting (see docs/cli-spec.md -> Environment Variables).
func requireServerURL() (string, error) {
	v, err := requireNonEmpty(envServerURL)
	if err != nil {
		return "", err
	}
	parsed, parseErr := url.Parse(v)
	if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", newUsageCLIError(fmt.Sprintf("%s %q: must be an absolute URL", envServerURL, v), errMalformedEnv)
	}
	return v, nil
}
