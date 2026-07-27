package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/bobcob7/loam/internal/hooksocket"
)

// parseUpdates reads git's own pre-receive hook stdin format: one
// "<old-sha> <new-sha> <ref>" line per proposed ref update in the whole
// push, terminated at EOF (git-scm.com's own githooks(5) documentation for
// pre-receive; docs/git-spec.md cites this shape but does not itself spell
// out the exact three-field-per-line wire format). Blank lines are
// skipped; any non-blank line that does not split into exactly three
// whitespace-separated fields is a malformed invocation this stub cannot
// make sense of, and is reported as an error so the caller fails the whole
// push closed rather than guessing at a partial parse.
func parseUpdates(r io.Reader) ([]hooksocket.RefUpdateWire, error) {
	var updates []hooksocket.RefUpdateWire
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed pre-receive line %q: want exactly 3 fields, got %d", line, len(fields))
		}
		updates = append(updates, hooksocket.RefUpdateWire{OldSHA: fields[0], NewSHA: fields[1], Ref: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading pre-receive stdin: %w", err)
	}
	return updates, nil
}
