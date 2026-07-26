package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"connectrpc.com/connect"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// cloneOutput is the JSON success shape for `loam clone` (see
// docs/cli-spec.md -> clone).
type cloneOutput struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

// runCloneCommand implements `loam clone <repo> <branch>`'s body, once
// commands_root.go's runClone has validated that exactly two positional
// arguments were given. Per docs/cli-spec.md -> clone and docs/git-spec.md
// -> The CLI's Role, it: (1) confirms repo is enrolled via
// RepoService.GetRepo -- a NotFound there is classified by
// classifyConnectError into exit 3, matching the bead's "exit 3 unenrolled
// repo"; (2) runs a single-branch `git clone` into ./<repo_name>, the
// clone's sole remote; (3) bootstraps the clone for plain git: user.name /
// user.email set to the agent identity, plus the three Loam-Agent-* headers
// written as http.extraHeader entries so every subsequent plain git
// operation carries them, with no wrapper command and no hook; (4) emits
// {repo, path, branch} through the injected encoder.
func runCloneCommand(ctx context.Context, deps *Deps, repo, branch string) error {
	if repo == "" || branch == "" {
		return newUsageCLIError("clone requires a non-empty repo and branch argument", nil)
	}
	group, name, ok := splitRepo(repo)
	if !ok {
		return newUsageCLIError(fmt.Sprintf("repo %q must be shaped like <group>/<repo_name>", repo), nil)
	}
	if _, err := deps.connect.Repo().GetRepo(ctx, connect.NewRequest(&loamv1.GetRepoRequest{Repo: repo})); err != nil {
		return fmt.Errorf("resolving enrolled repo %s: %w", repo, err)
	}
	dest := "./" + name
	remoteURL := cloneURL(deps.config.ServerURL(), group, name)
	if err := deps.cloner.Clone(ctx, remoteURL, branch, dest); err != nil {
		return newPreconditionFailedError(fmt.Sprintf("cloning %s at branch %q: %s", repo, branch, err), err)
	}
	if err := bootstrapCloneIdentity(ctx, deps.cloner, dest, deps.config); err != nil {
		return fmt.Errorf("bootstrapping clone identity in %s: %w", dest, err)
	}
	return deps.encoder.Encode(cloneOutput{Repo: repo, Path: dest, Branch: branch})
}

// splitRepo splits repo ("<group>/<repo_name>", see docs/cli-spec.md ->
// clone) at its last "/" into group and its final path segment. ok is false
// when repo has no "/", or either half would be empty. repo_name is always
// the final segment, with no override (cli-spec: "bobcob7/doc-server" ->
// "./doc-server").
func splitRepo(repo string) (group, name string, ok bool) {
	idx := strings.LastIndex(repo, "/")
	if idx <= 0 || idx == len(repo)-1 {
		return "", "", false
	}
	return repo[:idx], repo[idx+1:], true
}

// cloneURL composes <serverURL>/git/<group>/<name>.git (see docs/cli-spec.md
// -> clone, docs/git-spec.md -> Endpoint & Protocol), tolerating a trailing
// slash on serverURL so LOAM_SERVER_URL="https://loam.example/" and
// "https://loam.example" compose identically.
func cloneURL(serverURL, group, name string) string {
	return fmt.Sprintf("%s/git/%s/%s.git", strings.TrimRight(serverURL, "/"), group, name)
}

// bootstrapCloneIdentity writes the git config `loam clone` promises: the
// git author (user.name / user.email, see docs/cli-spec.md -> clone: "sets
// the git author ... so commits are attributed") and the three
// Loam-Agent-* identity headers as http.extraHeader entries (see
// docs/git-spec.md -> Identity on Git Operations), reusing the exact header
// name constants connect.go attaches to every Connect RPC so the two never
// drift apart. user.email is "<identifier>@loam" per the bead's design (the
// resolved "<name>-<id>-<role>" identifier, same as whoami and the identity
// headers).
func bootstrapCloneIdentity(ctx context.Context, cloner gitCloner, dest string, cfg Config) error {
	if err := cloner.SetConfig(ctx, dest, "user.name", cfg.AgentName()); err != nil {
		return fmt.Errorf("setting user.name: %w", err)
	}
	if err := cloner.SetConfig(ctx, dest, "user.email", cfg.Identifier()+"@loam"); err != nil {
		return fmt.Errorf("setting user.email: %w", err)
	}
	headers := []struct{ name, value string }{
		{headerAgentName, cfg.AgentName()},
		{headerAgentID, cfg.AgentID()},
		{headerAgentRole, cfg.AgentRole()},
	}
	for _, h := range headers {
		if err := cloner.AddConfig(ctx, dest, "http.extraHeader", fmt.Sprintf("%s: %s", h.name, h.value)); err != nil {
			return fmt.Errorf("adding http.extraHeader for %s: %w", h.name, err)
		}
	}
	return nil
}

// execGitCloner is the real gitCloner, backed by the git binary on PATH.
type execGitCloner struct{}

// newGitCloner constructs the real gitCloner. Unexported: only deps.go's
// NewProductionDeps and this package's own tests call it.
func newGitCloner() gitCloner { return execGitCloner{} }

// Clone implements gitCloner via `git clone --branch branch --single-branch
// url dest`.
func (execGitCloner) Clone(ctx context.Context, url, branch, dest string) error {
	return runGitCommand(ctx, "", "clone", "--branch", branch, "--single-branch", url, dest)
}

// SetConfig implements gitCloner via `git -C dest config key value`.
func (execGitCloner) SetConfig(ctx context.Context, dest, key, value string) error {
	return runGitCommand(ctx, dest, "config", key, value)
}

// AddConfig implements gitCloner via `git -C dest config --add key value`.
func (execGitCloner) AddConfig(ctx context.Context, dest, key, value string) error {
	return runGitCommand(ctx, dest, "config", "--add", key, value)
}

// runGitCommand runs `git -C dir <args...>` (or `git <args...>` when dir is
// empty, e.g. Clone, whose destination does not exist yet). err wraps the
// process's combined stdout+stderr so callers can surface git's own reason
// for a failure (a missing remote branch, an unreachable URL, ...).
func runGitCommand(ctx context.Context, dir string, args ...string) error {
	fullArgs := args
	if dir != "" {
		fullArgs = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out.Bytes()))
	}
	return nil
}
