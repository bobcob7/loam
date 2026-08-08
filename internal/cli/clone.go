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
//
// Target/BaseSHA/HeadSHA were added by loam-hwru, and they are not
// decoration. Before it, a clone reported only where it put the files: no
// origin/<target>, no merge base, and no record anywhere of the commit the
// branch was cut from. `git diff origin/main...HEAD` failed with "unknown
// revision", and the natural recovery -- eyeball `git log` and guess where
// the branch starts -- produces a diff that LOOKS right whether the guess
// was right or not. Too far back and a reviewer reviews other people's
// merged work as the author's; too far forward and it silently skips
// commits. Naming the three commits here means the guess is never
// necessary and, if someone guesses anyway, it can be contradicted.
type cloneOutput struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	// Target is the branch Branch is diffed and merged against -- `main`
	// for a work branch created by `work start ... main`, and Branch
	// itself when a target branch was cloned directly.
	Target string `json:"target"`
	// BaseSHA is the merge base of Target and Branch: the commit this
	// branch's changes actually start from, and the left endpoint of the
	// three-dot range both `loam work diff` and `git diff
	// origin/<target>...HEAD` compute.
	BaseSHA string `json:"base_sha"`
	// HeadSHA is the cloned checkout's HEAD.
	HeadSHA string `json:"head_sha"`
}

// humanText implements the humanText interface (encoder.go): `loam clone`'s
// human rendering names the two commands that work in the clone it just
// made. loam-hwru's own bead calls this out as its third fix -- `loam work
// diff` existed the whole time and reviewers found it only AFTER
// reconstructing the base by hand, because nothing in the clone's output
// mentioned it.
func (o cloneOutput) humanText() string {
	return fmt.Sprintf("Cloned %s at %s into %s\n"+
		"  target:   %s\n"+
		"  base:     %s (merge base of %s and %s)\n"+
		"  head:     %s\n"+
		"\n"+
		"Diff this branch against its target with either of:\n"+
		"  git diff origin/%s...HEAD\n"+
		"  loam work diff %s %s\n",
		o.Repo, o.Branch, o.Path, o.Target, o.BaseSHA, o.Target, o.Branch, o.HeadSHA, o.Target, o.Repo, o.Branch)
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
	target, err := targetBranchFor(ctx, deps, repo, branch, cloneBranch)
	if err != nil {
		return err
	}
	if err := bootstrapTargetRef(ctx, deps, dest, branch, target); err != nil {
		return err
	}
	base, head, err := cloneRange(ctx, deps.gitRefs, dest, target)
	if err != nil {
		return err
	}
	return deps.encoder.Encode(cloneOutput{Repo: repo, Path: dest, Branch: branch, Target: target, BaseSHA: base, HeadSHA: head})
}

// targetBranchFor resolves the branch this clone should be diffed against.
// A TARGET branch cloned directly is its own target -- there is nothing
// above it -- so that case answers without a request. Anything else is a
// work branch, and its target is registry state, not something to infer
// from the name: GetWorkBranch owns the answer, exactly as
// cloneBranchFor consults GetRepo's target_branches rather than
// pattern-matching. cloneBranch is passed in rather than recomputed so the
// two decisions cannot drift apart.
func targetBranchFor(ctx context.Context, deps *Deps, repo, branch, cloneBranch string) (string, error) {
	if cloneBranch == branch {
		return branch, nil
	}
	resp, err := deps.connect.WorkBranch().GetWorkBranch(ctx, connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: repo, WorkBranch: branch}))
	if err != nil {
		return "", fmt.Errorf("resolving the target branch of work branch %s/%s: %w", repo, branch, err)
	}
	target := resp.Msg.GetWorkBranch().GetTarget()
	if target == "" {
		return "", fmt.Errorf("work branch %s/%s reports no target branch, so its base commit cannot be identified", repo, branch)
	}
	return target, nil
}

// bootstrapTargetRef brings the target branch down as
// refs/remotes/origin/<target> and registers the refspec that keeps it
// current, so PLAIN git works in the clone from here on:
//
//	git diff origin/<target>...HEAD
//	git log origin/<target>..HEAD
//
// This is loam-hwru's primary fix. `git clone --single-branch` fetches
// exactly one ref, and refnames.ClientFetchRefspec (bootstrapWorkBranchRefspecs
// above) adds only the RESERVED namespace -- work branches. Neither covers
// refs/heads/<target>, so before this the clone had no origin/main at all.
//
// Both halves are written, not just the fetch: without the config line the
// ref lands once and then silently ages as the target moves, which would
// trade a loud "unknown revision" for a quiet wrong answer -- the exact
// exchange this bead exists to refuse. AddConfig (not SetConfig) for the
// same reason bootstrapWorkBranchRefspecs uses it: remote.origin.fetch is
// multi-valued and already holds two entries by this point.
//
// A failure here is fatal to the clone rather than a warning. A clone that
// silently lacks its base ref is the state the bead was filed about, and
// "mostly cloned" is not a state worth handing back.
func bootstrapTargetRef(ctx context.Context, deps *Deps, dest, branch, target string) error {
	if target == branch {
		return nil
	}
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", target, target)
	if err := deps.cloner.AddConfig(ctx, dest, "remote.origin.fetch", refspec); err != nil {
		return fmt.Errorf("adding the target-branch fetch refspec %s in %s: %w", refspec, dest, err)
	}
	if deps.gitRefs == nil {
		return fmt.Errorf("fetching target branch %s into %s: no git ref resolver configured", target, dest)
	}
	if err := deps.gitRefs.Fetch(ctx, dest, refspec); err != nil {
		return newPreconditionFailedError(fmt.Sprintf("fetching target branch %s into %s: %s", target, dest, err), err)
	}
	return nil
}

// cloneRange resolves the two commits cloneOutput reports: HEAD, and the
// merge base of origin/<target> and HEAD. The merge base -- not
// origin/<target>'s tip -- is what `git diff origin/<target>...HEAD`
// actually starts from, and reporting the tip under the name "base" would
// be a subtler version of the same wrong answer this bead is about.
func cloneRange(ctx context.Context, refs gitRefs, dest, target string) (base, head string, err error) {
	if refs == nil {
		return "", "", fmt.Errorf("identifying the cloned range in %s: no git ref resolver configured", dest)
	}
	head, err = refs.RevParse(ctx, dest, "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("resolving HEAD in %s: %w", dest, err)
	}
	base, err = refs.MergeBase(ctx, dest, "refs/remotes/origin/"+target, "HEAD")
	if err != nil {
		return "", "", newPreconditionFailedError(fmt.Sprintf("finding the merge base of origin/%s and HEAD in %s: %s", target, dest, err), err)
	}
	return base, head, nil
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
//
// It runs DETACHED (see gitenv.go), and the two halves of that do very
// different amounts of work -- worth separating, because one is load-bearing
// and the other is not:
//
//   - The ENVIRONMENT STRIPPING is load-bearing. An ambient
//     GIT_CONFIG_PARAMETERS -- which git sets on the children of an alias,
//     so a `loam clone` invoked that way inherits one -- carries arbitrary
//     config into this clone, url.insteadOf included, and would silently
//     redirect the very first request to a host loam never named. It also
//     carried init.templatedir, which was CODE EXECUTION. GIT_TEMPLATE_DIR
//     is the same execution by a shorter route. All are stripped, and all
//     are pinned by tests.
//   - The detached GIT_DIR itself is BELT-AND-BRACES, and deliberately not
//     pinned, because there is nothing to observe. `git clone` establishes
//     its own repository at the destination and consults GIT_DIR for
//     nothing, and it reads no enclosing repository either (verified on git
//     2.50.1: an enclosing url.insteadOf, core.hooksPath and
//     http.extraHeader are all ignored). Cloning with and without the
//     detached GIT_DIR, from inside a repository carrying all three,
//     produced byte-identical destination configs. It is kept because it is
//     free and it is the right default for a call site that must read no
//     repository -- not because it closes anything today. See
//     TestGitSubprocessEnv_DetachedGitDirPointsOutsideTheEnclosingRepository
//     for why no test claims otherwise.
//
// The LEADING empty `--config http.extraHeader=` is the same reset
// execGitRefs.LsRemote uses, for the same reason and against the same layer
// (loam-54ze round 2). Detachment closes the enclosing repository; it does
// not close the user's own ~/.gitconfig, which this package deliberately
// keeps honouring. Measured on git 2.50.1 against a header-logging server,
// with a global http.extraHeader carrying "Loam-Agent-Name: GLOBAL-ATTACKER":
// without the reset the initial fetch sent [GLOBAL-ATTACKER real] and git
// accumulates rather than replaces, so the attacker's identity arrived
// FIRST and won; with it, [real].
//
// The trust-domain argument that protects the rest of the user's config
// does not reach this key, and that asymmetry is deliberate rather than an
// oversight. That argument is about not clobbering settings loam has no
// opinion about -- http.proxy, http.sslCAInfo, core.autocrlf, LFS filters,
// each of which decides whether the clone works at all or what it contains.
// Loam-Agent-* is not one of those: it is loam's own identity assertion,
// the thing the entire authorisation model is keyed on, and the reset
// clears ONLY that key, leaving proxy, CA and filters untouched.
//
// One consequence beyond the initial fetch, verified rather than assumed:
// --config persists, so the clone's .git/config carries the empty entry
// ahead of the three real ones, and every LATER fetch and push from that
// clone is reset the same way (measured: an operation from inside the clone
// sent [real], not [GLOBAL-ATTACKER real]).
//
// That persistence is the MORE valuable half, not a side effect: it is what
// protects the plain `git push` an agent runs by hand, which is exactly the
// path that bypasses loam's own header construction entirely and would
// otherwise carry whatever the user's global config prepends.
//
// IT HAS A COST, and it is stated here because its symptom points nowhere
// near loam. A LEGITIMATE global http.extraHeader -- a corporate gateway
// token, a routing header -- is silently dropped from a loam clone's
// operations too, and presents as an unexplained network failure with
// nothing implicating loam. Measured against a header-logging server: with
// a global "X-Corp-Route: eu-west" set, an operation from inside a loam
// clone sent only Loam-Agent-Name, and X-Corp-Route was absent.
//
// The workaround is verified and is one command: re-add the header to the
// CLONE's own config (`git -C <clone> config --add http.extraHeader
// "X-Corp-Route: eu-west"`). Because the reset lives at the head of the
// clone's own multi-valued list, anything added afterward lands AFTER it
// and survives -- measured, both X-Corp-Route and loam's identity arrived
// on the wire together.
func (execGitCloner) Clone(ctx context.Context, url, branch, dest string, headers []string) error {
	args := []string{"clone", "--branch", branch, "--single-branch", "--config", "http.extraHeader="}
	for _, h := range headers {
		args = append(args, "--config", "http.extraHeader="+h)
	}
	args = append(args, url, dest)
	return runDetachedGitCommand(ctx, args...)
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

// runGitCommand runs `git -C dir <args...>`, run purely for its effect. err
// wraps the process's combined stdout+stderr so callers can surface git's
// own reason for a failure (a missing remote branch, an unreachable URL,
// ...).
//
// dir == "" means the CALLER's own working copy, exactly as it does for
// gitrefs.go's runGitOutput -- an invocation that must read NO repository
// is runDetachedGitCommand instead.
func runGitCommand(ctx context.Context, dir string, args ...string) error {
	fullArgs := args
	if dir != "" {
		fullArgs = append([]string{"-C", dir}, args...)
	}
	return gitCombined(ctx, gitSubprocessEnv(""), args, fullArgs)
}

// runDetachedGitCommand runs `git <args...>` with no repository in scope at
// all -- see gitenv.go. For Clone, whose destination does not exist yet and
// which must inherit nothing from whatever repository the caller happens to
// be standing in.
func runDetachedGitCommand(ctx context.Context, args ...string) error {
	gitDir, cleanup, err := gitDetached()
	if err != nil {
		return err
	}
	defer cleanup()
	return gitCombined(ctx, gitSubprocessEnv(gitDir), args, args)
}

// gitCombined is the shared body of the two helpers above. reportArgs is
// what a failure names (the caller's own arguments, without this package's
// addressing flags); fullArgs is what git actually receives.
func gitCombined(ctx context.Context, env []string, reportArgs, fullArgs []string) error {
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(reportArgs, " "), err, bytes.TrimSpace(out.Bytes()))
	}
	return nil
}
