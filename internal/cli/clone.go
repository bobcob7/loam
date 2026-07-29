package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"connectrpc.com/connect"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/refnames"
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
// clone's sole remote, passing the three Loam-Agent-* identity headers as
// clone-time --config arguments (see identityHeaders) so even the clone's
// OWN initial fetch is authorized -- httpauth.Auth.GitIdentity 403s any
// /git/* request missing them, and headers written into dest's config only
// AFTER Clone returns would be too late for that very first request; (3)
// bootstraps the rest of the clone for plain git: user.name / user.email
// set to the agent identity, so commits are attributed, AND the two
// refspecs that map a work branch's bare name to its reserved ref path in
// the mirror (see bootstrapWorkBranchRefspecs); (4) emits {repo, path,
// branch} through the injected encoder.
func runCloneCommand(ctx context.Context, deps *Deps, repo, branch string) error {
	if repo == "" || branch == "" {
		return newUsageCLIError("clone requires a non-empty repo and branch argument", nil)
	}
	group, name, ok := splitRepo(repo)
	if !ok {
		return newUsageCLIError(fmt.Sprintf("repo %q must be shaped like <group>/<repo_name>", repo), nil)
	}
	repoResp, err := deps.connect.Repo().GetRepo(ctx, connect.NewRequest(&loamv1.GetRepoRequest{Repo: repo}))
	if err != nil {
		return fmt.Errorf("resolving enrolled repo %s: %w", repo, err)
	}
	dest := "./" + name
	remoteURL := cloneURL(deps.config.ServerURL(), group, name)
	cloneBranch := cloneBranchFor(branch, repoResp.Msg.GetTargetBranches())
	if err := deps.cloner.Clone(ctx, remoteURL, cloneBranch, dest, identityHeaders(deps.config)); err != nil {
		return newPreconditionFailedError(fmt.Sprintf("cloning %s at branch %q: %s", repo, branch, err), err)
	}
	if cloneBranch != branch {
		if err := deps.cloner.RenameBranch(ctx, dest, cloneBranch, branch); err != nil {
			return fmt.Errorf("renaming the cloned branch %s to %s in %s: %w", cloneBranch, branch, dest, err)
		}
	}
	if err := bootstrapCloneIdentity(ctx, deps.cloner, dest, deps.config); err != nil {
		return fmt.Errorf("bootstrapping clone identity in %s: %w", dest, err)
	}
	if err := bootstrapWorkBranchRefspecs(ctx, deps.cloner, dest); err != nil {
		return fmt.Errorf("bootstrapping work-branch refspecs in %s: %w", dest, err)
	}
	return deps.encoder.Encode(cloneOutput{Repo: repo, Path: dest, Branch: branch})
}

// cloneBranchFor decides how `git clone --branch` must spell branch. A
// TARGET branch is a mirrored ref and is spelled exactly as given; anything
// else is a work branch, whose ref lives under refnames.ReservedNamespace
// and which --branch can only reach by its "loam-reserved/<name>" short
// form (see refnames.CloneBranch).
//
// targetBranches is GetRepoResponse.target_branches, which
// runCloneCommand's own enrollment check has ALREADY fetched -- so this
// classification costs no extra request and consults the registry that
// actually owns the answer, rather than pattern-matching the name or
// probing the remote. A branch that is neither a target nor an existing
// work branch fails in git, with git's own "Remote branch ... not found"
// reason, which docs/cli-spec.md maps to exit 2.
func cloneBranchFor(branch string, targetBranches []string) string {
	for _, target := range targetBranches {
		if target == branch {
			return branch
		}
	}
	return refnames.CloneBranch(branch)
}

// identityHeaders builds the three "<Header>: <value>" strings (see
// docs/git-spec.md -> Identity on Git Operations) that authorize every
// /git/* request from this clone, reusing connect.go's exact header name
// constants so the git-transport headers can never drift from the ones
// attached to every Connect RPC.
func identityHeaders(cfg Config) []string {
	return []string{
		fmt.Sprintf("%s: %s", headerAgentName, cfg.AgentName()),
		fmt.Sprintf("%s: %s", headerAgentID, cfg.AgentID()),
		fmt.Sprintf("%s: %s", headerAgentRole, cfg.AgentRole()),
	}
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

// bootstrapCloneIdentity writes the remaining git config `loam clone`
// promises beyond the identity headers Clone already passed at clone time
// (see identityHeaders and Clone's doc comment): the git author (user.name
// / user.email, see docs/cli-spec.md -> clone: "sets the git author ... so
// commits are attributed"). user.email is "<identifier>@loam" per the
// bead's design (the resolved "<name>-<id>-<role>" identifier, same as
// whoami and the identity headers).
func bootstrapCloneIdentity(ctx context.Context, cloner gitCloner, dest string, cfg Config) error {
	if err := cloner.SetConfig(ctx, dest, "user.name", cfg.AgentName()); err != nil {
		return fmt.Errorf("setting user.name: %w", err)
	}
	if err := cloner.SetConfig(ctx, dest, "user.email", cfg.Identifier()+"@loam"); err != nil {
		return fmt.Errorf("setting user.email: %w", err)
	}
	return nil
}

// bootstrapWorkBranchRefspecs writes the two refspecs that make PLAIN git
// reach a work branch, whose ref in the mirror lives under Loam's reserved,
// server-owned namespace (refs/heads/loam-reserved/<name>) rather than at
// refs/heads/<name> -- see internal/refnames for why the namespace exists
// at all.
//
// This is the one place `loam clone` stops being purely a convenience.
// docs/git-spec.md -> "The CLI's Role" describes clone as bootstrapping
// "URL, identity, config" and then getting out of the way, and that is
// still what this is -- but without these two lines `git push origin
// wb-9c2f1a` from the clone targets refs/heads/wb-9c2f1a, an unregistered
// ref internal/refpolicy rejects. A HAND-ROLLED CLONE THEREFORE CANNOT
// PUSH, which is a genuine behavioural change and is stated as such in
// docs/git-spec.md.
//
//   - remote.origin.push (SetConfig, single-valued): git-push(1) documents
//     that a command-line refspec with no ":<dst>" resolves its
//     destination through this key, so `git push origin wb-9c2f1a` lands
//     on the reserved path. Verified against real git 2.50.1 by
//     TestExecGitCloner_RefspecsMakePlainPushReachTheReservedNamespace.
//   - remote.origin.fetch (AddConfig, multi-valued -- see AddConfig's own
//     doc comment): brings work branches down as refs/remotes/origin/<name>
//     under their bare names, which is what a reviewer's clone needs to
//     check one out at all.
func bootstrapWorkBranchRefspecs(ctx context.Context, cloner gitCloner, dest string) error {
	if err := cloner.SetConfig(ctx, dest, "remote.origin.push", refnames.ClientPushRefspec); err != nil {
		return fmt.Errorf("setting remote.origin.push: %w", err)
	}
	if err := cloner.AddConfig(ctx, dest, "remote.origin.fetch", refnames.ClientFetchRefspec); err != nil {
		return fmt.Errorf("adding remote.origin.fetch: %w", err)
	}
	return nil
}

// execGitCloner is the real gitCloner, backed by the git binary on PATH.
type execGitCloner struct{}

// newGitCloner constructs the real gitCloner. Unexported: only deps.go's
// NewProductionDeps and this package's own tests call it.
func newGitCloner() gitCloner { return execGitCloner{} }

// Clone implements gitCloner via `git clone --branch branch --single-branch
// --config http.extraHeader=<h> (once per entry in headers) url dest`.
// Passing the headers as clone-time --config arguments, rather than writing
// them into dest's git config once Clone returns, is required: git issues
// the upload-pack info/refs GET before dest exists at all, so config
// written afterward never reaches that request. --config persists into
// dest/.git/config exactly like a subsequent `config --add` would (proven
// by TestExecGitCloner_Clone_HeadersPersistIntoRealGitConfig), so the
// clone's later fetches/pushes still carry the same headers with no
// separate write.
func (execGitCloner) Clone(ctx context.Context, url, branch, dest string, headers []string) error {
	args := []string{"clone", "--branch", branch, "--single-branch"}
	for _, h := range headers {
		args = append(args, "--config", "http.extraHeader="+h)
	}
	args = append(args, url, dest)
	return runGitCommand(ctx, "", args...)
}

// SetConfig implements gitCloner via `git -C dest config key value`.
func (execGitCloner) SetConfig(ctx context.Context, dest, key, value string) error {
	return runGitCommand(ctx, dest, "config", key, value)
}

// AddConfig implements gitCloner via `git -C dest config --add key value`.
func (execGitCloner) AddConfig(ctx context.Context, dest, key, value string) error {
	return runGitCommand(ctx, dest, "config", "--add", key, value)
}

// RenameBranch implements gitCloner via `git -C dest branch -m from to`.
func (execGitCloner) RenameBranch(ctx context.Context, dest, from, to string) error {
	return runGitCommand(ctx, dest, "branch", "-m", from, to)
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
