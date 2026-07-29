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
