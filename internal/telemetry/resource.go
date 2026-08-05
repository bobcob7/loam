package telemetry

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// unknownVersion is the service.version value used when this binary carries
// no build stamp at all. It is set rather than omitted so a backend query
// can distinguish "built without VCS information" from "this build predates
// the version attribute".
const unknownVersion = "unknown"

// vcsRevisionLength is how much of a commit SHA goes into service.version
// when no semantic version is available. Twelve hex characters is git's own
// `git log --abbrev-commit` scale for a repository this size and is
// unambiguous in practice, while a full 40-character SHA makes every trace
// view's service column unreadable.
const vcsRevisionLength = 12

// newResource assembles the resource every span and metric this process
// emits is stamped with.
//
// It starts from resource.Default(), which contributes the telemetry SDK's
// own name/version/language AND honours the standard OTEL_RESOURCE_ATTRIBUTES
// / OTEL_SERVICE_NAME environment variables. That inherited escape hatch is
// deliberate: it is how the later deployment-wiring bead can add
// deployment.environment or k8s.* attributes from a Helm values file without
// this repository growing a LOAM_* variable per attribute.
//
// loam's own attributes are merged SECOND, so LOAM_OTEL_SERVICE_NAME wins
// over OTEL_SERVICE_NAME: the LOAM_ variable is the documented knob, and an
// operator who sets it and then sees a different service.name in their
// backend has a genuinely mysterious problem.
//
// The semconv import is pinned to v1.41.0 because that is the version
// go.opentelemetry.io/otel/sdk@v1.44.0's own resource package uses. A
// mismatch is not a compile error and not a runtime panic: resource.Merge
// returns ErrSchemaURLConflict together with a SCHEMALESS merged resource,
// so the process would keep running and quietly ship attributes with no
// schema URL. TestNewResource_UsesTheSDKsSchemaURL exists to fail the day
// an SDK bump moves that version.
func newResource(ctx context.Context, serviceName, serviceVersion string) (*resource.Resource, error) {
	if serviceVersion == "" {
		serviceVersion = unknownVersion
	}
	own := resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(serviceVersion),
	)
	if instanceID, err := os.Hostname(); err == nil && instanceID != "" {
		// service.instance.id is what tells two REPLICAS apart; in
		// Kubernetes the hostname is the pod name, and under compose it is
		// the container ID. A hostname lookup failing is not worth failing
		// startup over -- the resource is simply one attribute poorer.
		merged, err := resource.Merge(own, resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceInstanceID(instanceID),
			semconv.HostName(instanceID),
		))
		if err != nil {
			return nil, fmt.Errorf("merging instance attributes: %w", err)
		}
		own = merged
	}
	res, err := resource.Merge(resource.Default(), own)
	if err != nil {
		return nil, fmt.Errorf("merging telemetry resource: %w", err)
	}
	return res, ctx.Err()
}

// BuildVersion resolves this binary's own version from the build stamp the
// Go toolchain already embeds, rather than introducing a second source of
// truth (an -ldflags -X variable, or a hand-edited constant) that a tagged
// release would have to remember to update.
//
// The repository tags releases (v0.0.5 is current at the time of writing),
// and `go build ./cmd/server` from a git checkout turns that tag into
// debug.BuildInfo.Main.Version -- e.g. v0.0.6-0.20260805165912-72244cfcdf88
// for a commit past v0.0.5. Two cases do not get that far and are handled by
// resolveVersion: `go run` and `go test` produce Main.Version == "(devel)",
// and a build from a source tree with no .git at all produces "(devel)" plus
// no vcs settings. The second case is not hypothetical -- the repo-root
// .dockerignore excludes .git/, so the SHIPPED CONTAINER IMAGE takes exactly
// that path and reports "unknown". Recording it here rather than hiding it:
// fixing that belongs to the deployment-wiring bead (it is a Dockerfile/CI
// change, not a Go one), and reporting "unknown" honestly is strictly better
// than a constant that lies.
func BuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	return resolveVersion(info, ok)
}

// resolveVersion is BuildVersion's pure core, split out so its three cases
// can be tested without needing three differently-built binaries.
func resolveVersion(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return unknownVersion
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return unknownVersion
	}
	if len(revision) > vcsRevisionLength {
		revision = revision[:vcsRevisionLength]
	}
	if modified {
		return revision + "+dirty"
	}
	return revision
}
