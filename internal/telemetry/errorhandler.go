package telemetry

import (
	"log/slog"

	"go.opentelemetry.io/otel"
)

// sdkErrorMessage is the slog message every SDK-internal error is logged
// under. It is deliberately OURS and deliberately constant: the SDK's own
// wording ("traces export: context deadline exceeded: Post ...") is upstream's
// to change, so an operator's Loki query and this repository's tests both key
// on this string and put the SDK's text in a field instead.
const sdkErrorMessage = "opentelemetry sdk error"

// slogErrorHandler adapts *slog.Logger to otel.ErrorHandler.
//
// It is a plain value with one dependency, constructed by injection like
// everything else in this tree, so its BEHAVIOUR (what it writes, at what
// level, in what format) is testable without touching any global. Only
// InstallErrorHandler below does that, and only the composition root calls it.
type slogErrorHandler struct {
	logger *slog.Logger
}

// Handle implements otel.ErrorHandler.
//
// Level is Error, not Warn, even though a dropped span degrades observability
// rather than failing a request. Two reasons. The SDK already decided this is
// an error -- otel.Handle's contract is "irremediable events" -- and
// re-grading it here would be this translation layer overruling the only code
// that knows what happened. And the whole point of the exercise is that an
// operator staring at logs during an incident SEES that telemetry is lying to
// them; a Warn in a stream already full of Warns does not achieve that.
// Volume is not a counter-argument: against a black-holed collector the
// exporter's retry budget (retryMaxElapsed) means this fires roughly once per
// batch attempt, not once per span.
//
// A nil error is dropped rather than logged. otel.Handle(nil) is not something
// the SDK does today, but the upstream default handler would render it as the
// literal "<nil>", and a log line that reports an error while carrying none is
// strictly worse than no line at all.
func (h slogErrorHandler) Handle(err error) {
	if err == nil {
		return
	}
	h.logger.Error(sdkErrorMessage, "error", err.Error())
}

// InstallErrorHandler routes the OpenTelemetry SDK's own internal errors --
// an unreachable collector, a rejected payload, an exporter that gave up --
// into logger, and it is the ONLY OpenTelemetry global this repository
// installs. The argument for the exception belongs here, next to the call,
// because this package's doc comment argues the opposite for every other one.
//
// # Why a global at all
//
// otel.Handle is not reached through any value loam holds. The SDK calls it
// from inside its own goroutines -- the batch span processor's drain, the
// periodic reader's export loop -- with no reference to anything this package
// constructed. There is no seam to inject through: the process-wide handler is
// the only surface the SDK offers. So the choice is not "global versus
// constructor injection", which is the choice loam-p56y faced for the tracer
// provider and answered with injection. It is "global versus nothing", and
// "nothing" means the default handler stays installed and keeps writing
// log.Print output to stderr.
//
// # Why that is acceptable here and not for the providers
//
// An error handler is WRITE-ONLY. Apply loam-9v9s's test for an observability
// sink -- delete the field entirely and ask whether any caller gets a
// different result or the server takes a different action -- and the answer is
// that nothing changes except where bytes land. Nothing in loam ever calls
// otel.GetErrorHandler, no branch reads it, and no behaviour depends on which
// handler is installed. A TracerProvider global fails that test: instrumentation
// would RESOLVE it to decide what to build spans with, which is exactly why
// "telemetry is disabled" would stop being assertable.
//
// # Why it does not weaken the inertness proof
//
// This is a separate function rather than a line inside New, and that
// separation is load-bearing rather than tidy. New stays free of process-wide
// side effects, so
// TestNew_EndpointPresenceDecidesWhetherAnySDKMachineryExists can go on
// constructing 64 providers and reading concrete types and goroutine counts
// without any of them mutating state shared with the rest of the binary.
// Nothing in this repository asserts on otel.GetErrorHandler, so installing
// one weakens no existing test; the one test that must observe the global does
// it in a subprocess (see errorhandler_test.go), which is what keeps the
// mutation out of this package's own test binary.
//
// # What this does NOT cover
//
// The SDK has a SECOND non-JSON stderr sink: go.opentelemetry.io/otel's
// internal logr logger, replaceable via otel.SetLogger, used by global.Error /
// Warn / Info. At its default verbosity only Error-level calls print, and
// every Error-level call site linked into this binary is in the OTLP
// exporters' parsing of OTEL_EXPORTER_OTLP_* environment variables -- which
// loam's deployment does not set. It is therefore unreachable here today
// rather than merely unlikely, and closing it would promote github.com/go-logr/logr
// to a direct dependency for a path nothing takes. Reported rather than fixed.
//
// It is safe to call more than once (cmd/server's acceptance harness runs the
// whole startup sequence repeatedly in one process); otel.SetErrorHandler is
// an atomic store.
func InstallErrorHandler(logger *slog.Logger) {
	otel.SetErrorHandler(slogErrorHandler{logger: logger})
}
