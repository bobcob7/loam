package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestSlogErrorHandler_RendersOneSDKErrorAsOneJSONObject covers the handler as
// a VALUE, with no global anywhere near it. That separation is the point of
// slogErrorHandler being a plain struct: everything about what gets written --
// format, level, field name -- is pinned here, cheaply and in microseconds,
// and the expensive subprocess test below is left with the one claim that
// genuinely needs a process to itself.
func TestSlogErrorHandler_RendersOneSDKErrorAsOneJSONObject(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	handler := slogErrorHandler{logger: slog.New(slog.NewJSONHandler(&out, nil))}
	handler.Handle(errors.New("traces export: context deadline exceeded"))
	lines := nonBlankLines(out.String())
	require.Len(t, lines, 1)
	record := requireJSONObject(t, lines[0])
	assert.Equal(t, sdkErrorMessage, record["msg"])
	assert.Equal(t, "ERROR", record["level"])
	assert.Equal(t, "traces export: context deadline exceeded", record["error"])
}

// TestSlogErrorHandler_AMultiLineSDKErrorStaysOneRecord is the property that
// actually matters downstream, and it is not the same claim as the test above.
// The SDK joins errors (errors.Join in the exporter's shutdown path, the
// partial-success handling in otlptracehttp), and a joined error's Error()
// contains newlines. Loki splits on newlines, so a handler that wrote the
// error text raw would turn ONE failure into one structured line plus N
// orphaned fragments -- which is the same operator-facing symptom this whole
// change exists to remove, arriving by a different route.
func TestSlogErrorHandler_AMultiLineSDKErrorStaysOneRecord(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	handler := slogErrorHandler{logger: slog.New(slog.NewJSONHandler(&out, nil))}
	handler.Handle(errors.Join(errors.New("traces export failed"), errors.New("metrics export failed")))
	lines := nonBlankLines(out.String())
	require.Len(t, lines, 1, "a joined error must not become several stderr lines")
	record := requireJSONObject(t, lines[0])
	assert.Contains(t, record["error"], "traces export failed")
	assert.Contains(t, record["error"], "metrics export failed")
}

// TestSlogErrorHandler_DropsANilError pins the guard rather than the absence
// of a crash: upstream's default handler renders otel.Handle(nil) as the
// literal "<nil>", and an ERROR record whose error field is empty is a page
// with nothing behind it.
func TestSlogErrorHandler_DropsANilError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	handler := slogErrorHandler{logger: slog.New(slog.NewJSONHandler(&out, nil))}
	handler.Handle(nil)
	assert.Empty(t, nonBlankLines(out.String()))
}

// The subprocess fixture below. Everything from here down exists to answer one
// question that cannot be answered in-process without mutating a global for
// the whole test binary: when the SDK's OWN error path fires, does anything
// non-JSON reach file descriptor 2?
const (
	// childModeEnv both selects the child's behaviour and gates it: with the
	// variable unset, TestBlackHoledCollectorChild skips, so it costs nothing
	// in a normal run of this package.
	childModeEnv = "LOAM_TELEMETRY_ERRORHANDLER_CHILD"
	// childModeSlog installs InstallErrorHandler; childModeDefault leaves the
	// SDK's default handler in place. The two are the same program otherwise,
	// which is what makes the pair a discriminator instead of two assertions.
	childModeSlog    = "slog-handler-installed"
	childModeDefault = "sdk-default-handler"
	// childCollectorHitMsg is logged by the fixture collector the first time
	// the exporter actually delivers bytes to it. It is the answer to "what
	// does this fixture make indistinguishable": without it, a child that
	// never managed to export anything -- wrong endpoint, sampler dropping
	// every span, batch never reaching its threshold -- would produce a clean
	// all-JSON stderr and PASS the slog-mode assertion for entirely the wrong
	// reason.
	childCollectorHitMsg = "black-holed collector accepted an export attempt"
	// childLifetime must stay comfortably below the child's -test.timeout: a
	// child killed by the test timeout dumps every goroutine stack to STDERR,
	// which is non-JSON, which would fail the slog-mode assertion for a
	// reason that has nothing to do with the code under test.
	//
	// It must ALSO stay below the periodic metric reader's 60s default
	// interval, and that bound was earned rather than guessed. At exactly 60s
	// the reader wakes, tries its first metric export, finds the fixture
	// collector already torn down, and hands the resulting error to
	// otel.Handle -- which is a real SDK error arriving for a reason that has
	// nothing to do with a failed TRACE export. A mutant that made the
	// collector answer 200 (every export succeeding, so the slog-mode test
	// should have failed) passed on exactly that accident. Ending the child
	// before the reader's first tick removes the confound at the source.
	childLifetime  = 40 * time.Second
	childTestLimit = 5 * time.Minute
	// exportFailureDeadline is the second half of that fix, and the durable
	// half: the decisive line must arrive while the child is RUNNING, not as
	// it is being dismantled. The exporter's own budget is retryMaxElapsed
	// (10s) plus a final exportRequestTimeout (2s), so a genuine steady-state
	// failure surfaces in well under 20s. This is loose enough not to flake on
	// a loaded runner, and tight enough that an error which only appears at
	// teardown is reported rather than accepted.
	exportFailureDeadline = 30 * time.Second
	// parentDeadline bounds the whole subprocess, so a fixture that never
	// fails is a test failure rather than a hung suite.
	parentDeadline = 2 * time.Minute
)

// TestInstallErrorHandler_ASDKExportFailurePutsNoNonJSONLineOnStderr is this
// bead's assertion, and its shape is deliberate: it asserts the ABSENCE of
// malformed output, never the presence of a particular sentence. The SDK's
// wording ("traces export: context deadline exceeded: Post ...") is upstream's
// and will change under us; "every line on fd 2 is a JSON object" is ours and
// will not.
//
// It runs in a subprocess for one reason. otel.SetErrorHandler is
// process-wide, so installing it in this test binary would leave it installed
// for every other test in this package -- including
// TestNew_EndpointPresenceDecidesWhetherAnySDKMachineryExists, whose whole
// design is that nothing under test mutates state shared with its neighbours.
// A child process is the only place the global can be observed without being
// inflicted on anyone.
func TestInstallErrorHandler_ASDKExportFailurePutsNoNonJSONLineOnStderr(t *testing.T) {
	t.Parallel()
	lines := runBlackHoleChild(t, childModeSlog, func(line string) bool {
		record, ok := decodeJSONObject(line)
		return ok && record["msg"] == sdkErrorMessage
	})
	require.NotEmpty(t, lines, "the child wrote nothing to stderr; it probably failed before it could export")
	// The proof that a failure actually happened, and the reason this is not
	// a test of an empty stream: the exporter reached the collector and the
	// collector answered nothing.
	require.True(t, containsRecord(lines, childCollectorHitMsg),
		"the exporter never delivered bytes to the black hole, so no export can have failed:\n%s", strings.Join(lines, "\n"))
	for _, line := range lines {
		_, ok := decodeJSONObject(line)
		assert.True(t, ok, "non-JSON line on stderr: %q\nfull stderr:\n%s", line, strings.Join(lines, "\n"))
	}
	routed, ok := findRecord(lines, sdkErrorMessage)
	require.True(t, ok, "the SDK's error never reached slog:\n%s", strings.Join(lines, "\n"))
	// The error TEXT is not asserted -- only that one was carried. Anything
	// stronger pins upstream's wording into this repository's test suite.
	assert.NotEmpty(t, routed["error"], "the routed record carried no error text")
	assert.Equal(t, "ERROR", routed["level"])
}

// TestSDKDefaultHandler_ASDKExportFailureWritesNonJSONToStderr is the control,
// and without it the test above is close to worthless: a fixture that silently
// failed to drive any export at all would also produce an all-JSON stderr.
// This runs the identical child with the identical black hole and the ONE line
// of difference -- InstallErrorHandler not called -- and requires the malformed
// output to appear. It is simultaneously the reproduction of loam-0jle and the
// proof that the fixture bites.
func TestSDKDefaultHandler_ASDKExportFailureWritesNonJSONToStderr(t *testing.T) {
	t.Parallel()
	lines := runBlackHoleChild(t, childModeDefault, func(line string) bool {
		_, ok := decodeJSONObject(line)
		return !ok
	})
	require.NotEmpty(t, lines, "the child wrote nothing to stderr; it probably failed before it could export")
	require.True(t, containsRecord(lines, childCollectorHitMsg),
		"the exporter never delivered bytes to the black hole, so no export can have failed:\n%s", strings.Join(lines, "\n"))
	var malformed []string
	for _, line := range lines {
		if _, ok := decodeJSONObject(line); !ok {
			malformed = append(malformed, line)
		}
	}
	assert.NotEmpty(t, malformed,
		"the SDK's default handler wrote no non-JSON line, so the sibling test proves nothing:\n%s", strings.Join(lines, "\n"))
}

// TestBlackHoledCollectorChild is the subprocess body, not a test of its own.
// It skips unless re-executed by runBlackHoleChild, which is why it can sit in
// the ordinary test file without slowing a normal run down.
//
// It never calls Provider.Shutdown, and that is a deliberate saving rather
// than an oversight: Shutdown would drain the METRIC pipeline against the same
// black hole afterwards, paying the exporter's full retry budget a second time
// for an error the trace side has already produced. Filling one batch is
// enough to make the batch span processor export, and export is the only thing
// this fixture needs to fail.
func TestBlackHoledCollectorChild(t *testing.T) {
	mode := os.Getenv(childModeEnv)
	if mode == "" {
		t.Skip("subprocess fixture; runs only when re-executed with " + childModeEnv + " set")
	}
	// Everything this child says goes to stderr as JSON, in BOTH modes, so
	// the only possible source of a malformed line is the SDK itself.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if mode == childModeSlog {
		InstallErrorHandler(logger)
	}
	endpoint := hangingCollector(t, logger)
	provider, err := New(context.Background(), Config{
		Endpoint:       endpoint,
		ServiceName:    "loam-errorhandler-child",
		ServiceVersion: "v0.0.0-test",
		SampleRatio:    1,
	}, logger)
	require.NoError(t, err)
	tracer := provider.TracerProvider().Tracer("errorhandler-child")
	// One more than a full batch. This is a LATENCY optimisation and nothing
	// more, stated plainly because the obvious reading is wrong: the batch
	// span processor also exports on its 5s timer, so a single span would
	// still reach the collector and still fail -- measured, as a surviving
	// mutant. Filling the buffer makes the processor export at once, which
	// takes those 5 seconds off both subprocess tests. Referencing the SDK's
	// own constant means an upstream change to the default shows up as a
	// compile-time fact rather than as a test that quietly starts waiting.
	for range sdktrace.DefaultMaxExportBatchSize + 1 {
		_, span := tracer.Start(context.Background(), "span-that-can-never-be-exported")
		span.End()
	}
	// The parent kills this process the moment it has read what it needs;
	// this bound only covers the case where nothing ever arrives, and it must
	// expire well before -test.timeout so the exit is silent.
	time.Sleep(childLifetime)
}

// hangingCollector is p56y's black hole with one addition: it ACCEPTS the
// connection and reads the request before hanging, rather than leaving the
// connection in the listen backlog.
//
// The addition is what makes an export failure provable. A backlog-only
// listener is indistinguishable, from inside the test, from a listener nobody
// ever dialled -- so "stderr contained no malformed line" could mean "the
// handler worked" or "no export was ever attempted". Reading the request and
// saying so once turns the second reading into a test failure.
//
// It still never RESPONDS, and it never closes the connection either. Closing
// would hand the exporter an EOF, which is a fast, different failure; holding
// the connection open is what makes the client hit its own request timeout,
// which is the shape a wedged collector actually has.
func hangingCollector(t *testing.T, logger *slog.Logger) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	var announced atomic.Bool
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				buffer := make([]byte, 4096)
				n, _ := conn.Read(buffer)
				if n > 0 && !announced.Swap(true) {
					// The byte COUNT, never the bytes: this is an OTLP
					// payload, and spans are not this fixture's to print.
					logger.Info(childCollectorHitMsg, "bytes", n)
				}
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

// runBlackHoleChild re-executes this test binary as the subprocess fixture and
// returns every non-blank line the child put on stderr, stopping as soon as
// stop reports that the decisive line has arrived.
//
// Streaming rather than waiting for exit is what keeps the test to roughly the
// exporter's retry budget instead of childLifetime, and it costs nothing in
// rigour: any malformed line the child produced BEFORE the decisive one is
// already in the returned slice, and the caller asserts over all of them.
func runBlackHoleChild(t *testing.T, mode string, stop func(line string) bool) []string {
	t.Helper()
	// context.WithoutCancel: t.Context() is already cancelled by the time
	// cleanups run, and CommandContext would then kill the child during
	// cleanup rather than at the deadline this test actually wants.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), parentDeadline)
	t.Cleanup(cancel)
	child := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestBlackHoledCollectorChild$",
		"-test.timeout="+childTestLimit.String(),
	)
	child.Env = append(os.Environ(), childModeEnv+"="+mode)
	// The testing framework's own output goes to stdout, so discarding it
	// keeps stderr carrying nothing but the child's slog records and whatever
	// the SDK decides to write -- which is the entire measurement.
	child.Stdout = io.Discard
	stderr, err := child.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	// Kill BEFORE Wait, and register it as a cleanup rather than a defer, so
	// it still runs when an assertion fails the test early. Wait closes the
	// pipe, so it must not run until reading is finished.
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	started := time.Now()
	var lines []string
	stopped := false
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
		if stop(line) {
			stopped = true
			break
		}
	}
	elapsed := time.Since(started)
	require.True(t, stopped,
		"the child (%s) exited without ever producing the decisive line, so the SDK's error path was never reached:\n%s",
		mode, strings.Join(lines, "\n"))
	require.Less(t, elapsed, exportFailureDeadline,
		"the child (%s) took %s to report a failed export; an error that only appears as the process winds down is not the steady-state failure this test is about",
		mode, elapsed)
	return lines
}

func decodeJSONObject(line string) (map[string]any, bool) {
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		return nil, false
	}
	// A bare scalar is valid JSON but is not a log record; slog's JSON
	// handler emits an object per line and nothing else may.
	return record, record != nil
}

func requireJSONObject(t *testing.T, line string) map[string]any {
	t.Helper()
	record, ok := decodeJSONObject(line)
	require.True(t, ok, "not a JSON object: %q", line)
	return record
}

func findRecord(lines []string, message string) (map[string]any, bool) {
	for _, line := range lines {
		if record, ok := decodeJSONObject(line); ok && record["msg"] == message {
			return record, true
		}
	}
	return nil, false
}

func containsRecord(lines []string, message string) bool {
	_, ok := findRecord(lines, message)
	return ok
}

func nonBlankLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
