package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loamBinary is the path to the loam binary TestMain compiles once for the
// whole test process (see TestMain below). Package-level mutable state is
// against this repo's Go standards for production code, but there is no
// clean alternative for sharing one compiled binary across every test in
// this file — the same tradeoff sync.Once-guarded package state makes
// elsewhere in Go test suites.
var loamBinary string

// TestMain compiles cmd/loam once before any test in this file runs. The
// brief for this bead requires exercising the COMPILED BINARY for anything
// user-visible — real exit codes, real stdout — because a unit test
// asserting a mapper's return value is not the same as the process actually
// exiting with that code.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loam-cli-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	loamBinary = filepath.Join(dir, "loam")
	build := exec.Command("go", "build", "-o", loamBinary, ".")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "building loam binary: %v\n%s", buildErr, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// loamResult is one invocation's observable outcome: the real process exit
// code and real stdout/stderr, exactly what an agent driving this CLI sees.
type loamResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// runLoam invokes the compiled binary with args and env, capturing its
// real exit code and stdout/stderr — never a simulated in-process call.
func runLoam(t *testing.T, env []string, args ...string) loamResult {
	t.Helper()
	cmd := exec.Command(loamBinary, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		require.True(t, ok, "loam exited abnormally rather than with a normal exit code: %v (stderr: %s)", runErr, stderr.String())
		exitCode = exitErr.ExitCode()
	}
	return loamResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

// validEnv is a complete, valid LOAM_* environment (see docs/cli-spec.md ->
// Environment Variables), the baseline every test below starts from and
// mutates. PATH is preserved so the process can still resolve any
// subprocess it shells out to (e.g. git, for workspace inference).
func validEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"LOAM_SERVER_URL=https://loam.example",
		"LOAM_AGENT_NAME=ada-lovelace",
		"LOAM_AGENT_ID=7",
		"LOAM_AGENT_ROLE=reviewer",
	}
}

// TestLoam_NoArgs_ExitsUsageWithStructuredJSONError proves the top-level,
// agent-facing contract end to end: invoked with a fully valid environment
// but no command, the real process exits 2 and writes a structured JSON
// error with code "usage" to real stdout (see docs/cli-spec.md -> Exit
// Codes & Errors).
func TestLoam_NoArgs_ExitsUsageWithStructuredJSONError(t *testing.T) {
	t.Parallel()
	result := runLoam(t, validEnv())
	assert.Equal(t, 2, result.exitCode)
	assert.Contains(t, result.stdout, `"code":"usage"`)
	assert.Empty(t, result.stderr)
}

// TestLoam_UnknownCommand_ExitsUsage proves an unrecognized command name
// also exits 2 through the real binary, not just the in-process Router.
func TestLoam_UnknownCommand_ExitsUsage(t *testing.T) {
	t.Parallel()
	result := runLoam(t, validEnv(), "bogus-command")
	assert.Equal(t, 2, result.exitCode)
	assert.Contains(t, result.stdout, `"code":"usage"`)
}

// TestLoam_MissingRequiredEnvVar_ExitsUsage proves a missing required
// LOAM_* variable fails Deps construction itself (before any command
// dispatch) with a real exit code 2 and a structured error naming the
// cause — this is the exact NewProductionDeps path loam-qdr's wiring
// replaced NewPlaceholderDeps with.
func TestLoam_MissingRequiredEnvVar_ExitsUsage(t *testing.T) {
	t.Parallel()
	env := make([]string, 0, len(validEnv()))
	for _, kv := range validEnv() {
		if strings.HasPrefix(kv, "LOAM_AGENT_ROLE=") {
			continue
		}
		env = append(env, kv)
	}
	result := runLoam(t, env, "whoami")
	assert.Equal(t, 2, result.exitCode)
	assert.Contains(t, result.stdout, `"code":"usage"`)
	assert.Contains(t, result.stdout, "LOAM_AGENT_ROLE")
}

// TestLoam_ValidEnv_Whoami_ReportsIdentityWithoutContactingTheServer proves
// two things at once through the real binary.
//
// First, the full production wiring — real config, real workspace resolver,
// real Connect clients, real error mapper, real encoder — constructs
// successfully from a valid environment and dispatch actually happens: if
// NewProductionDeps' wiring were broken (e.g. newConnectClient failing on a
// valid config), this would be a construction-time usage/internal error
// instead of a rendered identity.
//
// Second, and this is the acceptance criterion "whoami works without
// contacting the server" (features/instructions.feature): validEnv()'s
// LOAM_SERVER_URL points at https://loam.example, which resolves to
// nothing. A whoami that made any RPC would fail here. Exit 0 with the
// identity on stdout is only reachable if it made none.
//
// The identifier is asserted as the full "<name>-<id>-<role>" string, not
// just checked for presence: reporting the bare agent name in that field
// was the P0 in loam-ppb, and that regression would still satisfy a
// looser assertion.
func TestLoam_ValidEnv_Whoami_ReportsIdentityWithoutContactingTheServer(t *testing.T) {
	t.Parallel()
	result := runLoam(t, validEnv(), "whoami")
	require.Equal(t, 0, result.exitCode, "stdout: %s stderr: %s", result.stdout, result.stderr)
	assert.Empty(t, result.stderr)
	var out struct {
		Name       string `json:"name"`
		ID         string `json:"id"`
		Role       string `json:"role"`
		Identifier string `json:"identifier"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.stdout), &out), "whoami must write a single JSON object: %s", result.stdout)
	assert.Equal(t, "ada-lovelace", out.Name)
	assert.Equal(t, "7", out.ID)
	assert.Equal(t, "reviewer", out.Role)
	assert.Equal(t, "ada-lovelace-7-reviewer", out.Identifier)
}

// TestLoam_ValidEnv_Instructions_ServerUnreachable_ExitsInternal proves the
// other half of the orientation pair through the real binary: `instructions`
// IS server-backed (loam.v1.MetaService.GetInstructions), so against
// validEnv()'s unreachable LOAM_SERVER_URL it fails — and fails with exit 1,
// the "server is unreachable" code docs/cli-spec.md -> instructions ->
// Errors pins. Run next to the whoami test above, the pair is the real
// evidence for the split: same environment, same binary, one command
// succeeds locally and the other cannot.
func TestLoam_ValidEnv_Instructions_ServerUnreachable_ExitsInternal(t *testing.T) {
	t.Parallel()
	result := runLoam(t, validEnv(), "instructions")
	assert.Equal(t, 1, result.exitCode)
	assert.Contains(t, result.stdout, `"code":"internal"`)
}

// TestLoam_HumanOutputFormat_PrintsPlainMessageNotJSON proves
// LOAM_OUTPUT_FORMAT=human changes real stdout rendering end to end (see
// docs/cli-spec.md -> Output: "In human output mode the CLI prints a plain
// message instead of the structured object").
func TestLoam_HumanOutputFormat_PrintsPlainMessageNotJSON(t *testing.T) {
	t.Parallel()
	env := append(validEnv(), "LOAM_OUTPUT_FORMAT=human")
	result := runLoam(t, env)
	assert.Equal(t, 2, result.exitCode)
	assert.NotContains(t, result.stdout, "{")
	assert.NotContains(t, result.stdout, `"code"`)
	assert.NotEmpty(t, strings.TrimSpace(result.stdout))
}

// emptyEnv is the opposite of validEnv(): no LOAM_* variable set at all,
// PATH preserved so the compiled binary can still start. The loam-dc2v/
// loam-q0ek acceptance criteria are specifically about a machine with "no
// LOAM_* environment at all", so every test below that claims to prove one
// of them uses this, never validEnv() with pieces removed.
func emptyEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH")}
}

// TestLoam_Help_EmptyEnvironment_ExitsZeroWithUsage proves the bare `loam
// help` form: loam-dc2v defect 2 ("help is gated behind config") is fixed
// when this succeeds on emptyEnv() -- before the fix this failed with a
// LOAM_SERVER_URL usage error, exit 2.
func TestLoam_Help_EmptyEnvironment_ExitsZeroWithUsage(t *testing.T) {
	t.Parallel()
	result := runLoam(t, emptyEnv(), "help")
	require.Equal(t, 0, result.exitCode, "stdout: %s stderr: %s", result.stdout, result.stderr)
	assert.Empty(t, result.stderr)
	assert.NotContains(t, result.stdout, `"code"`, "help must not be a JSON error envelope")
	assert.Contains(t, result.stdout, "whoami")
	assert.Contains(t, result.stdout, "instructions")
}

// TestLoam_DoubleDashHelp_TopLevel_EmptyEnvironment_ExitsZeroWithUsage
// proves `loam --help` specifically -- loam-q0ek's own reproduction of this
// exact route: "unknown command \"--help\"", exit 2, before the fix.
func TestLoam_DoubleDashHelp_TopLevel_EmptyEnvironment_ExitsZeroWithUsage(t *testing.T) {
	t.Parallel()
	result := runLoam(t, emptyEnv(), "--help")
	require.Equal(t, 0, result.exitCode, "stdout: %s stderr: %s", result.stdout, result.stderr)
	assert.NotContains(t, result.stdout, `"code":"usage"`)
	assert.Contains(t, result.stdout, "work")
}

// TestLoam_SubcommandHelp_EmptyEnvironment_ExitsZeroWithUsage proves
// `loam work start --help` -- loam-q0ek's other named reproduction
// ("pflag: help requested", exit 2) -- now exits 0 with real usage text
// including the flag/positional command name, and crucially requires no
// LOAM_* configuration at all to get there.
func TestLoam_SubcommandHelp_EmptyEnvironment_ExitsZeroWithUsage(t *testing.T) {
	t.Parallel()
	result := runLoam(t, emptyEnv(), "work", "start", "--help")
	require.Equal(t, 0, result.exitCode, "stdout: %s stderr: %s", result.stdout, result.stderr)
	assert.NotContains(t, result.stdout, `"code"`)
	assert.NotContains(t, result.stdout, "pflag: help requested", "pflag's internal sentinel text must never reach the user")
	assert.Contains(t, result.stdout, "work start")
}

// TestLoam_SubcommandHelp_WithFlags_EmptyEnvironment_RendersFlagUsage
// proves a leaf WITH registered flags (work set's --title) renders that
// flag's usage text specifically, not just a bare summary line -- the
// "flag/usage help from the pflag FlagSet" half of the chosen design
// (option (c), see loam-q0ek's notes).
func TestLoam_SubcommandHelp_WithFlags_EmptyEnvironment_RendersFlagUsage(t *testing.T) {
	t.Parallel()
	result := runLoam(t, emptyEnv(), "work", "set", "--help")
	require.Equal(t, 0, result.exitCode, "stdout: %s stderr: %s", result.stdout, result.stderr)
	assert.Contains(t, result.stdout, "--title")
}

// TestLoam_Whoami_NoServerURL_ExitsZeroAndReportsIdentity is loam-dc2v
// defect 3's own reproduction, made to pass: identity vars set, but
// LOAM_SERVER_URL entirely unset. Before the fix this exited 2 with
// {"error":{"code":"usage","message":"LOAM_SERVER_URL is required but not
// set"}}, contradicting whoami's own "Local only -- no server call"
// contract (docs/cli-spec.md line 139) and its own doc comment.
func TestLoam_Whoami_NoServerURL_ExitsZeroAndReportsIdentity(t *testing.T) {
	t.Parallel()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"LOAM_AGENT_NAME=grace-hopper",
		"LOAM_AGENT_ID=1",
		"LOAM_AGENT_ROLE=author",
	}
	result := runLoam(t, env, "whoami")
	require.Equal(t, 0, result.exitCode, "stdout: %s stderr: %s", result.stdout, result.stderr)
	assert.Empty(t, result.stderr)
	var out struct {
		Name       string `json:"name"`
		ID         string `json:"id"`
		Role       string `json:"role"`
		Identifier string `json:"identifier"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.stdout), &out), "stdout: %s", result.stdout)
	assert.Equal(t, "grace-hopper", out.Name)
	assert.Equal(t, "grace-hopper-1-author", out.Identifier)
}

// TestLoam_Whoami_NoServerURL_VerifyFlag_IsUsageErrorNotPanic proves the
// "two traps" warning: whoami --verify still requires LOAM_SERVER_URL at
// point of use (it just shipped independently of this bead), and the
// restructuring that lets bare whoami skip it must not leave --verify
// dialing a nil Connect client. A nil-pointer panic would show up here as
// a non-zero exit with no JSON error body and something on stderr; this
// asserts the real contract instead — a clean, structured usage error.
func TestLoam_Whoami_NoServerURL_VerifyFlag_IsUsageErrorNotPanic(t *testing.T) {
	t.Parallel()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"LOAM_AGENT_NAME=grace-hopper",
		"LOAM_AGENT_ID=1",
		"LOAM_AGENT_ROLE=author",
	}
	result := runLoam(t, env, "whoami", "--verify")
	assert.Equal(t, 2, result.exitCode, "stdout: %s stderr: %s", result.stdout, result.stderr)
	assert.Empty(t, result.stderr, "a nil Connect client dereference would panic to stderr, not report a clean usage error")
	assert.Contains(t, result.stdout, `"code":"usage"`)
	assert.Contains(t, result.stdout, "LOAM_SERVER_URL")
}

// TestLoam_MissingEverything_ReportsEveryVariableInOneRun is loam-dc2v
// defect 1's end-to-end proof: a completely unconfigured run must name
// every missing variable in its single structured error, not just
// LOAM_SERVER_URL (the first one loadConfig used to check before this
// fix).
//
// The vehicle is `work list` rather than `instructions`, which it used to
// be: loam-hi5o.31 made `instructions` the one command that no longer
// requires the three LOAM_AGENT_* variables at all (it falls back to the
// well-known orchestrator identity), so naming them in ITS error would now
// be the bug. `work list` still requires all four, and the property under
// test here -- report every missing variable in ONE run, never one per run
// -- is unchanged for it and for every other command.
func TestLoam_MissingEverything_ReportsEveryVariableInOneRun(t *testing.T) {
	t.Parallel()
	result := runLoam(t, emptyEnv(), "work", "list")
	assert.Equal(t, 2, result.exitCode)
	assert.Contains(t, result.stdout, `"code":"usage"`)
	for _, name := range []string{"LOAM_SERVER_URL", "LOAM_AGENT_NAME", "LOAM_AGENT_ID", "LOAM_AGENT_ROLE"} {
		assert.Contains(t, result.stdout, name, "a fully unconfigured run must name every missing variable in one output")
	}
}

// TestLoam_Instructions_MissingEverything_NamesOnlyServerURL is
// loam-hi5o.31 acceptance criterion 13's failing half, run against the real
// binary with a genuinely empty environment. The assertion that matters is
// the NEGATIVE one: the old message named all four variables, so a check
// for "contains LOAM_SERVER_URL" alone would pass against the exact
// behaviour this replaced. Three of the four are no longer required for
// this command, and naming them would send an operator to configure things
// that would change nothing.
func TestLoam_Instructions_MissingEverything_NamesOnlyServerURL(t *testing.T) {
	t.Parallel()
	result := runLoam(t, emptyEnv(), "instructions")
	assert.Equal(t, 2, result.exitCode)
	assert.Contains(t, result.stdout, `"code":"usage"`)
	assert.Contains(t, result.stdout, "LOAM_SERVER_URL")
	for _, name := range []string{"LOAM_AGENT_NAME", "LOAM_AGENT_ID", "LOAM_AGENT_ROLE"} {
		assert.NotContainsf(t, result.stdout, name, "%s is not required by `instructions` and must not be named", name)
	}
}

// TestLoam_Help_StillWorksWithNoEnvironmentAtAll guards the other half of
// the cold-start story loam-hi5o.31 leaves in place: `loam help` needs no
// LOAM_* variable at all -- not even LOAM_SERVER_URL, which `instructions`
// does need -- and must still name `instructions` as the authority on what
// a role may do. Cold start is now two steps (help lists everything,
// instructions gives the orchestrator's guidance) and both halves have to
// keep working.
func TestLoam_Help_StillWorksWithNoEnvironmentAtAll(t *testing.T) {
	t.Parallel()
	result := runLoam(t, emptyEnv(), "help")
	assert.Equal(t, 0, result.exitCode, "stdout: %s stderr: %s", result.stdout, result.stderr)
	assert.Contains(t, result.stdout, "instructions")
}

// TestMainWiring_NoPlaceholderCollaborators is loam-qdr's own acceptance
// criterion made concrete: main.go must wire the real Config, OutputEncoder,
// ErrorMapper, WorkspaceResolver, and ConnectClient, with no reference to
// the deleted internal/cli placeholder collaborators
// (NewPlaceholderDeps/unresolvedWorkspace/noopConnectClient), and
// internal/cli/stubs.go itself must be gone.
func TestMainWiring_NoPlaceholderCollaborators(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("main.go")
	require.NoError(t, err)
	for _, forbidden := range []string{"NewPlaceholderDeps", "unresolvedWorkspace", "noopConnectClient", "Placeholder"} {
		assert.NotContains(t, string(src), forbidden, "main.go must not reference the deleted placeholder collaborator %q (loam-qdr)", forbidden)
	}
	assert.Contains(t, string(src), "NewProductionDeps", "main.go must wire the real collaborators via cli.NewProductionDeps")
	_, statErr := os.Stat(filepath.Join("..", "..", "internal", "cli", "stubs.go"))
	assert.True(t, os.IsNotExist(statErr), "internal/cli/stubs.go must be deleted (loam-qdr)")
}
