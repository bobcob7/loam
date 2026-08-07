package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// execGitRefs is the real gitRefs, backed by the git binary on PATH.
//
// It is deliberately a SECOND git seam alongside execGitCloner rather than
// four more methods on it. execGitCloner is `loam clone`'s one-shot
// bootstrap and nothing else calls it (Deps' own doc comment says as much:
// tests that never dispatch clone pass nil for it). What lives here is the
// identification side -- which commit a diff is OF, and which commit it is
// AGAINST -- and it is called by `work diff` and `work show`, neither of
// which has any business holding a cloner.
type execGitRefs struct{}

// newGitRefs constructs the real gitRefs. Unexported: only deps.go's
// NewProductionDeps and this package's own tests call it.
func newGitRefs() gitRefs { return execGitRefs{} }

// Fetch implements gitRefs via `git -C dest fetch --no-tags origin
// <refspec>...`. --no-tags matters: `loam clone` calls this to add ONE
// target ref to a single-branch clone, and git's default tag-following
// would otherwise drag the mirror's whole tag namespace along with it.
func (execGitRefs) Fetch(ctx context.Context, dest string, refspecs ...string) error {
	args := append([]string{"fetch", "--no-tags", "origin"}, refspecs...)
	return runGitCommand(ctx, dest, args...)
}

// RevParse implements gitRefs via `git -C dir rev-parse --verify
// <rev>^{commit}`. The `^{commit}` peel is not decoration: it makes an
// annotated tag or any other non-commit object an ERROR here rather than
// resolving to an object SHA that no later commit-range operation can use.
func (execGitRefs) RevParse(ctx context.Context, dir, rev string) (string, error) {
	out, err := runGitOutput(ctx, dir, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// MergeBase implements gitRefs via `git -C dir merge-base a b`. git exits
// non-zero with no output when the two share no common ancestor, which
// surfaces here as an error rather than an empty string -- the same
// condition internal/gitdiff classifies as ErrNoMergeBase server-side.
func (execGitRefs) MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	out, err := runGitOutput(ctx, dir, "merge-base", a, b)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// CountCommitsAhead implements gitRefs via `git -C dir rev-list --count
// <base>..HEAD` -- how many commits the checked-out branch holds that base
// does not. This is the check loam-hwru's third failure mode was one
// command away from: base is the tip the SERVER has, so a non-zero count is
// exactly "the diff you are about to read cannot contain your last commit".
//
// An error here is meaningful and is never flattened into 0 by this method:
// git fails when base names an object this repository does not have, which
// is a genuinely different answer from "zero commits ahead" and callers
// must be able to tell them apart.
func (execGitRefs) CountCommitsAhead(ctx context.Context, dir, base string) (int, error) {
	out, err := runGitOutput(ctx, dir, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	trimmed := strings.TrimSpace(out)
	count, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("parsing `git rev-list --count %s..HEAD` output %q: %w", base, trimmed, err)
	}
	return count, nil
}

// LsRemote implements gitRefs via `git -c http.extraHeader=<h> (once per
// entry in headers) ls-remote <url> <ref>...`.
//
// The headers travel as `-c` arguments for the same reason
// execGitCloner.Clone passes them at clone time: this runs against a URL,
// not against a configured remote, so there is no clone config to inherit
// them from, and httpauth.Auth.GitIdentity 403s any /git/* request missing
// them. git's command-line config is a distinct, highest-priority config
// source and repeated `-c` entries for the same MULTI-VALUED key (which
// http.extraHeader is) accumulate rather than overwrite, so all three
// identity headers reach the request.
//
// Asking the remote, rather than reading the caller's own
// refs/remotes/origin/*, is the whole point: a remote-tracking ref is only
// as fresh as the last fetch, and the question this answers is what the
// SERVER will diff, not what this clone last heard about it.
//
// It runs DETACHED -- see gitenv.go for the rule and the mechanism. Unlike
// `git clone` (which establishes a fresh repository and reads no enclosing
// one), `ls-remote` performs ordinary repository discovery from the working
// directory, and the working directory this runs in is normally an agent's
// Loam clone. Everything that clone's config declares would otherwise apply
// to THIS request: its own three Loam-Agent-* http.extraHeaders (sent ahead
// of the ones passed here, since git accumulates that key rather than
// replacing it, so an agent invoking `work diff` from a clone bootstrapped
// under another identity authenticated as that identity), its
// url.<base>.insteadOf (which rewrites the URL before any header matters),
// its credential.helper, its http.proxy. Running with GIT_DIR pointed at a
// path that does not exist means none of it is read at all. Verified
// against real git by TestExecGitRefs_LsRemote_IgnoresEnclosingRepoInsteadOf
// and its two siblings.
//
// The EMPTY leading `-c http.extraHeader=` is kept even though detachment
// already makes the enclosing clone unreadable, and not as decoration: git
// still reads the USER's ~/.gitconfig and /etc/gitconfig here (deliberately
// -- gitenv.go argues why), and the empty-string reset is what git-config(1)
// documents as clearing an accumulated http.extraHeader from those layers
// too. It is the one defence that covers a layer detachment does not.
//
// A ref the remote does not advertise is simply absent from the result,
// not an error -- callers distinguish "missing" from "present" themselves.
func (execGitRefs) LsRemote(ctx context.Context, url string, headers, refs []string) (map[string]string, error) {
	args := make([]string, 0, 2*len(headers)+4+len(refs))
	args = append(args, "-c", "http.extraHeader=")
	for _, h := range headers {
		args = append(args, "-c", "http.extraHeader="+h)
	}
	args = append(args, "ls-remote", url)
	args = append(args, refs...)
	out, err := runDetachedGitOutput(ctx, args...)
	if err != nil {
		return nil, err
	}
	shas := make(map[string]string, len(refs))
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || sha == "" || ref == "" {
			continue
		}
		shas[ref] = sha
	}
	return shas, nil
}

// runGitOutput runs `git -C dir <args...>` and returns its STDOUT.
//
// dir == "" means the CALLER's own working copy, resolved by git's ordinary
// upward discovery -- RevParse(ctx, "", "HEAD") asking "what is checked out
// where I am standing". It does NOT mean "no repository": that is
// runDetachedGitOutput, and conflating the two into one empty-string
// sentinel is how `ls-remote` came to read the enclosing clone's config in
// the first place.
//
// It is a separate helper from clone.go's runGitCommand, which merges
// stdout into the same buffer as stderr: that is right for a command run
// purely for its effect, and wrong for every command here, whose stdout IS
// the answer and must not have git's chatter interleaved into it. stderr is
// still captured, and still carries git's own reason into the error.
func runGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	fullArgs := args
	if dir != "" {
		fullArgs = append([]string{"-C", dir}, args...)
	}
	return gitOutput(ctx, gitSubprocessEnv(""), args, fullArgs)
}

// runDetachedGitOutput runs `git <args...>` with no repository in scope at
// all -- see gitenv.go. For LsRemote, which addresses a URL and must not
// inherit anything from whatever clone the caller happens to be standing
// in.
func runDetachedGitOutput(ctx context.Context, args ...string) (string, error) {
	gitDir, cleanup, err := gitDetached()
	if err != nil {
		return "", err
	}
	defer cleanup()
	return gitOutput(ctx, gitSubprocessEnv(gitDir), args, args)
}

// gitOutput is the shared body of the two helpers above. reportArgs is what
// a failure names (the caller's own arguments, without this package's
// addressing flags); fullArgs is what git actually receives.
func gitOutput(ctx context.Context, env []string, reportArgs, fullArgs []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(reportArgs, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.String(), nil
}
