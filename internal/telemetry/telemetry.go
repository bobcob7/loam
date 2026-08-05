// Package telemetry owns this repository's single OpenTelemetry SDK seam
// (loam-p56y). It constructs a TracerProvider, a MeterProvider, and one
// bounded shutdown function, and it instruments NOTHING: no span, no metric,
// and no import of this package exists anywhere outside cmd/server today.
// The instrumentation beads (RPC spans at the internal/server chokepoint,
// pgx/forge/embedder spans, ingest metrics) each take the providers this
// package hands out and attach their own instrumentation at their own
// chokepoint.
//
// # Export is OTLP push, over HTTP/protobuf
//
// loam pushes to a collector; nothing scrapes loam. That is not merely
// taste. internal/server/router.go's RegisterUnauthenticated is documented
// at cmd/server/main.go as covering /healthz and /readyz and being "the
// only such exemption", and that property is ENFORCED by
// TestRouter_RegisterUnauthenticated_RejectsNonHealthPattern. A Prometheus
// /metrics endpoint would need a third exemption and would rewrite both the
// claim and its test. Push sidesteps the question entirely.
//
// HTTP/protobuf rather than gRPC for the transport: the otlp*grpc exporters
// make google.golang.org/grpc a direct dependency of the server for no gain
// here, while the otlp*http exporters reuse net/http, which every outbound
// caller in this tree (internal/forge, internal/ingest/embed/ollama) already
// uses. LOAM_OTEL_ENDPOINT is therefore a full URL
// (http://otel-collector:4318), handed to WithEndpointURL, so the choice of
// TLS is carried by the scheme rather than by a second boolean variable.
//
// # No OpenTelemetry globals
//
// This package never calls otel.SetTracerProvider or otel.SetMeterProvider,
// even though every upstream example does. Those are package-level mutable
// globals, which CLAUDE.md forbids, and the repository already threads
// *slog.Logger through ~30 packages by constructor injection rather than
// reaching for a global. The practical argument is stronger than the
// stylistic one: with a global installed, "telemetry is disabled" becomes a
// property of a process-wide variable that no test can assert about without
// mutating it for every other test in the binary, and the inertness proof
// below (a concrete no-op provider type, asserted on) becomes impossible.
// Both connectrpc.com/otelconnect and otelhttp accept an explicit
// WithTracerProvider option, so no downstream bead is forced into the global
// either.
//
// One consequence is worth naming rather than discovering: the SDK reports
// its OWN internal errors (an unreachable collector, a rejected payload)
// through otel.Handle, whose default handler writes to stderr via the
// standard log package, not through cfg.Logger's JSON handler. Routing that
// into slog means setting otel.SetErrorHandler, which is a process-wide
// global and belongs at the composition root if it is done at all; it is
// deliberately out of scope here.
//
// # Disabled must be genuinely inert
//
// With LOAM_OTEL_ENDPOINT unset, New creates no exporter, opens no
// connection, and starts no goroutine: it returns a Provider holding the
// upstream no-op TracerProvider/MeterProvider and a Shutdown that returns
// nil without doing any work. This matters because an OTLP exporter pointed
// at nothing does not fail loudly -- it buffers and retries, quietly
// accumulating, and can hold shutdown open for its whole retry budget. See
// telemetry_test.go for what proves it rather than asserts it.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// DefaultShutdownTimeout bounds how long Shutdown will wait for the OTLP
// exporters to flush before giving up on them. It is deliberately small
// relative to cmd/server's 30s shutdown grace: an unreachable collector is
// the case that actually happens in production, and a flush that outlived
// the pod's terminationGracePeriodSeconds would turn a graceful shutdown
// into a SIGKILL -- losing the in-flight HTTP requests and background work
// this repository already drains carefully, in exchange for telemetry about
// a shutdown nobody will ever see, since the collector is unreachable.
const DefaultShutdownTimeout = 5 * time.Second

// exportRequestTimeout bounds a single OTLP HTTP request. The exporter's own
// default is 10s, longer than DefaultShutdownTimeout, which would mean a
// bounded Shutdown could never complete even one attempt against a black-
// holed collector.
const exportRequestTimeout = 2 * time.Second

// retryMaxElapsed caps the exporter's retry budget. The upstream default is
// 60 seconds. Shutdown's own bounded context already cuts a retry loop off,
// so this is defence in depth rather than the primary control -- but it also
// governs the STEADY-STATE case, where a batch processor retrying a
// briefly-unreachable collector for a full minute holds a batch (and its
// memory) far longer than the next batch interval.
const retryMaxElapsed = 10 * time.Second

var errSampleRatioRange = errors.New("sample ratio out of range")

// Config is the fully resolved telemetry configuration. Every field is
// supplied by the composition root; nothing here reads the environment
// (internal/config owns every LOAM_* variable in this repository).
type Config struct {
	// Endpoint is the collector's OTLP/HTTP base URL, e.g.
	// http://otel-collector:4318. EMPTY MEANS DISABLED -- see the package
	// doc comment. This single switch is the whole enable/disable surface;
	// there is deliberately no separate LOAM_OTEL_ENABLED (internal/config's
	// loadOptional records why).
	Endpoint string
	// ServiceName becomes the service.name resource attribute.
	ServiceName string
	// ServiceVersion becomes the service.version resource attribute. The
	// composition root passes BuildVersion(); an empty value degrades to
	// unknownVersion rather than omitting the attribute, so a span from an
	// unstamped build is still distinguishable from one that never carried
	// a version at all.
	ServiceVersion string
	// SampleRatio is the head-sampling probability in [0,1], applied
	// through a ParentBased sampler so a sampled parent's children are
	// always kept.
	SampleRatio float64
	// ShutdownTimeout bounds Shutdown; zero means DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
}

// Provider holds the OpenTelemetry providers this process hands to its
// instrumentation, plus the one shutdown hook that flushes them. It is safe
// for concurrent use, and Shutdown is idempotent.
type Provider struct {
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	shutdownFuncs  []func(context.Context) error
	timeout        time.Duration
	enabled        bool
	once           sync.Once
	shutdownErr    error
}

// New constructs the telemetry seam described in this package's doc comment.
//
// It NEVER fails because the collector is unreachable: the OTLP/HTTP
// exporters dial lazily, so a wrong or dead LOAM_OTEL_ENDPOINT degrades to
// dropped telemetry rather than a server that refuses to boot. The only
// errors it returns are configuration errors it can decide locally -- an
// out-of-range sample ratio, or a resource that cannot be assembled.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Provider, error) {
	if math.IsNaN(cfg.SampleRatio) || cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return nil, fmt.Errorf("telemetry: %w: got %v, want between 0 and 1", errSampleRatioRange, cfg.SampleRatio)
	}
	if cfg.Endpoint == "" {
		logger.Info("telemetry disabled", "reason", "no OTLP endpoint configured")
		return disabled(), nil
	}
	res, err := newResource(ctx, cfg.ServiceName, cfg.ServiceVersion)
	if err != nil {
		return nil, fmt.Errorf("building telemetry resource: %w", err)
	}
	traceExporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
		otlptracehttp.WithTimeout(exportRequestTimeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 250 * time.Millisecond,
			MaxInterval:     2 * time.Second,
			MaxElapsedTime:  retryMaxElapsed,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("building OTLP trace exporter for %s: %w", cfg.Endpoint, err)
	}
	metricExporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(cfg.Endpoint),
		otlpmetrichttp.WithTimeout(exportRequestTimeout),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 250 * time.Millisecond,
			MaxInterval:     2 * time.Second,
			MaxElapsedTime:  retryMaxElapsed,
		}),
	)
	if err != nil {
		// The trace exporter is already live at this point and owns a
		// goroutine-free but still closeable client; shutting it down here
		// keeps the "New returned an error => nothing was started" contract
		// the caller's error path relies on.
		_ = traceExporter.Shutdown(ctx)
		return nil, fmt.Errorf("building OTLP metric exporter for %s: %w", cfg.Endpoint, err)
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		sdktrace.WithBatcher(traceExporter),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
	)
	timeout := cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}
	logger.Info("telemetry enabled",
		"endpoint", cfg.Endpoint,
		"service_name", cfg.ServiceName,
		"service_version", cfg.ServiceVersion,
		"sample_ratio", cfg.SampleRatio,
		"shutdown_timeout", timeout)
	// Only the two PROVIDERS are shut down here, deliberately, and not the
	// exporters underneath them. Each provider already owns its exporter's
	// lifecycle -- sdktrace's batch span processor and sdkmetric's periodic
	// reader both call their exporter's Shutdown as the last step of their
	// own -- so listing the exporters as well makes the second call fail
	// with "HTTP exporter is shutdown" and turns every clean shutdown into a
	// logged error. That is not hypothetical: it is what the first version
	// of this list did, and TestShutdown_IsIdempotent caught it.
	return &Provider{
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		shutdownFuncs: []func(context.Context) error{
			tracerProvider.Shutdown,
			meterProvider.Shutdown,
		},
		timeout: timeout,
		enabled: true,
	}, nil
}

// disabled returns the inert Provider: upstream's no-op providers, no
// exporter, no goroutine, and a Shutdown with nothing to do. The concrete
// types are what make inertness ASSERTABLE -- an sdktrace.TracerProvider can
// never satisfy a type assertion to noop.TracerProvider, so a test cannot
// confuse "no exporter exists" with "an exporter that happens not to have
// sent anything yet".
func disabled() *Provider {
	return &Provider{
		tracerProvider: tracenoop.NewTracerProvider(),
		meterProvider:  metricnoop.NewMeterProvider(),
	}
}

// TracerProvider returns the provider instrumentation should create tracers
// from. It is never nil: when telemetry is disabled it is upstream's no-op
// implementation, so a caller never needs a nil check or an enabled check.
func (p *Provider) TracerProvider() trace.TracerProvider { return p.tracerProvider }

// MeterProvider returns the provider instrumentation should create meters
// from. It is never nil, for the same reason as TracerProvider.
func (p *Provider) MeterProvider() metric.MeterProvider { return p.meterProvider }

// Enabled reports whether an OTLP endpoint was configured. Instrumentation
// should NOT branch on this -- the no-op providers already make the disabled
// path free -- but the composition root logs it, and tests assert on it.
func (p *Provider) Enabled() bool { return p.enabled }

// Shutdown flushes and stops everything New started, bounded by whichever of
// ctx's deadline and this Provider's own ShutdownTimeout expires first.
//
// The internal bound is not redundant with the caller's. An unreachable
// collector is the production case, and it does not fail fast: the OTLP
// exporter buffers, retries with backoff, and will happily consume every
// second a caller gives it. Making the bound a property of this package
// rather than of its caller is what lets it be tested here, and it means a
// caller that passes context.Background() (cmd/server's startup-failure path
// does) still cannot hang.
//
// Shutdown is idempotent: cmd/server calls it from serve's shutdown sequence
// at the one point where the ordering is correct, and ALSO from a defer in
// run that covers every startup-failure path before serve is reached. The
// second call must not turn a clean shutdown into an error -- sdkmetric's
// MeterProvider.Shutdown returns ErrReaderShutdown when called twice -- so
// the work happens exactly once and every caller sees the same result.
func (p *Provider) Shutdown(ctx context.Context) error {
	p.once.Do(func() {
		if !p.enabled {
			return
		}
		bounded, cancel := context.WithTimeout(ctx, p.timeout)
		defer cancel()
		var errs []error
		for _, shutdown := range p.shutdownFuncs {
			// Every hook runs even after one fails: a trace exporter that
			// timed out against a dead collector must not prevent the meter
			// provider's periodic reader goroutine from being stopped.
			if err := shutdown(bounded); err != nil {
				errs = append(errs, err)
			}
		}
		p.shutdownErr = errors.Join(errs...)
	})
	if p.shutdownErr != nil {
		return fmt.Errorf("shutting down telemetry: %w", p.shutdownErr)
	}
	return nil
}
