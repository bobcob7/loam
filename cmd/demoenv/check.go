package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// This file holds demo:m3's assertions. They live in Go, not in shell,
// because every one of them is a claim about a specific field of a
// specific JSON document, and the shell equivalents (grep for a substring
// anywhere in the payload) pass for the wrong reasons: a `ref` grep
// matches the same SHA appearing under `at`, a `file` grep matches the
// file named in a snippet rather than in the row that named it, and a
// "first result" check is not expressible at all. Exit status is the
// contract -- non-zero on any failed assertion -- so the Taskfile's
// errexit does the rest.

// envelope is the shared {ingested, truncated, results} shape every
// `loam graph <subquery>` and `loam search` emits (internal/cli's
// graphQueryOutput and searchOutput). results' rows differ per subquery,
// so they are decoded as generic objects and only the fields common to
// every row shape -- file, symbol -- are addressed by name.
type envelope struct {
	Ingested []struct {
		Repo   string `json:"repo"`
		Target string `json:"target"`
		Ref    string `json:"ref"`
		At     string `json:"at"`
	} `json:"ingested"`
	Truncated bool             `json:"truncated"`
	Results   []map[string]any `json:"results"`
}

// runCheckEnvelope asserts over one graph/search envelope read from stdin.
func runCheckEnvelope(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("check-envelope", flag.ContinueOnError)
	label := fs.String("label", "envelope", "what is being checked, echoed into pass/fail messages")
	wantRef := fs.String("ref", "", "assert every ingested[].ref equals this commit")
	wantRepo := fs.String("repo", "", "assert every ingested[].repo equals this repo")
	wantTarget := fs.String("target", "", "assert every ingested[].target equals this branch")
	minResults := fs.Int("min-results", 0, "assert at least this many results")
	firstFile := fs.String("first-file", "", "assert results[0].file equals this path")
	wantFiles := fs.String("want-files", "", "comma-separated: assert some result's file equals each")
	wantSymbols := fs.String("want-symbols", "", "comma-separated: assert some result's symbol equals each")
	denySymbols := fs.String("deny-symbols", "", "comma-separated: assert NO result's symbol equals any")
	if err := fs.Parse(args); err != nil {
		return err
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return fmt.Errorf("decoding %s: %w (payload: %s)", *label, err, truncate(string(payload)))
	}
	var failures []string
	// The ingested envelope is asserted first and unconditionally over
	// EVERY entry, not just the first: this is the bead's explicit
	// acceptance point, and checking only ingested[0] would silently pass
	// a fan-out where one repo in scope reported a stale ref.
	if *wantRef != "" || *wantRepo != "" || *wantTarget != "" {
		if len(env.Ingested) == 0 {
			failures = append(failures, "ingested is empty: the response named no commit its index was built from")
		}
		for i, in := range env.Ingested {
			if *wantRef != "" && in.Ref != *wantRef {
				failures = append(failures, fmt.Sprintf("ingested[%d].ref is %q, want %q", i, in.Ref, *wantRef))
			}
			if *wantRepo != "" && in.Repo != *wantRepo {
				failures = append(failures, fmt.Sprintf("ingested[%d].repo is %q, want %q", i, in.Repo, *wantRepo))
			}
			if *wantTarget != "" && in.Target != *wantTarget {
				failures = append(failures, fmt.Sprintf("ingested[%d].target is %q, want %q", i, in.Target, *wantTarget))
			}
			if in.At == "" {
				failures = append(failures, fmt.Sprintf("ingested[%d].at is empty: the index reports no ingest time", i))
			}
		}
	}
	if len(env.Results) < *minResults {
		failures = append(failures, fmt.Sprintf("got %d results, want at least %d", len(env.Results), *minResults))
	}
	if *firstFile != "" {
		switch {
		case len(env.Results) == 0:
			failures = append(failures, fmt.Sprintf("no results at all, so none could rank first; want results[0].file == %q", *firstFile))
		case field(env.Results[0], "file") != *firstFile:
			failures = append(failures, fmt.Sprintf("results[0].file is %q, want %q (ranked-first assertion)", field(env.Results[0], "file"), *firstFile))
		}
	}
	for _, want := range split(*wantFiles) {
		if !anyField(env.Results, "file", want) {
			failures = append(failures, fmt.Sprintf("no result has file == %q; got %v", want, collect(env.Results, "file")))
		}
	}
	for _, want := range split(*wantSymbols) {
		if !anyField(env.Results, "symbol", want) {
			failures = append(failures, fmt.Sprintf("no result has symbol == %q; got %v", want, collect(env.Results, "symbol")))
		}
	}
	for _, deny := range split(*denySymbols) {
		if anyField(env.Results, "symbol", deny) {
			failures = append(failures, fmt.Sprintf("result has symbol == %q, which must not be in the index", deny))
		}
	}
	return report(stdout, *label, failures)
}

// jobsResponse is ListIngestJobs' response shape. protojson omits
// zero-valued fields, so an absent `status` decodes as "" and an absent
// `attempts` as 0 -- both handled below rather than assumed present.
type jobsResponse struct {
	Jobs []struct {
		Repo         string `json:"repo"`
		TargetBranch string `json:"targetBranch"`
		Kind         string `json:"kind"`
		Status       string `json:"status"`
		Attempts     int    `json:"attempts"`
		Error        string `json:"error"`
	} `json:"jobs"`
}

// runCheckJobs asserts over one ListIngestJobs response read from stdin.
//
// The predicate the demo polls with -- "at least N jobs exist AND every
// one of them succeeded" -- is chosen because it cannot race, which
// repos.sync_state cannot claim: under a live scheduler and ingest pool
// sync_state CYCLES idle -> syncing -> idle (loam-4q2), so a single
// sample of it cannot distinguish "never ran" from "mid-cycle". Job rows
// are different. They are never deleted, so the count only rises; a job
// reaches 'succeeded' only from the pool's own success transaction and
// never leaves it (only a 'failed' job is ever requeued); and a job still
// queued or running is simply not 'succeeded' yet. So the predicate is
// false before the work is done, true after, and never flickers back --
// which is exactly what makes it safe to poll and to trust once seen.
func runCheckJobs(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("check-jobs", flag.ContinueOnError)
	label := fs.String("label", "ingest jobs", "what is being checked, echoed into pass/fail messages")
	minJobs := fs.Int("min-jobs", 1, "assert at least this many jobs exist")
	allSucceeded := fs.Bool("all-succeeded", false, "assert every job's status is INGEST_STATUS_SUCCEEDED")
	wantKinds := fs.String("want-kinds", "", "comma-separated: assert some job has each kind (e.g. INGEST_KIND_FULL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	var resp jobsResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return fmt.Errorf("decoding %s: %w (payload: %s)", *label, err, truncate(string(payload)))
	}
	var failures []string
	if len(resp.Jobs) < *minJobs {
		failures = append(failures, fmt.Sprintf("got %d ingest jobs, want at least %d", len(resp.Jobs), *minJobs))
	}
	if *allSucceeded {
		for i, job := range resp.Jobs {
			if job.Status != "INGEST_STATUS_SUCCEEDED" {
				failures = append(failures, fmt.Sprintf("job[%d] (%s %s) status is %q attempts=%d error=%q, want INGEST_STATUS_SUCCEEDED",
					i, job.Kind, job.TargetBranch, job.Status, job.Attempts, job.Error))
			}
		}
	}
	for _, want := range split(*wantKinds) {
		found := false
		for _, job := range resp.Jobs {
			if job.Kind == want {
				found = true
				break
			}
		}
		if !found {
			failures = append(failures, fmt.Sprintf("no ingest job has kind %q", want))
		}
	}
	return report(stdout, *label, failures)
}

// report prints one PASS line or every failure and returns an error when
// any assertion failed.
func report(stdout io.Writer, label string, failures []string) error {
	if len(failures) == 0 {
		fmt.Fprintf(stdout, "PASS: %s\n", label)
		return nil
	}
	for _, failure := range failures {
		fmt.Fprintf(stdout, "FAIL: %s: %s\n", label, failure)
	}
	return errors.New(label + ": " + fmt.Sprint(len(failures)) + " assertion(s) failed")
}

// field reads a string field from a decoded result row, returning "" when
// it is absent or not a string (e.g. `refs` rows carry no `kind`).
func field(row map[string]any, name string) string {
	value, ok := row[name].(string)
	if !ok {
		return ""
	}
	return value
}

// anyField reports whether any row's named field equals want.
func anyField(rows []map[string]any, name, want string) bool {
	for _, row := range rows {
		if field(row, name) == want {
			return true
		}
	}
	return false
}

// collect gathers every row's named field, for a failure message that
// shows what was actually returned instead of only what was missing.
func collect(rows []map[string]any, name string) []string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, field(row, name))
	}
	return values
}

// split parses a comma-separated flag value, ignoring empty entries so a
// trailing comma or an unset flag yields nothing rather than an
// impossible-to-satisfy empty-string assertion.
func split(list string) []string {
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// truncate caps a payload echoed into a decode error, so a malformed
// multi-megabyte response does not bury the message that explains it.
func truncate(payload string) string {
	const max = 400
	if len(payload) <= max {
		return payload
	}
	return payload[:max] + "... (truncated)"
}
