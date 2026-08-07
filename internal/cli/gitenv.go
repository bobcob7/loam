package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// This file holds the ONE rule every git subprocess this package launches
// obeys, and the two mechanisms that implement it. It exists because
// loam-54ze's predecessor was fixed per-call-site and per-config-key, which
// is what left the rest of the surface reachable.
//
// # The rule
//
// A loam CLI git invocation never lets the WORKING DIRECTORY'S REPOSITORY,
// or an AMBIENT GIT_* VARIABLE, decide which repository it operates on or
// what config it reads. Every invocation either names the repository it
// means (`-C dir`, where dir is a path this package chose) or declares
// "no repository at all" (gitDetached, below).
//
// # Why the working directory is the boundary, and the user's own config is not
//
// The CLI runs on a human's or an agent's own machine, and it is a
// deliberately thin wrapper around git. Its threat boundary is therefore
// NOT the server side's: internal/gitrun and internal/gittransport build a
// git environment from NOTHING ambient (an explicit variable list,
// GIT_CONFIG_NOSYSTEM, a redirected HOME) because they run inside the loam
// server, where every ambient setting is somebody else's and none of it is
// wanted. Copying that here would be actively wrong. `loam clone` behind a
// corporate proxy needs the user's http.proxy; behind a private CA it needs
// their http.sslCAInfo; their core.autocrlf and LFS filters decide what the
// resulting working copy actually contains. Blanking ~/.gitconfig and
// /etc/gitconfig would break all of that to defend against a file the user
// owns and that the loam binary itself is read from the same trust domain
// as.
//
// What is NOT in that trust domain is the enclosing repository. Agents
// routinely cd into a clone another agent created -- the orchestrator
// points one agent at another's clone as a read-only reference, and
// reviewers work inside clones all day. That clone's .git/config is DATA,
// written by a different principal, and until this file existed it decided:
//
//   - which identity the request authenticated as (http.extraHeader -- the
//     originally-reported defect: an assertion expecting grace-hopper
//     received barbara-grosz);
//   - WHERE THE REQUEST WENT AT ALL (url.<base>.insteadOf, which rewrites
//     the URL before any header matters, and which no empty-string reset
//     can undo);
//   - what credential was handed to whatever host it went to
//     (credential.helper);
//   - what proxy it traversed (http.proxy).
//
// Ambient GIT_* variables belong on the same side of that boundary, because
// git ITSELF sets them: a `loam` command run from a git hook, an alias, or
// `git rebase -x` inherits GIT_DIR, GIT_INDEX_FILE and GIT_CONFIG_PARAMETERS
// from the parent git, pointed at the parent's repository. Measured against
// git 2.50.1: an ambient absolute GIT_DIR OVERRIDES `-C dir` for config and
// ref resolution while `-C dir` still supplies the working tree, so a
// `-C`-addressed invocation silently reads one repository's config against
// another's tree. GIT_CONFIG_PARAMETERS carries arbitrary config, including
// insteadOf.
//
// # Mechanism 1: gitSubprocessEnv (every invocation)
//
// os.Environ() minus git's repository-location and config-injection
// variables. Everything else -- PATH, HOME, proxy variables, CA bundles --
// is passed through untouched, per the boundary above.
//
// # Mechanism 2: gitDetached (invocations that must read no repository)
//
// `ls-remote` and `clone` address a URL and need no local repository. git
// clone reads no enclosing repository (verified against git 2.50.1: an
// enclosing url.insteadOf, http.extraHeader and core.hooksPath are all
// ignored by clone), but ls-remote performs ordinary repository discovery
// and reads all of it.
//
// The remedy is to set GIT_DIR to a path that does not exist. git then
// skips discovery entirely and treats the missing config file the way it
// treats a missing ~/.gitconfig: silently, as "no config". Verified against
// git 2.50.1 -- with a hostile enclosing url.insteadOf, ls-remote reaches
// the attacker's host without it and the intended host with it, and the
// path is never created. `git clone` tolerates the same variable (it
// establishes its own repository and never consults GIT_DIR for it), so
// both detached call sites share one mechanism rather than two.
//
// This is preferred over the alternatives, all of which were weighed:
//
//   - Resetting each key (`-c http.extraHeader=`, `-c credential.helper=`)
//     is what the predecessor fix did, and it does not generalise:
//     url.<base>.insteadOf is a NAMESPACE keyed by the rewritten URL, so
//     there is no key to reset and no way to enumerate what a hostile
//     config declared. Chasing keys is how this stayed reachable.
//   - Passing an explicit --git-dir/-C does not help the detached calls:
//     `-C dir` reads dir's own config, and for `Fetch` dir IS the possibly-
//     foreign clone. It also loses to an ambient GIT_DIR, as measured above.
//   - GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM/GIT_CONFIG_NOSYSTEM isolate the
//     wrong layer: verified against git 2.50.1, all three together leave
//     the enclosing repository's config fully in effect, so they do not fix
//     the reported bug at all -- and they break the legitimate cases named
//     above.
//
// # What this rule does NOT neutralise, stated plainly
//
//   - A hostile ~/.gitconfig or /etc/gitconfig. Deliberately out of scope
//     (see the boundary argument above). A global http.extraHeader is still
//     sent on `loam clone`'s initial fetch; LsRemote's surviving
//     empty-string reset happens to cover the global layer too, which is
//     why that reset is kept rather than deleted as redundant.
//   - `execGitRefs.Fetch(ctx, dest, ...)`, which by design uses dest's own
//     remote, headers and hooks. `git fetch` runs the reference-transaction
//     hook out of the fetched repository's core.hooksPath (verified against
//     git 2.50.1), so that call executes code from dest's config. It is
//     called from exactly one place -- `loam clone`, against the clone this
//     process just created two lines earlier -- so it is not reachable with
//     a foreign dest today. Nothing here would stop it if a future caller
//     passed one.
//   - `-C dir` where dir is not itself a repository: git walks UP and
//     operates on an enclosing one. Every current caller passes a directory
//     this package just cloned, so it is not reachable; internal/gitrun's
//     GitDirArgs documents the same hazard for the server side.
//   - RevParse/CountCommitsAhead called with dir == "", which deliberately
//     resolve the CALLER's own working copy. That is the question being
//     asked ("does the clone I am standing in hold unpushed commits"), so
//     discovery is the feature; only the ambient-GIT_DIR override is closed.

// detachedGitDirName is the basename, inside a fresh temp directory, that
// gitDetached points GIT_DIR at and deliberately never creates.
const detachedGitDirName = "loam-cli-no-repository"

// inheritedGitRepoVars are the environment variables git uses to locate a
// repository, its index, its object stores, or extra config -- the ones a
// parent git process sets on its children, and the ones that would let
// something outside this package redirect an invocation this package
// addressed explicitly. gitSubprocessEnv drops every one of them.
//
// GIT_CEILING_DIRECTORIES is on the list for the opposite reason to the
// rest: an ambient one would stop discovery EARLY, which would make
// execGitLookup.CloneRoot report a directory that is not the clone root, or
// fail inside a clone that is perfectly valid.
//
// GIT_CONFIG_NOSYSTEM and GIT_CONFIG_GLOBAL are deliberately absent: they
// select which of the USER's own config files apply, and this package keeps
// honouring those (see this file's header).
var inheritedGitRepoVars = map[string]struct{}{
	"GIT_DIR":                          {},
	"GIT_WORK_TREE":                    {},
	"GIT_COMMON_DIR":                   {},
	"GIT_INDEX_FILE":                   {},
	"GIT_OBJECT_DIRECTORY":             {},
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
	"GIT_NAMESPACE":                    {},
	"GIT_CEILING_DIRECTORIES":          {},
	"GIT_DISCOVERY_ACROSS_FILESYSTEM":  {},
	"GIT_CONFIG":                       {},
	"GIT_CONFIG_PARAMETERS":            {},
	"GIT_CONFIG_COUNT":                 {},
}

// isInheritedGitRepoVar reports whether name is one of the variables
// gitSubprocessEnv drops. The two prefixes are GIT_CONFIG_COUNT's numbered
// companions: dropping the count alone would leave the keys behind for a
// later count to pick up.
func isInheritedGitRepoVar(name string) bool {
	if _, ok := inheritedGitRepoVars[name]; ok {
		return true
	}
	return strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// gitSubprocessEnv builds the environment for one CLI git subprocess:
// os.Environ() with every isInheritedGitRepoVar entry removed, plus
// GIT_DIR=detachedGitDir when detachedGitDir is non-empty (see gitDetached).
//
// The removal has to happen even on the detached path, not just the
// override: GIT_INDEX_FILE, GIT_OBJECT_DIRECTORY and GIT_CONFIG_PARAMETERS
// are honoured independently of GIT_DIR, so setting GIT_DIR alone would
// leave a parent git's index, object store and injected config in play.
func gitSubprocessEnv(detachedGitDir string) []string {
	environ := os.Environ()
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if isInheritedGitRepoVar(name) {
			continue
		}
		env = append(env, kv)
	}
	if detachedGitDir != "" {
		env = append(env, "GIT_DIR="+detachedGitDir)
	}
	return env
}

// gitDetached returns the GIT_DIR value for an invocation that must read no
// repository at all, and a cleanup func the caller must defer once that
// subprocess has exited.
//
// The returned path is inside a freshly-created temp directory and is
// deliberately NEVER created: a per-invocation directory, rather than a
// fixed name under os.TempDir(), is what makes "does not exist" a property
// rather than a hope -- nothing else can have placed a repository there,
// and no concurrent invocation shares it.
func gitDetached() (gitDir string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "loam-cli-detached-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating detached git environment: %w", err)
	}
	return filepath.Join(dir, detachedGitDirName), func() { _ = os.RemoveAll(dir) }, nil
}
