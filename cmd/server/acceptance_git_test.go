//go:build acceptance

// Git and CLI driver plumbing shared by every step definition: the
// Author/Reviewer actor's two drivers per testing-spec Layer 1's table --
// the compiled loam binary (runLoamCLI) and plain git inside that actor's
// own clone (runPlainGit) -- plus small assertion helpers over both.
package main

import (
	"os"
	"os/exec"
	"strings"
)

// loamCLIResult is one `loam` invocation's observable outcome: the real
// process exit code and real stdout/stderr, mirroring
// cmd/server/clonepush_integration_test.go's own loamCLIResult (a
// different build tag's file, so not reachable here -- reproduced per
// this package's established duplication convention).
type loamCLIResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// runLoamCLI runs the compiled loam binary as a real subprocess -- the
// Author/Reviewer actor driver testing-spec Layer 1's table names -- with
// world's own workspace as its working directory and world's agent
// identity as its LOAM_AGENT_* environment, pointed at the shared
// in-process server.
func (h *acceptanceHarness) runLoamCLI(world *acceptanceWorld, args ...string) loamCLIResult {
	cmd := exec.Command(h.loamBinary, args...)
	cmd.Dir = world.workspace
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"LOAM_SERVER_URL=" + h.server.baseURL,
		"LOAM_AGENT_NAME=" + world.agentName,
		"LOAM_AGENT_ID=" + world.agentID,
		"LOAM_AGENT_ROLE=" + world.agentRole,
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
			stderr.WriteString("\n[harness] loam did not exit normally: " + runErr.Error())
		}
	}
	return loamCLIResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

// runPlainGit runs a real git subcommand in dir with no loam involvement
// -- the plain-git half of the Author/Reviewer actor driver -- returning
// its combined stdout+stderr and error rather than failing immediately, so
// callers can assert on a REJECTED push's exact output.
func runPlainGit(dir string, args ...string) (output string, err error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// gitConfigGet reads back a single --local git config key from dir,
// reporting ok=false if the key is unset -- never trusting a merged
// system/global/local read, which could pass even if this repo's own
// config wrote nothing (the same discipline
// internal/mirrorreconcile/reconcile_test.go's gitConfigGet documents).
func gitConfigGet(dir, key string) (value string, ok bool) {
	cmd := exec.Command("git", "config", "--local", "--get", key)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// gitConfigGetAll reads back every value of a --local, possibly-multi-
// valued git config key (e.g. http.extraheader) from dir.
func gitConfigGetAll(dir, key string) []string {
	cmd := exec.Command("git", "config", "--local", "--get-all", key)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// gitLocalBranches returns dir's local branch names, short form.
func gitLocalBranches(dir string) []string {
	cmd := exec.Command("git", "for-each-ref", "--format=%(refname:short)", "refs/heads")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// gitRemotes returns dir's configured remote names.
func gitRemotes(dir string) []string {
	cmd := exec.Command("git", "remote")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// mirrorRefSubject returns the subject line of ref's current commit inside
// the bare mirror at mirrorDir, addressed via --git-dir (never -C, so this
// never accidentally walks upward past mirrorDir into some enclosing
// repository -- the same reasoning internal/mirrorreconcile documents for
// its own git invocations).
func mirrorRefSubject(mirrorDir, ref string) (string, error) {
	cmd := exec.Command("git", "--git-dir="+mirrorDir, "log", "-1", "--format=%s", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// mirrorRefSHA returns ref's current commit SHA inside the bare mirror at
// mirrorDir.
func mirrorRefSHA(mirrorDir, ref string) (string, error) {
	cmd := exec.Command("git", "--git-dir="+mirrorDir, "rev-parse", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// cloneHeadSHA returns clonePath's current HEAD commit SHA.
func cloneHeadSHA(clonePath string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = clonePath
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
