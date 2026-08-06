package deploycheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	helmTemplatesDir     = "../../helm/loam/templates"
	helmValuesSchemaPath = "../../helm/loam/values.schema.json"
	serverPkgDir         = "../../cmd/server"
)

// helmCommentBlock and helmYAMLComment strip the two kinds of comment a
// Helm template can carry before helmChartEnvNames scans it for variable
// names. Without this, doc.go's objection to grepping applies in full: this
// chart's templates talk about LOAM_* variables in prose constantly -- the
// deployment's env block explains where LOAM_DB_PASSWORD comes from, the
// configmap's header explains why LOAM_ADMIN_PASSWORD is absent -- and a
// naive scan would "find" every one of them, including the ones the chart
// deliberately does NOT set. The compose tests get this for free by parsing
// YAML; a Go-template file is not valid YAML until it is rendered, and
// rendering needs a helm binary this package must not require, so the
// comments come out by hand instead.
var (
	// The `\s*` either side of the comment body is not cosmetic. Helm's
	// template comments are conventionally written `{{- /* ... */ -}}`, with
	// a space between the trim marker and the delimiter, and a pattern that
	// demanded `{{-/*` matched none of this chart's comments -- which was
	// caught by mutation, not by reading: deleting LOAM_OTEL_ENDPOINT's
	// emission from templates/configmap.yaml left
	// TestHelmChartCanCarryEveryConfigVariable green, because the paragraph
	// of prose above the deleted lines still contained the name.
	helmCommentBlock = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	helmYAMLComment  = regexp.MustCompile(`(?m)^[ \t]*#.*$`)
	loamEnvName      = regexp.MustCompile(`LOAM_[A-Z0-9_]+`)
)

// helmChartEnvNames returns every LOAM_* environment-variable name the chart
// templates actually SET -- in the loam-config ConfigMap's data keys or the
// Deployment's env entries -- with comment text removed first.
//
// NOTES.txt is excluded: it is release output for a human, not a manifest,
// so a variable named there is discussed rather than set. Including it would
// make TestHelmChartSetsOnlyRealConfigVariables pass for a variable the
// chart merely mentions.
func helmChartEnvNames(t *testing.T) map[string]struct{} {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(helmTemplatesDir, "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "found no templates under %s", helmTemplatesDir)
	seen := map[string]struct{}{}
	for _, path := range paths {
		for name := range helmTemplateEnvNames(t, path) {
			seen[name] = struct{}{}
		}
	}
	return seen
}

func helmTemplateEnvNames(t *testing.T, path string) map[string]struct{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)
	stripped := helmYAMLComment.ReplaceAllString(helmCommentBlock.ReplaceAllString(string(raw), ""), "")
	seen := map[string]struct{}{}
	for _, name := range loamEnvName.FindAllString(stripped, -1) {
		seen[name] = struct{}{}
	}
	return seen
}

// TestHelmChartCanCarryEveryConfigVariable is the guard loam-uwus was filed
// for, stated as a property rather than as a list.
//
// The bead's blocker was not that helm/loam had the WRONG value for
// LOAM_OTEL_ENDPOINT. It was that the chart had no way to express it at
// all: the ConfigMap emits a fixed key list, the Deployment's env is fixed,
// and its only envFrom is that ConfigMap, so a variable internal/config
// reads and the chart does not name simply cannot reach a pod. Nothing
// failed. The chart rendered, ArgoCD synced green, and the setting
// evaporated.
//
// So the property is: for every LOAM_* variable internal/config reads, the
// chart must name it somewhere. Both sides are DISCOVERED -- the left from
// internal/config's AST (configEnvNames, shared with the compose tests), the
// right from the templates -- so a variable added to internal/config is a
// red test here on the day it lands, not a deployment nobody can configure.
//
// This is the direction TestComposeEnvironmentSatisfiesConfigLoad cannot
// cover even for compose, because that test runs config.Load and config.Load
// only complains about REQUIRED variables. Every optional variable -- which
// is all three OTel ones -- is invisible to it. Here, required and optional
// are treated identically, because from the chart's point of view they are:
// one that cannot be set is equally unsettable either way.
func TestHelmChartCanCarryEveryConfigVariable(t *testing.T) {
	t.Parallel()
	known := configEnvNames(t)
	require.NotEmpty(t, known, "found no LOAM_* names in internal/config; the discovery is broken, not the chart")
	chart := helmChartEnvNames(t)
	for _, name := range known {
		assert.Contains(t, chart, name,
			"internal/config reads %s but no helm/loam template sets it: an operator has no way to configure it, and the attempt fails SILENTLY -- helm renders, ArgoCD syncs green, the value never reaches the pod (loam-uwus)", name)
	}
}

// TestHelmChartSetsOnlyRealConfigVariables is the other direction, and it is
// the one that answers "does deploycheck notice OPTIONAL variables?" for the
// chart. It does now; it did not before.
//
// A rename in internal/config -- LOAM_OTEL_ENDPOINT becoming, say,
// LOAM_OTLP_ENDPOINT -- leaves a chart that keeps setting the old name. For
// a REQUIRED variable that is loud: the server refuses to boot. For an
// OPTIONAL one it is completely silent: the server starts, applies the
// default, and the operator's setting is ignored with no message anywhere,
// which for LOAM_OTEL_ENDPOINT means telemetry that is simply off. This is
// the exact hazard TestComposeSetsOnlyRealConfigVariables already covers for
// deploy/docker-compose.yml, applied to the artifact that had no coverage of
// its own at all -- before loam-uwus, the only thing any test read out of
// helm/loam/values.yaml was postgres.image.
func TestHelmChartSetsOnlyRealConfigVariables(t *testing.T) {
	t.Parallel()
	known := map[string]struct{}{}
	for _, name := range configEnvNames(t) {
		known[name] = struct{}{}
	}
	chart := helmChartEnvNames(t)
	require.NotEmpty(t, chart, "found no LOAM_* names in the helm templates; the discovery is broken, not internal/config")
	for name := range chart {
		assert.Contains(t, known, name,
			"helm/loam sets %s, which internal/config never reads: either a typo or internal/config renamed it, and for an OPTIONAL variable both are silent -- the server starts and applies the default instead", name)
	}
}

// TestComposeOffersTheSameKnobsAsTheChart closes the last corner of the
// optional-variable gap, the one on the compose side, and it took a mutation
// to find: deleting LOAM_OTEL_SAMPLE_RATIO from deploy/docker-compose.yml
// left every other test in this package green.
//
// The two existing compose tests cover the other two corners and cannot
// cover this one. TestComposeSetsOnlyRealConfigVariables checks the names
// the file DOES set, so a deleted line is nothing to it.
// TestComposeEnvironmentSatisfiesConfigLoad runs config.Load, which
// complains only about REQUIRED variables -- and every OTel variable is
// optional, on purpose (internal/config's loadTelemetry explains why that
// optionality is a structural requirement of this very package). So a
// deleted optional variable was invisible from both directions: the compose
// stack would quietly stop passing an operator's setting through and apply
// internal/config's default instead, with nothing anywhere saying so.
//
// The property that catches it without a hand-maintained list is a real one
// rather than a convenient one: THE TWO DEPLOYMENT STACKS SHOULD OFFER THE
// SAME CONFIGURATION SURFACE. A knob you can turn on Kubernetes but not on
// the single-machine stack is an asymmetry an operator discovers the hard
// way, and there is no reason for one to exist.
//
// The comparison is deliberately against the chart's ConfigMap alone, not
// the whole chart. templates/deployment.yaml also names LOAM_DATABASE_URL,
// which deploy/docker-compose.yml pointedly does NOT set -- it ships a
// bundled Postgres and uses internal/config's discrete LOAM_DB_* form, and
// setting both at once is a startup error by design. The ConfigMap is
// exactly the non-secret, unconditional surface where the two stacks really
// should agree.
func TestComposeOffersTheSameKnobsAsTheChart(t *testing.T) {
	t.Parallel()
	chart := helmTemplateEnvNames(t, filepath.Join(helmTemplatesDir, "configmap.yaml"))
	require.NotEmpty(t, chart, "found no LOAM_* names in the chart's ConfigMap; the discovery is broken")
	deploy := loadCompose(t, deployComposePath)
	loam, ok := deploy.Services["loam"]
	require.True(t, ok)
	for name := range chart {
		assert.Contains(t, loam.Environment, name,
			"helm/loam's ConfigMap offers %s but deploy/docker-compose.yml does not, so the same setting is reachable on Kubernetes and not on the single-machine stack. If the divergence is intentional, it needs to be stated somewhere louder than a missing line.", name)
	}
}

// TestHelmValuesSchemaExists guards the property that makes every other
// mistake in this chart loud instead of quiet, and it is deliberately a
// separate test from the coverage ones above because it fails for a
// different reason: deleting values.schema.json, or removing the
// `additionalProperties: false` that gives it teeth, restores the original
// loam-uwus trap without touching a single variable name.
//
// Helm's default with no schema is to accept and DISCARD unrecognised keys.
// Rendering this chart with `--set config.otelEndpoint=... --set
// terminationGracePeriodSeconds=45` before either key existed produced a
// ZERO-BYTE diff against the default render -- which is what a plausible
// wiring in an ArgoCD Application would have done.
//
// What this test can check without a helm binary is that the file is
// present, that every object level in it closes itself to unknown keys, and
// that it declares the keys the templates actually read. The full
// behavioural proof -- unknown key REJECTED, OTel values reaching the pod
// spec, default render unchanged -- needs `helm template` and is recorded on
// the bead rather than pretended at here.
func TestHelmValuesSchemaExists(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(helmValuesSchemaPath)
	require.NoError(t, err, "helm/loam/values.schema.json is missing: without it Helm silently IGNORES unknown values keys, which is the failure loam-uwus was filed for")
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema), "values.schema.json is not valid JSON, so Helm will refuse to render the chart at all")
	// Walk the whole schema rather than counting substrings: a new object
	// declared without additionalProperties reopens its entire subtree to
	// typos, and a count-based check would need a magic slack number that
	// goes stale the first time a legitimately free-form map is added.
	// Declaring `true` is a perfectly acceptable answer here -- the test
	// asks only that openness be a DECISION recorded in the file, not an
	// omission.
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		properties, hasProperties := node["properties"].(map[string]any)
		if node["type"] == "object" || hasProperties {
			_, declared := node["additionalProperties"]
			assert.True(t, declared,
				"values.schema.json's %s is an object that does not declare additionalProperties, so every unknown key beneath it is accepted and silently discarded -- the exact loam-uwus failure. Set it to false, or to a schema, or explicitly to true if openness is intended.", path)
		}
		for name, child := range properties {
			if sub, ok := child.(map[string]any); ok {
				walk(path+"."+name, sub)
			}
		}
		for _, key := range []string{"definitions", "$defs"} {
			defs, ok := node[key].(map[string]any)
			if !ok {
				continue
			}
			for name, child := range defs {
				if sub, ok := child.(map[string]any); ok {
					walk(key+"."+name, sub)
				}
			}
		}
	}
	walk("(root)", schema)
	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok, "values.schema.json declares no properties at all")
	config, ok := properties["config"].(map[string]any)
	require.True(t, ok, "values.schema.json declares no config object")
	configProperties, ok := config["properties"].(map[string]any)
	require.True(t, ok)
	for _, key := range []string{"otelEndpoint", "otelServiceName", "otelSampleRatio"} {
		assert.Contains(t, configProperties, key,
			"values.schema.json does not declare config.%s, so setting it is now a RENDER FAILURE even though templates/configmap.yaml reads it -- with a schema in place, an undeclared key is no longer merely ignored", key)
	}
	assert.Contains(t, properties, "terminationGracePeriodSeconds",
		"values.schema.json does not declare terminationGracePeriodSeconds, which templates/deployment.yaml reads")
}
