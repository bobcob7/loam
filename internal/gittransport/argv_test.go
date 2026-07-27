package gittransport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installArgvSpyGit puts a fake "git" executable first on PATH that,
// instead of running anything, records the exact argv and environment it
// was invoked with to captureFile, then exits 0. This is the only way to
// observe precisely what Transport hands the real git binary -- argv is
// exactly what `ps` would show any other process on the box, and this
// spy proves no secret ever reaches it, without needing root or a real
// system gitconfig to do so.
//
// Deliberately not t.Parallel(): t.Setenv (needed to make PATH resolve
// to the spy) panics if the test or an ancestor calls t.Parallel, since
// env vars are process-global.
func installArgvSpyGit(t *testing.T) (captureFile string) {
	t.Helper()
	binDir := t.TempDir()
	captureFile = filepath.Join(t.TempDir(), "capture.txt")
	script := "#!/bin/sh\n" +
		"{\n" +
		"  echo ARGV_BEGIN\n" +
		"  for a in \"$@\"; do printf '%s\\n' \"$a\"; done\n" +
		"  echo ARGV_END\n" +
		"  echo ENV_BEGIN\n" +
		"  env\n" +
		"  echo ENV_END\n" +
		"} > " + shellQuote(captureFile) + "\n" +
		"exit 0\n"
	spyPath := filepath.Join(binDir, "git")
	require.NoError(t, os.WriteFile(spyPath, []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return captureFile
}

// shellQuote wraps s in single quotes for embedding in a /bin/sh script,
// escaping any single quote s itself contains. Test-only fixture
// plumbing, not a general-purpose shell-safety helper.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseSpyCapture splits a captured argv+env dump into its two sections.
func parseSpyCapture(t *testing.T, raw string) (argv []string, env []string) {
	t.Helper()
	section := ""
	for _, line := range strings.Split(raw, "\n") {
		switch line {
		case "ARGV_BEGIN":
			section = "argv"
		case "ARGV_END", "ENV_END":
			section = ""
		case "ENV_BEGIN":
			section = "env"
		default:
			if section == "argv" {
				argv = append(argv, line)
			} else if section == "env" {
				env = append(env, line)
			}
		}
	}
	return argv, env
}

// TestTransport_RemovesItsTempHomeOnEveryPath pins that the per-invocation
// HOME/XDG/GIT_CONFIG_GLOBAL scratch dir is cleaned up. Deleting run's
// defer os.RemoveAll leaves every other test green, but the mirrorsync
// scheduler fetches on every tick, so an un-removed dir per invocation is
// unbounded growth on a long-running server.
//
// TMPDIR is redirected so this test owns its own temp parent and cannot be
// confounded by dirs other (parallel) tests create. Deliberately not
// t.Parallel(): t.Setenv.
func TestTransport_RemovesItsTempHomeOnEveryPath(t *testing.T) {
	tmpParent := t.TempDir()
	t.Setenv("TMPDIR", tmpParent)
	installLeakyGit(t)
	transport := New(&staticCredentialSource{token: "tmpdir-cleanup-token"}, newGitCredsConverter(), testLogger())

	_, err := transport.Fetch(t.Context(), "forge.example.invalid", t.TempDir(),
		"https://forge.example.invalid/acme/widgets.git", []string{"+refs/heads/*:refs/heads/*"})
	require.Error(t, err, "the fake git exits 128, so this exercises the FAILURE path")

	left, globErr := filepath.Glob(filepath.Join(tmpParent, "loam-gittransport-*"))
	require.NoError(t, globErr)
	assert.Empty(t, left, "run must remove its per-invocation scratch HOME even when git fails")
}

// installLeakyGit puts a fake "git" first on PATH that echoes its own
// injected auth header to stderr and fails. Real git never does this --
// it does not print the Authorization header on a 401 -- which is exactly
// why the scrubber's coverage cannot be proven against a real failed
// fetch: the encoded value simply never reaches that output, so the
// assertion would pass whether or not scrubbing handled it. This fake
// makes git behave as badly as the scrubber is meant to defend against.
func installLeakyGit(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo \"leaked header: $GIT_CONFIG_VALUE_0\" >&2\n" +
		"exit 128\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestTransport_ScrubsHeaderGitEchoedIntoOutput pins the production call
// site's secret list, not just scrubSecrets' own behaviour. Dropping
// authHeaderValue from run's scrubSecrets call leaves every other test in
// this package green, because nothing else ever puts the base64 into git's
// output. The header carries base64(user:token), which decodes straight
// back to the credential.
//
// Deliberately not t.Parallel(): t.Setenv.
func TestTransport_ScrubsHeaderGitEchoedIntoOutput(t *testing.T) {
	const token = "header-echoed-by-a-badly-behaved-git"
	encoded := basicAuthValue(t, token)
	installLeakyGit(t)
	transport := New(&staticCredentialSource{token: token}, newGitCredsConverter(), testLogger())
	_, err := transport.Fetch(t.Context(), "forge.example.invalid", t.TempDir(),
		"https://forge.example.invalid/acme/widgets.git", []string{"+refs/heads/*:refs/heads/*"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "leaked header:", "precondition: the fake git must have echoed its header into the captured output")
	assert.NotContains(t, err.Error(), encoded, "run must scrub the base64 header value, not only the plaintext token")
	assert.NotContains(t, err.Error(), token, "run must scrub the plaintext token")
}

func TestTransport_NeverPutsCredentialsInArgv(t *testing.T) {
	requireGit(t)
	captureFile := installArgvSpyGit(t)
	const token = "argv-must-never-see-me"
	const username = "loam"
	credStore := &credentialSourceMock{
		GetByHostFunc: func(_ context.Context, _ string) (credentialstore.Credential, error) {
			return credentialstore.Credential{Token: token}, nil
		},
	}
	gitCreds := &gitCredentialConverterMock{
		GitCredentialsFunc: func(_ context.Context, tok string) (string, string, error) {
			return username, tok, nil
		},
	}
	transport := New(credStore, gitCreds, testLogger())
	const upstreamURL = "https://forge.example.invalid/acme/widgets.git"
	const mirrorDir = "/tmp/does-not-need-to-exist-for-this-spy"
	_, _ = transport.Fetch(t.Context(), "forge.example.invalid", mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	raw, err := os.ReadFile(captureFile)
	require.NoError(t, err, "the argv spy must have run and recorded a capture")
	argv, env := parseSpyCapture(t, string(raw))
	joinedArgv := strings.Join(argv, "\x00")
	assert.NotContains(t, joinedArgv, token, "argv (visible to every process on the box via ps) must never contain the token")
	assert.NotContains(t, joinedArgv, "Authorization", "argv must never carry the Authorization header itself")
	assert.NotContains(t, joinedArgv, basicAuthValue(t, token), "argv must not carry the base64 header value either -- it is trivially reversible, and a plaintext-only check would miss it")
	assert.Contains(t, argv, upstreamURL, "the upstream URL must be passed through exactly as given")
	assert.Contains(t, argv, "-c", "credential.helper must be cleared via -c (a bare flag, no secret)")
	assert.Contains(t, argv, "credential.helper=")
	headerInEnv := false
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_CONFIG_VALUE_0=Authorization: Basic ") {
			headerInEnv = true
		}
	}
	// The header IS expected in the environment -- env is not visible via
	// `ps` to other users the way argv is -- so this is the header's one
	// legitimate home.
	assert.True(t, headerInEnv, "the Authorization header must be injected via GIT_CONFIG_VALUE_0 in the environment")
}

func TestTransport_IsolationEnvOverridesAmbientHome(t *testing.T) {
	requireGit(t)
	captureFile := installArgvSpyGit(t)
	ambientHome := t.TempDir()
	t.Setenv("HOME", ambientHome)
	credStore := &credentialSourceMock{
		GetByHostFunc: func(_ context.Context, _ string) (credentialstore.Credential, error) {
			return credentialstore.Credential{Token: "irrelevant-for-this-test"}, nil
		},
	}
	gitCreds := &gitCredentialConverterMock{
		GitCredentialsFunc: func(_ context.Context, tok string) (string, string, error) {
			return "loam", tok, nil
		},
	}
	transport := New(credStore, gitCreds, testLogger())
	_, _ = transport.Fetch(t.Context(), "forge.example.invalid", "/tmp/irrelevant-mirror", "https://forge.example.invalid/a/b.git", nil)
	raw, err := os.ReadFile(captureFile)
	require.NoError(t, err)
	_, env := parseSpyCapture(t, string(raw))
	homeLine := ""
	nosystemSeen := false
	globalOverridden := false
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") {
			homeLine = e
		}
		if e == "GIT_CONFIG_NOSYSTEM=1" {
			nosystemSeen = true
		}
		if strings.HasPrefix(e, "GIT_CONFIG_GLOBAL=") && e != "GIT_CONFIG_GLOBAL=" {
			globalOverridden = true
		}
	}
	require.NotEmpty(t, homeLine, "HOME must be set in the subprocess environment")
	assert.NotEqual(t, "HOME="+ambientHome, homeLine, "the subprocess must never inherit the ambient HOME -- it must run under an isolated, per-invocation HOME")
	assert.True(t, nosystemSeen, "GIT_CONFIG_NOSYSTEM=1 must always be set")
	assert.True(t, globalOverridden, "GIT_CONFIG_GLOBAL must always be redirected to a path inside the isolated HOME")
}
