// Package diffplan implements loam-c94.3: given a repo's bare mirror and an
// old_ref/new_ref pair, it turns `git diff --name-status` into a per-file
// action Plan (docs/ingestion-spec.md "Incremental Build") -- which files to
// drop derived rows for, which to re-parse/re-embed, with the rest reused
// implicitly. Planner is a PURE producer: it never touches the database, so
// the atomic-swap orchestrator (loam-c94.12) owns the transaction and this
// package stays unit-testable against nothing but a real git mirror.
//
// Planner also authoritatively finalizes the job's Kind, escalating an
// incremental request to full whenever incremental is unsafe or impossible.
// docs/ingestion-spec.md "Incremental Build" -> "Full rebuild" lists the
// triggers this package detects: first ingest (Request.OldRef empty -- see
// its own doc comment), "no valid diff base (force-push, history rewrite,
// shallow/reset ref)" (checkMergeBase), and "a Tree-sitter grammar / pipeline
// version bump, or an embedding-model change" (versionMismatchReason). This
// package adds two escalation triggers of its own, not spec-pinned: an
// unparseable `--name-status` record (defensive; see parseNameStatusZ) and a
// changed-file count above maxIncrementalChanges (too large to be worth
// incrementalizing). The admin-driven triggers in that same spec bullet list
// ("the admin changing the repo's indexed branch", "a manual reindex
// requested by the admin") are NOT this package's concern: they arrive as an
// already-Kind=full Request from the caller, which Plan honors directly
// without running any of the checks above.
//
// Git plumbing here reuses internal/gitdiff's hard-won isolation, verified
// empirically against real git during that package's own review
// (loam-fwk): `--git-dir=<mirrorDir>`, never `-C <mirrorDir>` (the latter
// performs upward repository discovery and can silently operate on an
// enclosing repository instead of failing); `--no-ext-diff`, never `-c
// diff.external=` (git treats an explicitly-empty diff.external as
// configured and tries to exec it); an explicit minimal environment (not
// os.Environ()) with GIT_CONFIG_NOSYSTEM and a redirected HOME/
// XDG_CONFIG_HOME so no host or user gitconfig is ever read; and
// exec.CommandContext with a WaitDelay so a canceled request's context kills
// the subprocess. That plumbing now lives in internal/gitrun (loam-ldx):
// gitdiff's run/gitEnv had been copied here "verbatim" (this comment's own
// prior wording) because gitdiff exported none of it, and five further
// copies elsewhere in this tree made the duplication itself the bug worth
// fixing -- see internal/gitrun's own package doc comment. Only the
// classification of a git subcommand's raw exit/stderr into this package's
// own Plan/error vocabulary stays here, per that package's explicit
// call: launch mechanics are shared, interpretation is not.
package diffplan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/bobcob7/loam/internal/gitrun"
	"github.com/bobcob7/loam/internal/ingest"
)

// maxIncrementalChanges bounds how many changed-file records
// diffNameStatus's parsed output may contain before Plan gives up on
// incrementalizing and escalates to a full rebuild. Not pinned by
// docs/ingestion-spec.md or any other spec in this tree (grepped: no such
// value exists) -- this package's own choice, the same kind of judgment call
// gitdiff.maxDiffBytes documents: past this many touched files, running the
// downstream parse/chunk/embed pipeline per file is no longer meaningfully
// cheaper than a full rebuild's whole-tree walk, so there is little to lose
// by conceding the fallback rather than incrementalizing a diff this large.
// A package-level var, not a const, so tests can shrink it without
// materializing thousands of real fixture files.
var maxIncrementalChanges = 2000

// maxStderrBytes caps captured stderr the same way internal/gitdiff does --
// git's own error output is always a few lines, never proportional to
// repository or diff size.
const maxStderrBytes = 64 << 10

// ErrMirrorMissing indicates the mirror at the given mirrorDir does not
// exist on disk, or is not a valid git repository at all. Unlike every
// full-rebuild trigger this package detects, this is an operational fault --
// nothing (incremental or full) can be planned without a valid mirror -- so
// it is returned as an error, never silently escalated into a Plan.
var ErrMirrorMissing = errors.New("diffplan: bare mirror missing or invalid on disk")

// ErrRefMissing indicates Request.NewRef does not resolve to a commit in the
// mirror. NewRef is expected to always be the live mirror's own tip,
// resolved by the caller immediately before calling Plan (see Request's doc
// comment) -- so this signals a caller/environment fault, not a condition
// this package's full-rebuild fallback is meant to paper over. Contrast with
// Request.OldRef, whose unresolvability is exactly one of the conditions
// this package DOES treat as a routine escalation trigger (see
// checkMergeBase).
var ErrRefMissing = errors.New("diffplan: new_ref not found in mirror")

// errUnparseableStatus is parseNameStatusZ's internal signal that a
// `--name-status -z` record did not conform to any status this package
// knows how to classify -- never returned to Plan's caller as an error;
// Plan treats it as one more full-rebuild escalation trigger (see Plan's
// doc comment).
var errUnparseableStatus = errors.New("diffplan: unparseable git diff --name-status record")

// Versions is the grammar/pipeline/embedding-model version triple
// docs/persistence-spec.md's "repo_target_branches" section says
// `ingested_versions` (jsonb) records, compared "against the binary's
// versions to trigger the full-rebuild fallback" (same section). That
// section describes the column's purpose, not a field-level shape -- no
// other package in this tree defines one yet (grepped: none exists) -- so
// this struct is diffplan's own choice, meant to be the shape
// repo_target_branches persists once that store exists.
type Versions struct {
	Grammar        string
	Pipeline       string
	EmbeddingModel string
}

// equal reports whether v and other carry the same grammar, pipeline, and
// embedding-model versions.
func (v Versions) equal(other Versions) bool {
	return v.Grammar == other.Grammar && v.Pipeline == other.Pipeline && v.EmbeddingModel == other.EmbeddingModel
}

// Request is one Plan call's input: an old_ref/new_ref pair already
// resolved by the caller (per internal/ingest's Orchestrator doc comment,
// resolving RepoID/TargetBranch to refs is the Orchestrator's job, not this
// package's), the kind the caller is asking for, and the version state Plan
// needs to decide whether an incremental request is actually safe.
type Request struct {
	// OldRef is the previously ingested commit (repo_target_branches.
	// ingested_ref), or "" if the repo has never been ingested on this
	// target branch -- docs/persistence-spec.md "repo_target_branches":
	// "null until first ingest". Plan treats "" as a hard signal to build a
	// full plan; it is never itself resolved against the mirror.
	OldRef string
	// NewRef is the live mirror's current tip for the target branch. Unlike
	// OldRef, this is expected to always resolve (see ErrRefMissing's doc
	// comment).
	NewRef string
	// RequestedKind is the kind the caller is asking for. Plan only ever
	// escalates KindIncremental to KindFull; a KindFull request is always
	// honored as-is.
	RequestedKind ingest.Kind
	// StoredVersions is the grammar/pipeline/embedding-model versions the
	// current index (as of OldRef) was built with, or nil if never
	// recorded. nil is treated the same as a mismatch: without a recorded
	// version to compare against, Plan cannot certify that incremental
	// reuse of the existing index is safe.
	StoredVersions *Versions
	// CurrentVersions is the running binary's own grammar/pipeline/
	// embedding-model versions.
	CurrentVersions Versions
}

// Plan is Planner.Plan's pure output: DropFiles/ReparseFiles are the incremental
// action lists docs/ingestion-spec.md's "Incremental Build" describes, with
// unchanged files appearing in neither (left untouched -- "reused"
// implicitly, never enumerated).
//
// DropFiles is only ever populated when Kind is KindIncremental. For
// KindFull, "drop all existing derived rows for the repo+target_branch"
// (this bead's own DESCRIPTION) is a repo-scoped delete the orchestrator
// performs keyed on Kind alone, not a per-file operation -- so DropFiles is
// always nil for a full Plan, and ReparseFiles instead carries every file in
// the tree at NewRef.
type Plan struct {
	// Kind is the finalized kind: RequestedKind, or KindFull if Plan
	// escalated it. Never downgrades KindFull to KindIncremental.
	Kind ingest.Kind
	// Reason is non-empty iff Kind was escalated above the Request's
	// RequestedKind, naming which trigger fired -- for logging at the
	// call site, not itself a machine-checked value.
	Reason string
	// DropFiles are paths whose symbols/symbol_references/chunks rows must
	// be dropped: deleted files, and a rename's/copy's OLD path (a rename
	// is a delete of the old path plus an add of the new one, for derived
	// tables -- see this bead's own DESIGN). Always nil when Kind is
	// KindFull.
	DropFiles []string
	// ReparseFiles are paths to re-parse and re-embed: added/modified
	// files, and a rename's/copy's NEW path. For a full Plan, this is
	// every file in the tree at NewRef.
	ReparseFiles []string
}

// WithRetryPaths returns p with paths added to its ReparseFiles: the
// per-path rejection ledger's outstanding entries, unioned into the plan
// so a file the chunk store previously refused is re-read, re-chunked and
// re-embedded even though it did not change (loam-qj21).
//
// This is the whole of the retry mechanism, and it lives here because the
// gap it closes is a property of what a diff can express. Plan is built
// from `git diff old..new`, which by construction reports only paths that
// DIFFER between the two refs. A rejected file did not change -- the
// ingest that rejected it still advanced ingested_ref past it, because
// every other file in that batch landed -- so it appears in neither
// DropFiles nor ReparseFiles, and no amount of care inside the diff
// classification can put it back. Only a source of paths from OUTSIDE the
// diff can, which is what the ledger is.
//
// Three rules, each of which would be a defect if dropped:
//
//   - A KindFull plan is returned UNCHANGED. Its ReparseFiles is already
//     every file in the tree at NewRef, so any ledgered path still in the
//     tree is in it; and a ledgered path NOT in the tree must not be
//     added, because it does not exist to be read. (The caller empties the
//     ledger for a full rebuild in the same transaction, so those rows do
//     not survive to be retried on a tree that no longer holds them.)
//   - A path already in DropFiles is skipped. The plan says that file was
//     deleted or renamed away; reparsing it would ask git for a blob that
//     is not at NewRef. Its ledger row is cleared by the caller instead --
//     nothing is missing once the file itself is gone.
//   - Duplicates are skipped, in both directions: against ReparseFiles
//     (the ordinary case where the ledgered file was ALSO edited, so the
//     diff already names it) and against paths itself.
//
// The returned Plan's Kind and Reason are untouched. A retry is not an
// escalation: it adds files to an incremental plan, it does not make the
// plan a rebuild.
func (p Plan) WithRetryPaths(paths []string) Plan {
	if p.Kind == ingest.KindFull || len(paths) == 0 {
		return p
	}
	seen := make(map[string]struct{}, len(p.ReparseFiles)+len(p.DropFiles)+len(paths))
	for _, f := range p.ReparseFiles {
		seen[f] = struct{}{}
	}
	for _, f := range p.DropFiles {
		seen[f] = struct{}{}
	}
	var extra []string
	for _, f := range paths {
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		extra = append(extra, f)
	}
	if len(extra) == 0 {
		return p
	}
	out := p
	out.ReparseFiles = append(append(make([]string, 0, len(p.ReparseFiles)+len(extra)), p.ReparseFiles...), extra...)
	return out
}

// Planner builds Plans by shelling out to real git against a repo's bare
// mirror. Construct with New; stateless beyond its logger, so a single
// Planner is safe to reuse and share across repos and jobs.
type Planner struct {
	logger *slog.Logger
}

// New builds a Planner. logger must be non-nil.
func New(logger *slog.Logger) *Planner {
	return &Planner{logger: logger}
}

// Plan resolves req against the mirror at mirrorDir into a finalized Plan.
// It first verifies mirrorDir is a valid repository and req.NewRef resolves
// within it (ErrMirrorMissing / ErrRefMissing -- hard faults, never
// escalated away); a KindFull request is then honored directly via
// buildFullPlan. For an incremental request, Plan runs, in order, every
// full-rebuild-fallback check this package owns -- first ingest, version
// mismatch, invalid merge base, then the diff itself (unparseable output or
// too many changed files) -- falling back to buildFullPlan the moment any of
// them fires, and only then runs `git diff --name-status -z` for the actual
// incremental Plan.
func (p *Planner) Plan(ctx context.Context, mirrorDir string, req Request) (Plan, error) {
	if err := p.verifyRef(ctx, mirrorDir, req.NewRef); err != nil {
		return Plan{}, fmt.Errorf("verifying new_ref %s: %w", req.NewRef, err)
	}
	if req.RequestedKind == ingest.KindFull {
		return p.buildFullPlan(ctx, mirrorDir, req.NewRef, "")
	}
	if req.OldRef == "" {
		return p.buildFullPlan(ctx, mirrorDir, req.NewRef, "no previous indexed commit (first ingest)")
	}
	if reason := versionMismatchReason(req.StoredVersions, req.CurrentVersions); reason != "" {
		return p.buildFullPlan(ctx, mirrorDir, req.NewRef, reason)
	}
	reason, err := p.checkMergeBase(ctx, mirrorDir, req.OldRef, req.NewRef)
	if err != nil {
		return Plan{}, err
	}
	if reason != "" {
		return p.buildFullPlan(ctx, mirrorDir, req.NewRef, reason)
	}
	changes, reason, err := p.diffNameStatus(ctx, mirrorDir, req.OldRef, req.NewRef)
	if err != nil {
		return Plan{}, err
	}
	if reason != "" {
		return p.buildFullPlan(ctx, mirrorDir, req.NewRef, reason)
	}
	return buildIncrementalPlan(changes), nil
}

// versionMismatchReason reports why req's stored versions no longer match
// current (the full-rebuild trigger docs/ingestion-spec.md's "Incremental
// Build" -> "Full rebuild" names as "a Tree-sitter grammar / pipeline
// version bump, or an embedding-model change"), or "" if they match. nil
// stored versions is treated as a mismatch (Request.StoredVersions' own doc
// comment).
func versionMismatchReason(stored *Versions, current Versions) string {
	if stored == nil {
		return "no stored grammar/pipeline/embedding-model versions recorded"
	}
	if !stored.equal(current) {
		return fmt.Sprintf("stored versions %+v differ from current %+v", *stored, current)
	}
	return ""
}

// checkMergeBase runs `git merge-base old new` as the "valid diff base"
// check this bead's own DESIGN names explicitly. A common ancestor existing
// is not itself required for `git diff old..new` to succeed (verified
// empirically: unlike the three-dot form, `git diff`'s two-dot syntax is
// just two commits named side by side, not a revision range -- it diffs the
// two trees directly regardless of ancestry), so this check exists
// specifically to detect the conditions docs/ingestion-spec.md's "Full
// rebuild" bullet groups together as "no valid diff base (force-push,
// history rewrite, shallow/reset ref)": old either no longer resolves at
// all (pruned after an upstream force-push -- loam-giq.2's mirror fetch
// runs forced + pruning) or resolves but shares no common history with new
// (an unrelated-history rewrite). Both shapes make real git's merge-base
// exit nonzero (empirically: exit 1 with no stderr for "no common
// ancestor", exit 128 with "fatal: Not a valid commit name ..." for an
// unresolvable old), so any nonzero exit here is treated uniformly as
// "invalid diff base" and returns a non-empty reason, not just those two
// specific cases -- except a bad --git-dir (mirror missing under this
// call), which is classified separately and returned as ErrMirrorMissing.
func (p *Planner) checkMergeBase(ctx context.Context, mirrorDir, oldRef, newRef string) (string, error) {
	out, err := p.run(ctx, mirrorDir, "merge-base", oldRef, newRef)
	if err != nil {
		return "", err
	}
	if out.exitCode == 0 {
		return "", nil
	}
	if isMirrorMissingStderr(out.stderr) {
		return "", fmt.Errorf("%s: %w", mirrorDir, ErrMirrorMissing)
	}
	return fmt.Sprintf("no valid diff base between %s and %s (git merge-base exited %d: %s)",
		oldRef, newRef, out.exitCode, strings.TrimSpace(out.stderr)), nil
}

// fileChange is one parsed `--name-status -z` record.
type fileChange struct {
	status  byte
	oldPath string
	// newPath is only set for a rename/copy record (status R/C); empty
	// otherwise.
	newPath string
}

// diffNameStatus runs `git diff --no-ext-diff --name-status -z old..new`
// and parses its output. A non-empty reason means Plan should escalate to a
// full rebuild (an unparseable record, or too many changed files); err is
// only ever a hard failure (the subprocess itself failing to run, or the
// mirror going missing between checkMergeBase and this call).
func (p *Planner) diffNameStatus(ctx context.Context, mirrorDir, oldRef, newRef string) ([]fileChange, string, error) {
	out, err := p.run(ctx, mirrorDir, "diff", "--no-ext-diff", "--name-status", "-z", oldRef+".."+newRef)
	if err != nil {
		return nil, "", err
	}
	if out.exitCode != 0 {
		if isMirrorMissingStderr(out.stderr) {
			return nil, "", fmt.Errorf("%s: %w", mirrorDir, ErrMirrorMissing)
		}
		return nil, "", fmt.Errorf("git diff --name-status %s..%s exited %d: %s", oldRef, newRef, out.exitCode, strings.TrimSpace(out.stderr))
	}
	changes, err := parseNameStatusZ(out.stdout)
	if err != nil {
		return nil, fmt.Sprintf("unparseable git diff --name-status output: %s", err), nil
	}
	if len(changes) > maxIncrementalChanges {
		return nil, fmt.Sprintf("%d changed files exceeds incremental threshold of %d", len(changes), maxIncrementalChanges), nil
	}
	return changes, "", nil
}

// parseNameStatusZ parses `git diff --name-status -z` output: a flat stream
// of NUL-separated fields, NOT NUL-terminated lines with tab-separated
// fields the way the non -z form is -- verified empirically against real
// git 2.50.1. That distinction is exactly why -z is used at all: without
// it, git C-quotes any path containing a control character -- a filename
// with a literal newline comes back as `A\t"weird\nname.txt"`, the newline
// escaped inside double quotes, regardless of core.quotepath. Records do
// not shift (git is careful about that), but nothing here unescapes C
// quoting, so the non-z form would hand us a mangled path that silently
// fails to match the file on disk. Verified empirically, not assumed --
// see TestPlan_FilenameWithNewline_ParsedCorrectly_RequiresDashZ. Most statuses (A/M/D/T -- added,
// modified, deleted, type-changed) are a two-field record: the status
// letter (with no similarity suffix) then one path. A rename or copy
// (verified empirically: git enables rename detection for `git diff` by
// default, no -M needed, so an R-status record is a normal, not a rare,
// occurrence here; copy detection, by contrast, needs --find-copies-harder
// and this package never passes it, so a C record should not occur in
// practice -- it is still handled, defensively, the same as R) is a
// three-field record: a status carrying a trailing similarity score (e.g.
// "R100"), then the OLD path, then the NEW path.
func parseNameStatusZ(stdout []byte) ([]fileChange, error) {
	trimmed := bytes.TrimSuffix(stdout, []byte{0})
	if len(trimmed) == 0 {
		return nil, nil
	}
	fields := bytes.Split(trimmed, []byte{0})
	var changes []fileChange
	for i := 0; i < len(fields); {
		status := fields[i]
		i++
		if len(status) == 0 {
			return nil, fmt.Errorf("%w: empty status field", errUnparseableStatus)
		}
		switch status[0] {
		case 'A', 'M', 'D', 'T':
			if i >= len(fields) {
				return nil, fmt.Errorf("%w: status %q missing its path field", errUnparseableStatus, status)
			}
			changes = append(changes, fileChange{status: status[0], oldPath: string(fields[i])})
			i++
		case 'R', 'C':
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("%w: status %q missing its old/new path fields", errUnparseableStatus, status)
			}
			changes = append(changes, fileChange{status: status[0], oldPath: string(fields[i]), newPath: string(fields[i+1])})
			i += 2
		default:
			return nil, fmt.Errorf("%w: unrecognized status %q", errUnparseableStatus, status)
		}
	}
	return changes, nil
}

// buildIncrementalPlan classifies parsed changes into DropFiles/ReparseFiles
// per this bead's own DESIGN: Deleted/Renamed-away/Copied-from -> drop;
// Added/Modified/Type-changed and rename-to/copy-to targets -> reparse.
// Unchanged files never appear in changes at all (git diff only reports
// paths that differ), so they never appear in either list.
func buildIncrementalPlan(changes []fileChange) Plan {
	plan := Plan{Kind: ingest.KindIncremental}
	for _, c := range changes {
		switch c.status {
		case 'A', 'M', 'T':
			plan.ReparseFiles = append(plan.ReparseFiles, c.oldPath)
		case 'D':
			plan.DropFiles = append(plan.DropFiles, c.oldPath)
		case 'R', 'C':
			plan.DropFiles = append(plan.DropFiles, c.oldPath)
			plan.ReparseFiles = append(plan.ReparseFiles, c.newPath)
		}
	}
	return plan
}

// buildFullPlan lists every file in the tree at newRef (`git ls-tree -r -z
// --name-only`, NUL-delimited for the same newline-safety reason
// parseNameStatusZ documents) and returns a KindFull Plan reparsing all of
// them, with DropFiles left nil (see Plan's own doc comment for why a full
// plan's drop is repo-scoped, not file-scoped). reason is copied onto the
// returned Plan verbatim; the empty string means the caller's request was
// already KindFull, not an escalation.
func (p *Planner) buildFullPlan(ctx context.Context, mirrorDir, newRef, reason string) (Plan, error) {
	out, err := p.run(ctx, mirrorDir, "ls-tree", "-r", "-z", "--name-only", newRef)
	if err != nil {
		return Plan{}, err
	}
	if out.exitCode != 0 {
		if isMirrorMissingStderr(out.stderr) {
			return Plan{}, fmt.Errorf("%s: %w", mirrorDir, ErrMirrorMissing)
		}
		return Plan{}, fmt.Errorf("git ls-tree %s exited %d: %s", newRef, out.exitCode, strings.TrimSpace(out.stderr))
	}
	if reason != "" {
		p.logger.InfoContext(ctx, "diffplan escalated to full rebuild", "reason", reason)
	}
	return Plan{Kind: ingest.KindFull, Reason: reason, ReparseFiles: splitNulDelimited(out.stdout)}, nil
}

// splitNulDelimited splits a NUL-terminated stream (as `-z` output is) into
// its individual, non-empty fields.
func splitNulDelimited(stdout []byte) []string {
	trimmed := bytes.TrimSuffix(stdout, []byte{0})
	if len(trimmed) == 0 {
		return nil
	}
	fields := bytes.Split(trimmed, []byte{0})
	paths := make([]string, len(fields))
	for i, f := range fields {
		paths[i] = string(f)
	}
	return paths
}

// verifyRef confirms ref resolves to a commit in the mirror at mirrorDir,
// via `git rev-parse --verify --quiet <ref>^{commit}`, classifying its
// three distinguishable outcomes exactly as internal/gitdiff's own verifyRef
// does: exit 0 (resolves), exit 1 (does not resolve -- ErrRefMissing), exit
// 128 with "not a git repository" in stderr (bad --git-dir --
// ErrMirrorMissing).
func (p *Planner) verifyRef(ctx context.Context, mirrorDir, ref string) error {
	out, err := p.run(ctx, mirrorDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return err
	}
	switch out.exitCode {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%s: %w", ref, ErrRefMissing)
	default:
		if isMirrorMissingStderr(out.stderr) {
			return fmt.Errorf("%s: %w", mirrorDir, ErrMirrorMissing)
		}
		return fmt.Errorf("git rev-parse %s exited %d: %s", ref, out.exitCode, strings.TrimSpace(out.stderr))
	}
}

// isMirrorMissingStderr reports whether stderr is git's own "not a git
// repository" complaint about a bad --git-dir, checked case-insensitively:
// internal/gitdiff's own isMirrorMissingStderr documents (verified
// empirically) that different git subcommands word this differently --
// `rev-parse`/`merge-base` say "fatal: not a git repository: '<path>'" while
// `diff` instead misparses the whole invocation and says "warning: Not a
// git repository." (capital N) -- so a case-sensitive check would silently
// miss one of them.
func isMirrorMissingStderr(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "not a git repository")
}

// gitOutput is one subprocess invocation's classified result, matching
// internal/gitdiff's own gitOutput shape (minus its byte cap: nothing this
// package reads is a caller-facing response body the way gitdiff's unified
// diff text is, so there is no analogous memory-bounding concern here --
// buildFullPlan's own file list is inherently proportional to repo size,
// exactly the cost a full rebuild already accepts).
type gitOutput struct {
	stdout   []byte
	exitCode int
	stderr   string
}

// run executes one git subcommand against mirrorDir (via --git-dir, never
// -C), isolated from the host and user gitconfig via internal/gitrun --
// see that package's own doc comment for the full rationale (loam-ldx
// folded this package's own run/gitEnv, copied verbatim from
// internal/gitdiff, and five other identical copies into it).
func (p *Planner) run(ctx context.Context, mirrorDir string, args ...string) (gitOutput, error) {
	home, cleanup, err := gitrun.NewIsolatedHome()
	if err != nil {
		return gitOutput{}, err
	}
	defer cleanup()
	var outBuf bytes.Buffer
	errBuf := gitrun.NewCappedBuffer(maxStderrBytes)
	cmd := gitrun.NewCommand(ctx, gitrun.Env(home), nil, &outBuf, errBuf, gitrun.GitDirArgs(mirrorDir, args...)...)
	runErr := cmd.Run()
	if runErr == nil {
		return gitOutput{stdout: outBuf.Bytes(), exitCode: 0, stderr: errBuf.String()}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return gitOutput{stdout: outBuf.Bytes(), exitCode: exitErr.ExitCode(), stderr: errBuf.String()}, nil
	}
	return gitOutput{}, fmt.Errorf("running git %v: %w", args, runErr)
}
