// Package config loads and validates the Loam server's environment-variable
// configuration surface described in docs/server-spec.md. Load never panics
// and never calls os.Exit; that decision belongs to cmd/server/main.go, which
// keeps the loader itself testable.
package config

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// maxIngestWorkers bounds LOAM_INGEST_WORKERS from above. Unlike the sync
// scheduler's per-cycle git subprocess pair (cmd/server's
// defaultMaxConcurrentCycles doc comment), an ingest worker is just a
// goroutine plus a pgx connection borrowed from the shared pool for the
// duration of a claim/run cycle -- there is no per-worker OS resource
// (file descriptor, subprocess) that makes a large count immediately
// dangerous. The ceiling here exists purely to catch an operator's typo
// (an extra zero, a pasted port number) rather than to protect a hard OS
// limit: pgx pool's own default MaxConns is max(4, NumCPU), so any
// setting above a few dozen already outstrips available DB connections
// and just queues on Acquire, and 256 leaves generous headroom above that
// for unusually large machines while still refusing a five- or six-digit
// value outright.
const maxIngestWorkers = 256

// defaultOTelServiceName is the service.name every span and metric this
// process emits carries unless LOAM_OTEL_SERVICE_NAME overrides it.
const defaultOTelServiceName = "loam"

// defaultOTelSampleRatio is the head-sampling probability applied when
// LOAM_OTEL_SAMPLE_RATIO is unset. It is deliberately conservative: the
// first deployment to set LOAM_OTEL_ENDPOINT should not simultaneously
// discover its collector's ingest volume. The sampler is ParentBased
// (internal/telemetry), so this ratio is a decision about ROOT spans only --
// once a trace is sampled, all of its spans in this process are kept, and a
// sampled trace is therefore never a partial one.
const defaultOTelSampleRatio = 0.1

// Config holds the fully validated server configuration decoded from the
// LOAM_* environment variables in docs/server-spec.md.
type Config struct {
	HTTPAddr      string
	AdminUser     string
	AdminPassword string
	DatabaseURL   string
	DataDir       string
	EncryptionKey []byte
	SyncInterval  time.Duration
	PRAttribution bool
	EmbedderURL   string
	EmbedderModel string
	IngestWorkers int
	LogLevel      slog.Level
	Logger        *slog.Logger
	// OTelEndpoint is the collector's OTLP/HTTP base URL. EMPTY MEANS
	// TELEMETRY IS DISABLED ENTIRELY -- see loadOptional for why this is
	// the only switch.
	OTelEndpoint    string
	OTelServiceName string
	OTelSampleRatio float64
	// TracerProvider is the resolved handle instrumentation creates tracers
	// from. Like Logger above it is NOT an environment variable: Load leaves
	// it nil, and cmd/server sets it from telemetry.Provider once
	// telemetry.New has run, which cannot happen inside Load because
	// constructing the provider needs a context and can fail.
	//
	// A nil value means "not instrumented", which is what the
	// composition-root tests that build a bare Config literal get, and is a
	// no-op rather than a panic at every consumer (internal/db's
	// Config.TracerProvider, forge.InstrumentHTTPClient,
	// ollama.InstrumentHTTPClient).
	//
	// # THE RULE FOR ADDING A THIRD NON-ENV FIELD
	//
	// Logger and TracerProvider are both OBSERVABILITY SINKS: the process
	// writes to them and never reads a value back out to make a decision
	// with. That is what makes carrying them here a pattern rather than a
	// service locator, and it is the test a third field has to pass. Two
	// properties follow from it and are what keep this honest:
	//
	//   - Removing the field can change what an operator SEES, never what
	//     the server DOES. Nothing branches on it. If a proposed field would
	//     be read in an `if`, it is an input, and it belongs in a parameter
	//     on the constructor that needs it -- not here.
	//   - Its zero value is inert, so a Config literal in a test is
	//     automatically correct without knowing the field exists.
	//
	// A MeterProvider would qualify. A database pool, an *http.Client, a
	// feature flag or a clock would NOT -- each is an input to behaviour,
	// and putting one here would turn Config into the bag of dependencies
	// this comment exists to prevent.
	TracerProvider trace.TracerProvider
}

// Load reads and validates every LOAM_* environment variable, applying the
// documented defaults where a variable is optional. It fails fast, returning
// on the first problem it finds: a missing required variable, an invalid
// value, or an unwritable data directory. The only filesystem write Load
// performs is creating LOAM_DATA_DIR (mode 0o700) if it does not already
// exist, as an unavoidable part of probing it for writability; that probe
// runs last, after every other variable has validated, so a config that is
// ultimately rejected never touches the filesystem.
func Load() (Config, error) {
	var cfg Config
	if err := loadRequired(&cfg); err != nil {
		return Config{}, err
	}
	if err := loadOptional(&cfg); err != nil {
		return Config{}, err
	}
	cfg.Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	return cfg, nil
}

// loadRequired populates and validates the environment variables that have
// no default: LOAM_ADMIN_PASSWORD, LOAM_ENCRYPTION_KEY, and the Postgres DSN
// (LOAM_DATABASE_URL directly, or assembled from the discrete LOAM_DB_*
// parts -- see resolveDatabaseURL's doc comment for the precedence rule).
func loadRequired(cfg *Config) error {
	adminPassword, err := lookupRequired("LOAM_ADMIN_PASSWORD")
	if err != nil {
		return err
	}
	cfg.AdminPassword = adminPassword
	databaseURL, err := resolveDatabaseURL()
	if err != nil {
		return err
	}
	cfg.DatabaseURL = databaseURL
	encryptionKeyRaw, err := lookupRequired("LOAM_ENCRYPTION_KEY")
	if err != nil {
		return err
	}
	encryptionKey, err := parseEncryptionKey(encryptionKeyRaw)
	if err != nil {
		return err
	}
	cfg.EncryptionKey = encryptionKey
	return nil
}

// loadOptional populates and validates the environment variables that carry
// a default value from the server-spec table.
func loadOptional(cfg *Config) error {
	cfg.HTTPAddr = lookupDefault("LOAM_HTTP_ADDR", ":8080")
	cfg.AdminUser = lookupDefault("LOAM_ADMIN_USER", "admin")
	cfg.DataDir = lookupDefault("LOAM_DATA_DIR", "/var/lib/loam")
	syncInterval, err := parseDurationEnv("LOAM_SYNC_INTERVAL", 60*time.Second)
	if err != nil {
		return err
	}
	if syncInterval <= 0 {
		return fmt.Errorf("LOAM_SYNC_INTERVAL: %w: got %s, want greater than zero", errSyncIntervalRange, syncInterval)
	}
	cfg.SyncInterval = syncInterval
	prAttribution, err := parseBoolEnv("LOAM_PR_ATTRIBUTION", true)
	if err != nil {
		return err
	}
	cfg.PRAttribution = prAttribution
	cfg.EmbedderURL = lookupDefault("LOAM_EMBEDDER_URL", "http://localhost:11434")
	cfg.EmbedderModel = lookupDefault("LOAM_EMBEDDER_MODEL", "nomic-embed-text")
	ingestWorkers, err := parseIntEnv("LOAM_INGEST_WORKERS", 2)
	if err != nil {
		return err
	}
	if ingestWorkers < 1 || ingestWorkers > maxIngestWorkers {
		return fmt.Errorf("LOAM_INGEST_WORKERS: %w: got %d, want between 1 and %d", errIngestWorkersRange, ingestWorkers, maxIngestWorkers)
	}
	cfg.IngestWorkers = ingestWorkers
	logLevel, err := parseLogLevel(lookupDefault("LOAM_LOG_LEVEL", "info"))
	if err != nil {
		return err
	}
	cfg.LogLevel = logLevel
	if err := loadTelemetry(cfg); err != nil {
		return err
	}
	if err := checkDataDirWritable(cfg.DataDir); err != nil {
		return err
	}
	return nil
}

// loadTelemetry reads the three OTel variables (loam-p56y). All three are
// OPTIONAL, via lookupDefault, and that is a structural requirement rather
// than a preference: internal/deploycheck's
// TestComposeEnvironmentSatisfiesConfigLoad RUNS this loader against
// deploy/docker-compose.yml's environment instead of modelling it, so a new
// lookupRequired here breaks compose the moment it lands, while a
// lookupDefault does not. Wiring telemetry into the deployment artifacts is
// a separate bead, and this one must not force it early.
//
// LOAM_OTEL_ENDPOINT's presence is the ONLY enable switch; there is
// deliberately no LOAM_OTEL_ENABLED. The case for a second variable is that
// it lets an operator keep an endpoint configured while turning collection
// off. The case against, which wins here:
//
//   - Two switches make four states, three of which mean "off", and an
//     operator debugging silent telemetry then has to work out WHICH off
//     they are in. One switch has no such ambiguity.
//   - One of those four states, ENABLED=true with no endpoint, cannot be
//     honoured at all. Either it is an error -- which makes
//     LOAM_OTEL_ENABLED a de-facto required variable the moment anyone sets
//     it, exactly the coupling deploycheck punishes -- or it is silently
//     ignored, which is the failure mode the second variable was supposed
//     to prevent.
//   - The stated benefit is already available without new surface.
//     Unsetting LOAM_OTEL_ENDPOINT is a one-line change in precisely the
//     same file (a Helm values.yaml or a compose env block) that setting
//     LOAM_OTEL_ENABLED=false would be.
//
// One correction to that last point, because an earlier version of this
// comment overstated it and the overstatement mattered: LOAM_OTEL_SAMPLE_RATIO=0
// is NOT a "keep the endpoint, stop collecting" switch. Sampling is a TRACE
// concept -- sdkmetric has no sampler -- so ratio 0 silences traces while the
// metric exporter keeps pushing on its periodic reader, unchanged. That
// asymmetry is asserted, not assumed, by the last case in
// internal/telemetry's TestNew_SampleRatioActuallyReachesTheSampler, and it
// is documented in docs/server-spec.md's table so an operator does not
// discover it from their collector's bill.
//
// The decision to decline the second variable stands on the first two
// bullets, which do not depend on it: the four-state ambiguity, and the
// state that cannot be honoured at all. An operator who genuinely wants
// telemetry off unsets the endpoint.
func loadTelemetry(cfg *Config) error {
	cfg.OTelEndpoint = lookupDefault("LOAM_OTEL_ENDPOINT", "")
	if cfg.OTelEndpoint != "" {
		if err := validateOTelEndpoint(cfg.OTelEndpoint); err != nil {
			return err
		}
	}
	cfg.OTelServiceName = lookupDefault("LOAM_OTEL_SERVICE_NAME", defaultOTelServiceName)
	sampleRatio, err := parseFloatEnv("LOAM_OTEL_SAMPLE_RATIO", defaultOTelSampleRatio)
	if err != nil {
		return err
	}
	// math.IsNaN first, and not as an afterthought: strconv.ParseFloat
	// accepts "NaN", and every ordered comparison against NaN is false, so
	// the range check below would wave it through to
	// sdktrace.TraceIDRatioBased -- which would then sample nothing while
	// reporting a perfectly valid configuration.
	if math.IsNaN(sampleRatio) || sampleRatio < 0 || sampleRatio > 1 {
		return fmt.Errorf("LOAM_OTEL_SAMPLE_RATIO: %w: got %v, want between 0 and 1", errOTelSampleRatioRange, sampleRatio)
	}
	cfg.OTelSampleRatio = sampleRatio
	return nil
}
