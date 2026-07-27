package main

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleEnvelope = `{
  "ingested": [{"repo":"git/fixture-polyglot","target":"main","ref":"abc123","at":"2026-07-27T00:00:00Z"}],
  "truncated": false,
  "results": [
    {"repo":"git/fixture-polyglot","file":"pkg/auth/auth.go","line":10,"symbol":"Login","kind":"function"},
    {"repo":"git/fixture-polyglot","file":"pkg/report/report.go","line":9,"symbol":"Summarize","kind":"function"}
  ]
}`

func checkEnvelope(t *testing.T, payload string, args ...string) error {
	t.Helper()
	return runCheckEnvelope(args, strings.NewReader(payload), io.Discard)
}

func checkJobs(t *testing.T, payload string, args ...string) error {
	t.Helper()
	return runCheckJobs(args, strings.NewReader(payload), io.Discard)
}

func TestCheckEnvelope_AllAssertionsHold(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkEnvelope(t, sampleEnvelope,
		"-ref", "abc123", "-repo", "git/fixture-polyglot", "-target", "main",
		"-min-results", "2", "-first-file", "pkg/auth/auth.go",
		"-want-files", "pkg/report/report.go", "-want-symbols", "Login,Summarize",
		"-deny-symbols", "LegacySignIn"))
}

func TestCheckEnvelope_WrongIngestedRefFails(t *testing.T) {
	t.Parallel()
	err := checkEnvelope(t, sampleEnvelope, "-ref", "deadbeef")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 assertion(s) failed")
}

// TestCheckEnvelope_RefIsNotMatchedAsASubstring is the reason these
// assertions are not greps: a substring search over the payload would
// happily match a SHA that appears anywhere at all, including in a
// snippet, rather than in the ingested field that is being asserted.
func TestCheckEnvelope_RefIsNotMatchedAsASubstring(t *testing.T) {
	t.Parallel()
	payload := `{"ingested":[{"repo":"r","target":"main","ref":"aaa","at":"t"}],"results":[{"file":"f","snippet":"bbb"}]}`
	require.Error(t, checkEnvelope(t, payload, "-ref", "bbb"))
}

func TestCheckEnvelope_EveryIngestedEntryIsChecked(t *testing.T) {
	t.Parallel()
	payload := `{"ingested":[{"repo":"a","target":"main","ref":"good","at":"t"},{"repo":"b","target":"main","ref":"stale","at":"t"}],"results":[]}`
	err := checkEnvelope(t, payload, "-ref", "good")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 assertion(s) failed")
}

func TestCheckEnvelope_EmptyIngestedFails(t *testing.T) {
	t.Parallel()
	require.Error(t, checkEnvelope(t, `{"ingested":[],"results":[]}`, "-ref", "abc123"))
}

func TestCheckEnvelope_MissingIngestedAtFails(t *testing.T) {
	t.Parallel()
	require.Error(t, checkEnvelope(t, `{"ingested":[{"repo":"r","target":"main","ref":"abc123","at":""}],"results":[]}`, "-ref", "abc123"))
}

func TestCheckEnvelope_FirstFileIsPositional(t *testing.T) {
	t.Parallel()
	// The auth row is present but second, which is exactly the failure a
	// "does the payload mention auth.go" check would miss.
	require.Error(t, checkEnvelope(t, sampleEnvelope, "-first-file", "pkg/report/report.go"))
	require.NoError(t, checkEnvelope(t, sampleEnvelope, "-first-file", "pkg/auth/auth.go"))
}

func TestCheckEnvelope_NoResultsCannotSatisfyFirstFile(t *testing.T) {
	t.Parallel()
	require.Error(t, checkEnvelope(t, `{"ingested":[],"results":[]}`, "-first-file", "pkg/auth/auth.go"))
}

func TestCheckEnvelope_DenySymbolCatchesAStaleIndex(t *testing.T) {
	t.Parallel()
	payload := `{"ingested":[],"results":[{"file":"pkg/legacy/legacy.go","symbol":"LegacySignIn"}]}`
	require.Error(t, checkEnvelope(t, payload, "-deny-symbols", "LegacySignIn"))
}

func TestCheckEnvelope_MinResults(t *testing.T) {
	t.Parallel()
	require.Error(t, checkEnvelope(t, sampleEnvelope, "-min-results", "3"))
	require.NoError(t, checkEnvelope(t, sampleEnvelope, "-min-results", "2"))
}

func TestCheckEnvelope_MalformedJSONIsAnError(t *testing.T) {
	t.Parallel()
	require.Error(t, checkEnvelope(t, "{not json", "-min-results", "0"))
}

const sampleJobs = `{"jobs":[
  {"repo":"git/fixture-polyglot","targetBranch":"main","kind":"INGEST_KIND_INCREMENTAL","status":"INGEST_STATUS_SUCCEEDED"},
  {"repo":"git/fixture-polyglot","targetBranch":"main","kind":"INGEST_KIND_FULL","status":"INGEST_STATUS_SUCCEEDED"}
]}`

func TestCheckJobs_DrainedQueuePasses(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkJobs(t, sampleJobs, "-min-jobs", "2", "-all-succeeded",
		"-want-kinds", "INGEST_KIND_FULL,INGEST_KIND_INCREMENTAL"))
}

// TestCheckJobs_RunningJobIsNotDrained is the whole reason the drain is
// polled on job status rather than on repos.sync_state: a job still in
// flight must read as not-yet-drained, never as quiescent.
func TestCheckJobs_RunningJobIsNotDrained(t *testing.T) {
	t.Parallel()
	payload := `{"jobs":[{"kind":"INGEST_KIND_INCREMENTAL","status":"INGEST_STATUS_RUNNING"},{"kind":"INGEST_KIND_FULL","status":"INGEST_STATUS_SUCCEEDED"}]}`
	require.Error(t, checkJobs(t, payload, "-min-jobs", "2", "-all-succeeded"))
}

// TestCheckJobs_OnlyTheEnrollJobIsNotEnough covers the other race: the
// enrollment ingest can be finished long before the sync tick that reacts
// to the upstream rewrite has even enqueued its own job, so "everything
// succeeded" alone would be a false green.
func TestCheckJobs_OnlyTheEnrollJobIsNotEnough(t *testing.T) {
	t.Parallel()
	payload := `{"jobs":[{"kind":"INGEST_KIND_FULL","status":"INGEST_STATUS_SUCCEEDED"}]}`
	require.Error(t, checkJobs(t, payload, "-min-jobs", "2", "-all-succeeded"))
}

func TestCheckJobs_FailedJobIsReportedWithItsError(t *testing.T) {
	t.Parallel()
	payload := `{"jobs":[{"kind":"INGEST_KIND_FULL","status":"INGEST_STATUS_FAILED","attempts":2,"error":"boom"}]}`
	var out strings.Builder
	err := runCheckJobs([]string{"-min-jobs", "1", "-all-succeeded"}, strings.NewReader(payload), &out)
	require.Error(t, err)
	assert.Contains(t, out.String(), "boom")
	assert.Contains(t, out.String(), "attempts=2")
}

func TestCheckJobs_MissingKindFails(t *testing.T) {
	t.Parallel()
	require.Error(t, checkJobs(t, sampleJobs, "-want-kinds", "INGEST_KIND_NOPE"))
}

func TestSplit_IgnoresEmptyEntries(t *testing.T) {
	t.Parallel()
	assert.Empty(t, split(""))
	assert.Equal(t, []string{"a", "b"}, split("a, b,"))
}
