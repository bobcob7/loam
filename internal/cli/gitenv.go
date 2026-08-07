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
// Ambient GIT_* variables belong on the same side of that boundary. The two
// that matter get there by different routes, and an earlier draft of this
// comment conflated them -- it claimed git sets GIT_DIR on a `loam` run from
// a hook, an alias or `git rebase -x`, under a "measured" prefix, and that
// is false. What was actually measured, on git 2.50.1:
//
//   - GIT_CONFIG_PARAMETERS IS propagated by git itself. An alias sets it
//     (carrying any `-c` given alongside), and it carries arbitrary config
//     including url.insteadOf -- and, pre-loam-54ze, init.templatedir, which
//     was a live CODE EXECUTION channel (see inheritedGitRepoVars). This is
//     the sharper of the two, and the one git routinely hands to children.
//     `git rebase -x` and hooks did NOT set it.
//   - GIT_DIR is NOT set by an alias, by `git rebase -x`, or by a
//     pre-commit/pre-push/post-checkout hook -- all four measured UNSET.
//     git sets it absolute in `git filter-branch` and in hooks run DURING a
//     clone or checkout, and relative (".git") in `git submodule foreach`.
//     Its real reachability is therefore external tooling -- IDEs, hook
//     managers, wrapper scripts, CI harnesses -- rather than git handing it
//     to you routinely, plus those two git contexts.
//
// GIT_DIR is on the list regardless of how it gets set, because of what it
// does when it is: an ambient ABSOLUTE GIT_DIR OVERRIDES `-C dir` for
// config, ref and object resolution while `-C dir` still supplies the
// working tree, so a `-C`-addressed invocation silently reads one
// repository's config against another's tree. The "absolute" qualifier is
// load-bearing and must not be dropped in a later edit: a RELATIVE ambient
// GIT_DIR (".git", as submodule foreach sets it) resolves against the
// directory `-C` supplied and does NOT override. Both measured on git
// 2.50.1.
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
//   - A hostile ~/.gitconfig or /etc/gitconfig, IN GENERAL. Deliberately
//     out of scope (see the boundary argument above): loam has no opinion
//     about http.proxy, http.sslCAInfo, core.autocrlf or LFS filters, and
//     clobbering them would break clones that must work.
//
//     ONE key is excepted, and the exception is deliberate rather than an
//     inconsistency: http.extraHeader carries Loam-Agent-*, which is loam's
//     own identity assertion and the thing the whole authorisation model is
//     keyed on -- not a user setting loam is neutral about. Both LsRemote
//     and Clone therefore reset it with an empty-string entry, which clears
//     only that key. This was a declared residual on `loam clone` until
//     round 2 measured it: a global http.extraHeader really did reach
//     clone's initial fetch AHEAD of loam's own identity, and the reset
//     really does close it. Every OTHER global key still applies, by design.
//   - A hostile GIT_CONFIG_GLOBAL/GIT_CONFIG_NOSYSTEM setting is likewise
//     honoured, since those select which of the user's own files apply.
//     Same trust domain, same reasoning.
//   - `execGitRefs.Fetch(ctx, dest, ...)`, which by design uses dest's own
//     remote, headers and hooks. `git fetch` runs the reference-transaction
//     hook out of the fetched repository's core.hooksPath (verified against
//     git 2.50.1), so that call executes code from dest's config. It is
//     called from exactly one place -- `loam clone`, against the clone this
//     process just created two lines earlier -- so it is not reachable with
//     a foreign dest today. Nothing here would stop it if a future caller
//     passed one.
//
// One thing this list must NOT be read as saying, because an earlier draft
// did say it and it is false: it is NOT true that "no discovery-performing
// CLI invocation writes a ref or checks out a tree". `git clone` checks out
// a tree and runs post-checkout. The accurate statement is narrower and
// stronger: clone runs hooks from the DESTINATION IT JUST CREATED, and the
// enclosing repository cannot influence what lands there -- verified on git
// 2.50.1 for both core.hooksPath (an enclosing hooksPath did not fire
// during clone) and init.templateDir (an enclosing templateDir did not
// reach clone). The channels that CAN put code there are ambient, not
// inherited-from-cwd, and both are closed above: GIT_CONFIG_PARAMETERS
// carrying init.templatedir, and GIT_TEMPLATE_DIR. That distinction is the
// whole reason this list exists, so a future reader deciding whether a new
// call site needs detaching does not conclude "clone executes nothing".
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

// inheritedGitRepoVars are the environment variables that would let
// something outside this package redirect an invocation this package
// addressed explicitly -- by relocating the repository, its index or its
// object stores, by injecting config, or by deciding what executable code
// lands in a repository this package creates. gitSubprocessEnv drops every
// one of them.
//
// The charter is deliberately "anything ambient that changes what the
// invocation DOES", not the narrower "variables that locate a repository"
// an earlier draft used. That narrower charter is what hid GIT_TEMPLATE_DIR
// (below) for a whole revision: it is not a locating variable, and under
// the old wording it did not obviously belong.
//
// GIT_TEMPLATE_DIR is the sharpest entry here and the only one that is
// CODE EXECUTION rather than misdirection: `git clone` copies the named
// directory's hooks/ into the new repository and then runs post-checkout
// out of it. Measured on git 2.50.1 under this package's full post-fix
// shape (detached GIT_DIR, GIT_CONFIG_PARAMETERS stripped), an ambient
// GIT_TEMPLATE_DIR still executed an attacker's post-checkout during
// `loam clone` and left the hook installed in the clone; dropping the
// variable stops both.
//
// The same execution was reachable pre-loam-54ze through
// GIT_CONFIG_PARAMETERS carrying init.templatedir -- also measured, also
// fired -- and stripping GIT_CONFIG_PARAMETERS already closed that door.
// So this list was closing a code-execution channel before anyone noticed
// it was one.
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
	"GIT_TEMPLATE_DIR":                 {},
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
