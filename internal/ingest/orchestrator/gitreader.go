package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// subprocessWaitDelay mirrors internal/gitdiff's and internal/diffplan's
// constant of the same name: how long a canceled invocation's process gets
// to exit on its own before Cmd forces its pipes closed.
const subprocessWaitDelay = 5 * time.Second

// maxStderrBytes caps captured stderr the same way internal/gitdiff and
// internal/diffplan do -- git's own error output is a few lines, never
// proportional to repository or blob size.
const maxStderrBytes = 64 << 10

// errBranchMissing means the target branch does not exist in the bare
// mirror at all. It is a hard fault, not something to escalate around: the
// mirror is the only source of the new tip, and without one there is
// nothing to ingest.
var errBranchMissing = errors.New("orchestrator: target branch not found in mirror")

// errMirrorMissing means the bare mirror directory is absent or is not a
// git repository. Distinguished from errBranchMissing so an operator can
// tell "this repo was never cloned" from "this repo has no such branch".
var errMirrorMissing = errors.New("orchestrator: bare mirror missing or invalid on disk")

// errBatchProtocol means `git cat-file --batch` produced output this
// package could not parse. It should be unreachable; it exists so a
// surprise is a returned error rather than a slice index panic, which in
// this package's caller would kill the server process (loam-337).
var errBatchProtocol = errors.New("orchestrator: unparseable git cat-file --batch output")

// gitReader is contentReader's production implementation: it shells out to
// real git against a repo's bare mirror.
//
// Reads are done in TWO stages, deliberately. First `git ls-tree -r -z` at
// the ref gives every path's object id and type, NUL-delimited; then the
// object ids of just the wanted paths are fed to one `git cat-file --batch`
// on stdin. The alternative -- writing `<ref>:<path>` lines straight into
// cat-file --batch -- reads the same content in one process instead of
// two, but its stdin is newline-delimited, so a path containing a literal
// newline (legal in git, and the exact hazard internal/diffplan's own
// parseNameStatusZ documents) would be split into two bogus requests and
// silently desynchronize every subsequent response from its path. Object
// ids are hex, so the second stage's stdin cannot contain a delimiter at
// all. The ls-tree pass also reports each entry's TYPE, which is how a
// submodule gitlink is recognized and skipped rather than fetched as if it
// were a blob.
//
// The git plumbing (isolated --git-dir, no ext-diff, an explicit minimal
// environment, a WaitDelay on cancellation) is carried over from
// internal/gitdiff and internal/diffplan, which established it empirically;
// see internal/diffplan's package doc comment for the full rationale of
// each flag. It is duplicated rather than imported because neither package
// exports any of it.
type gitReader struct {
	logger *slog.Logger
}

// newGitReader builds a gitReader. logger must be non-nil.
func newGitReader(logger *slog.Logger) *gitReader {
	return &gitReader{logger: logger}
}

// ResolveRef returns the commit the mirror currently has for branch --
// the new_ref the whole ingest is relative to. It resolves
// refs/heads/<branch> explicitly rather than the bare branch name so a tag
// or a remote-tracking ref sharing the name can never be picked up
// instead: internal/mirrorsync fetches upstream branches into
// refs/heads/* (its allRefsRefspec), so that is the only namespace an
// indexed branch is ever in.
func (r *gitReader) ResolveRef(ctx context.Context, mirrorDir, branch string) (string, error) {
	out, err := r.run(ctx, mirrorDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", err
	}
	switch out.exitCode {
	case 0:
		ref := strings.TrimSpace(string(out.stdout))
		if ref == "" {
			return "", fmt.Errorf("%s: %w", branch, errBranchMissing)
		}
		return ref, nil
	case 1:
		return "", fmt.Errorf("%s: %w", branch, errBranchMissing)
	default:
		if isMirrorMissingStderr(out.stderr) {
			return "", fmt.Errorf("%s: %w", mirrorDir, errMirrorMissing)
		}
		return "", fmt.Errorf("git rev-parse %s exited %d: %s", branch, out.exitCode, strings.TrimSpace(out.stderr))
	}
}

// ReadFiles returns the blob content of each of paths at ref, in the order
// given. A path that is not a blob at ref -- a submodule gitlink, or a
// path that disappeared between the plan being computed and this read --
// is logged and omitted from the result rather than failing the ingest:
// the plan is a snapshot of a moment and the mirror is live, and a missing
// path simply has nothing to reparse.
//
// An empty paths makes no subprocess call at all.
func (r *gitReader) ReadFiles(ctx context.Context, mirrorDir, ref string, paths []string) ([]File, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	blobs, err := r.listBlobs(ctx, mirrorDir, ref)
	if err != nil {
		return nil, err
	}
	wanted := make([]string, 0, len(paths))
	oids := make([]string, 0, len(paths))
	for _, p := range paths {
		oid, ok := blobs[p]
		if !ok {
			r.logger.WarnContext(ctx, "skipping planned file that is not a blob at the ingested ref", "file", p, "ref", ref)
			continue
		}
		wanted = append(wanted, p)
		oids = append(oids, oid)
	}
	if len(oids) == 0 {
		return nil, nil
	}
	contents, err := r.readBlobs(ctx, mirrorDir, oids)
	if err != nil {
		return nil, err
	}
	if len(contents) != len(wanted) {
		return nil, fmt.Errorf("%w: asked for %d blobs, got %d", errBatchProtocol, len(wanted), len(contents))
	}
	files := make([]File, len(wanted))
	for i, p := range wanted {
		files[i] = File{Path: p, Content: contents[i]}
	}
	return files, nil
}

// listBlobs maps every BLOB path in the tree at ref to its object id.
// Non-blob entries (a submodule's commit gitlink) are omitted, which is
// what makes ReadFiles skip them.
//
// `-z` is not optional here for the same reason internal/diffplan's
// parseNameStatusZ documents: without it git C-quotes any path containing
// a control character, so a filename with a literal newline would come
// back escaped inside double quotes and silently fail to match the path
// the planner produced.
func (r *gitReader) listBlobs(ctx context.Context, mirrorDir, ref string) (map[string]string, error) {
	out, err := r.run(ctx, mirrorDir, "ls-tree", "-r", "-z", ref)
	if err != nil {
		return nil, err
	}
	if out.exitCode != 0 {
		if isMirrorMissingStderr(out.stderr) {
			return nil, fmt.Errorf("%s: %w", mirrorDir, errMirrorMissing)
		}
		return nil, fmt.Errorf("git ls-tree %s exited %d: %s", ref, out.exitCode, strings.TrimSpace(out.stderr))
	}
	return parseLsTreeZ(out.stdout)
}

// parseLsTreeZ parses `git ls-tree -r -z <ref>` output: NUL-terminated
// records of "<mode> SP <type> SP <object> TAB <path>". Only blob records
// are returned. A record that does not have that shape is skipped rather
// than treated as a path -- there is no positional indexing here that a
// malformed record could walk off the end of.
func parseLsTreeZ(stdout []byte) (map[string]string, error) {
	blobs := map[string]string{}
	for _, record := range bytes.Split(bytes.TrimSuffix(stdout, []byte{0}), []byte{0}) {
		if len(record) == 0 {
			continue
		}
		meta, path, found := strings.Cut(string(record), "\t")
		if !found {
			return nil, fmt.Errorf("%w: ls-tree record %q has no tab separator", errBatchProtocol, record)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: ls-tree record %q has %d metadata fields, want 3", errBatchProtocol, record, len(fields))
		}
		if fields[1] != "blob" {
			continue
		}
		blobs[path] = fields[2]
	}
	return blobs, nil
}

// readBlobs reads every object id in oids through ONE `git cat-file
// --batch` process, returning their contents in the same order. Object ids
// are hex, so feeding them on newline-delimited stdin is unambiguous no
// matter what the paths they came from looked like.
//
// --buffer is passed so git does not flush per request; the whole request
// list is written and stdin closed before any response is read, which is
// only safe because that request list is bounded by the plan's file count
// and each request is 41 bytes. Streaming request/response in lockstep
// would avoid even that, at the cost of a pipe-deadlock hazard this
// package has no need to take on.
func (r *gitReader) readBlobs(ctx context.Context, mirrorDir string, oids []string) ([][]byte, error) {
	var stdin bytes.Buffer
	for _, oid := range oids {
		stdin.WriteString(oid)
		stdin.WriteByte('\n')
	}
	out, err := r.runWithStdin(ctx, mirrorDir, stdin.Bytes(), "cat-file", "--batch", "--buffer")
	if err != nil {
		return nil, err
	}
	if out.exitCode != 0 {
		if isMirrorMissingStderr(out.stderr) {
			return nil, fmt.Errorf("%s: %w", mirrorDir, errMirrorMissing)
		}
		return nil, fmt.Errorf("git cat-file --batch exited %d: %s", out.exitCode, strings.TrimSpace(out.stderr))
	}
	return parseCatFileBatch(out.stdout, len(oids))
}

// parseCatFileBatch parses `git cat-file --batch` output: per request, a
// header line "<oid> SP <type> SP <size> LF", then exactly <size> bytes of
// content, then one more LF. want is how many responses are expected, so a
// truncated stream is a named error rather than a short slice the caller
// silently mispairs with its paths.
func parseCatFileBatch(stdout []byte, want int) ([][]byte, error) {
	reader := bufio.NewReader(bytes.NewReader(stdout))
	contents := make([][]byte, 0, want)
	for len(contents) < want {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("%w: reading response %d of %d: %w", errBatchProtocol, len(contents)+1, want, err)
		}
		fields := strings.Fields(strings.TrimSuffix(header, "\n"))
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: header %q has %d fields, want 3", errBatchProtocol, strings.TrimSuffix(header, "\n"), len(fields))
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 {
			return nil, fmt.Errorf("%w: header %q has an unusable size", errBatchProtocol, strings.TrimSuffix(header, "\n"))
		}
		content := make([]byte, size)
		if _, err := io.ReadFull(reader, content); err != nil {
			return nil, fmt.Errorf("%w: reading %d bytes of object %s: %w", errBatchProtocol, size, fields[0], err)
		}
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("%w: missing record terminator after object %s: %w", errBatchProtocol, fields[0], err)
		}
		contents = append(contents, content)
	}
	return contents, nil
}

// isMirrorMissingStderr reports whether stderr is git's own "not a git
// repository" complaint about a bad --git-dir, checked case-insensitively
// for the reason internal/diffplan's function of the same name documents:
// different git subcommands word it with different capitalization.
func isMirrorMissingStderr(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "not a git repository")
}

// gitOutput is one subprocess invocation's classified result, matching
// internal/diffplan's type of the same name.
type gitOutput struct {
	stdout   []byte
	exitCode int
	stderr   string
}

// run executes one git subcommand against mirrorDir with no stdin.
func (r *gitReader) run(ctx context.Context, mirrorDir string, args ...string) (gitOutput, error) {
	return r.runWithStdin(ctx, mirrorDir, nil, args...)
}

// runWithStdin executes one git subcommand against mirrorDir (via
// --git-dir, never -C: the latter performs upward repository discovery and
// can silently operate on an enclosing repository instead of failing),
// isolated from the host and user gitconfig, optionally feeding it stdin.
func (r *gitReader) runWithStdin(ctx context.Context, mirrorDir string, stdin []byte, args ...string) (gitOutput, error) {
	home, err := os.MkdirTemp("", "loam-ingest-*")
	if err != nil {
		return gitOutput{}, fmt.Errorf("creating isolated git environment: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	fullArgs := append([]string{"--no-pager", "-c", "credential.helper=", "--git-dir=" + mirrorDir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.WaitDelay = subprocessWaitDelay
	cmd.Env = gitEnv(home)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf bytes.Buffer
	errBuf := &cappedBuffer{max: maxStderrBytes}
	cmd.Stdout = &outBuf
	cmd.Stderr = errBuf
	runErr := cmd.Run()
	if runErr == nil {
		return gitOutput{stdout: outBuf.Bytes(), exitCode: 0, stderr: errBuf.buf.String()}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return gitOutput{stdout: outBuf.Bytes(), exitCode: exitErr.ExitCode(), stderr: errBuf.buf.String()}, nil
	}
	return gitOutput{}, fmt.Errorf("running git %v: %w", args, runErr)
}

// gitEnv builds the environment for one git subprocess invocation --
// identical in shape and rationale to internal/gitdiff's and
// internal/diffplan's own gitEnv: an explicit minimal list (never
// os.Environ() plus additions), so no system, user-global, or
// ambient-pointed gitconfig is ever read.
func gitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "unused-global-gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_PAGER=cat",
		"GIT_TRACE=0",
		"GIT_TRACE_CURL=0",
		"GIT_CURL_VERBOSE=0",
		"GIT_TRACE_PACKET=0",
		"GIT_TRACE_PACK_ACCESS=0",
		"GIT_TRACE_SETUP=0",
	}
}

// cappedBuffer is an io.Writer retaining only the first max bytes ever
// written, matching internal/gitdiff's and internal/diffplan's own -- used
// here only for stderr, never for stdout, since every stdout this package
// reads must be read in full to be parsed correctly.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

// Write implements io.Writer, always reporting every byte written (so a
// subprocess never blocks on a full pipe) while retaining only the first
// max bytes.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		c.buf.Write(p[:room])
	}
	return len(p), nil
}
