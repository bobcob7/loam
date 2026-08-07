package deploycheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
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
	// Everything from a `#` to end of line, NOT just whole-line comments.
	// The whole-line form was the first version and it had the same hole M2
	// exposed in the block-comment stripper, reached through YAML's comment
	// syntax instead of Helm's: a TRAILING comment (`failureThreshold: 60 #
	// ~5 minutes` is the shape this chart already uses) survives it, so a
	// deleted emission could be masked by prose on the line above.
	//
	// Stripping from any `#` will also cut a `#` that appears inside a
	// quoted value. That is deliberate, and it is the safe direction: the
	// only effect is that a name is not FOUND, which makes
	// TestHelmChartCanCarryEveryConfigVariable fail loudly. Being too
	// permissive here is what fails silently.
	helmYAMLComment = regexp.MustCompile(`(?m)#.*$`)
	loamEnvName     = regexp.MustCompile(`LOAM_[A-Z0-9_]+`)
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
	// Declaring an open answer is still perfectly acceptable here -- the
	// test asks that openness be a DECISION recorded in the file, not an
	// omission -- but since loam-wu10 it must be recorded in PROSE as well
	// as in a boolean, and that second half is not decoration. An open node
	// is an escape hatch out of
	// TestHelmSchemaDeclaresEveryValuesKeyTheTemplatesRead below, which
	// treats reaching one as the key being accounted for: that is right for
	// a real Kubernetes-API passthrough and a hole for anything else. A
	// review of this branch found the hatch reachable in TWO LINES for any
	// of the chart's closed objects -- setting persistence.additional
	// Properties to true and deleting persistence.properties.size left the
	// whole package green, silently un-guarding that subtree.
	//
	// Requiring a description does not make that impossible; nothing in a
	// unit test can stop a determined edit, and this guards rot rather than
	// adversaries. What it does is make the edit cost a SENTENCE, and a
	// sentence is the thing a reviewer actually reads. That limit is
	// measured, not assumed: the same two-line reopen PLUS a one-line
	// description still passes this package. The guard buys review
	// attention, not enforcement, and it should not be read as more. It costs nothing
	// today: the chart's genuine passthroughs are the five
	// docs/deployment-spec.md already enumerates and justifies one by one,
	// so each of them has an argument to state. An object with no argument
	// available -- image, namespace, secret, postgres, persistence,
	// ingress, config, all of which mirror a finite vocabulary this chart
	// defines itself -- has nothing to write in it, which is the point.
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		properties, hasProperties := node["properties"].(map[string]any)
		if node["type"] == "object" || hasProperties {
			additional, declared := node["additionalProperties"]
			assert.True(t, declared,
				"values.schema.json's %s is an object that does not declare additionalProperties, so every unknown key beneath it is accepted and silently discarded -- the exact loam-uwus failure. Set it to false, or to a schema, or explicitly to true if openness is intended.", path)
			if declared && additional != false {
				description, _ := node["description"].(string)
				assert.NotEmpty(t, description,
					"values.schema.json's %s is OPEN to unknown keys and does not say why. Openness here is not just laxer validation: it is an escape hatch out of TestHelmSchemaDeclaresEveryValuesKeyTheTemplatesRead, which stops checking at an open node, so every values key beneath %s becomes unguarded. That is correct for a verbatim passthrough of somebody else's API and wrong for a vocabulary this chart defines itself. Write the argument in a description, or close it.", path, path)
			}
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
	// WHICH keys the schema has to declare is NOT listed here. It used to
	// be -- an explicit []string{"otelEndpoint", "otelServiceName",
	// "otelSampleRatio"} plus terminationGracePeriodSeconds -- and loam-wu10
	// is what a list like that costs: internal/config grew a fourth OTel
	// variable, the list did not, and nothing noticed the schema could not
	// carry it. TestHelmSchemaDeclaresEveryValuesKeyTheTemplatesRead now
	// derives that set from the templates instead, which covers those four
	// keys and every other one, so the list is deleted rather than extended.
	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok, "values.schema.json declares no properties at all")
	// The one object asserted CLOSED rather than merely declared, and the
	// reason it is singled out: config mirrors internal/config's env-var
	// vocabulary, which is finite and knowable from the Go source. The
	// passthrough argument that makes ingress.annotations, nodeSelector,
	// affinity and the resource shapes legitimately open -- "this is a slice
	// of somebody else's API and a partial restatement would reject valid
	// input" -- is simply not available here. Opening it would also, quietly,
	// defeat TestHelmSchemaDeclaresEveryValuesKeyTheTemplatesRead below,
	// which treats reaching an open node as the key being accounted for; that
	// is correct behaviour for a real passthrough and a hole for this one.
	config, ok := properties["config"].(map[string]any)
	require.True(t, ok, "values.schema.json declares no config object")
	assert.Equal(t, false, config["additionalProperties"],
		"values.schema.json's config object must be CLOSED, not open. Its key set mirrors internal/config's LOAM_* vocabulary, so unlike the chart's five deliberate passthroughs there is nothing free-form to protect -- and opening it turns a mistyped config key back into a value Helm accepts and discards")
}

// valuesRef is one `.Values.<dotted.path>` reference found in a
// template, remembered together with the file it was read from so a failure
// names somewhere to go rather than just a path.
type valuesRef struct {
	path string
	file string
}

var helmValuesRef = regexp.MustCompile(`\.Values\.([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*)`)

// helmValuesPathsRead returns every values path the chart's templates read,
// comments stripped first for the reason helmChartEnvNames strips them: this
// chart's prose mentions values keys constantly, and a scan that counted
// those would "find" a key nobody actually reads.
//
// *.tpl is in the glob as well as *.yaml -- _helpers.tpl reads
// .Values.replicaCount, and a discovery that missed the helpers would be
// quietly partial, which is the failure mode this whole package exists to
// avoid.
func helmValuesPathsRead(t *testing.T) []valuesRef {
	t.Helper()
	var paths []string
	for _, pattern := range []string{"*.yaml", "*.tpl"} {
		matched, err := filepath.Glob(filepath.Join(helmTemplatesDir, pattern))
		require.NoError(t, err)
		paths = append(paths, matched...)
	}
	require.NotEmpty(t, paths, "found no templates under %s", helmTemplatesDir)
	seen := map[string]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		require.NoError(t, err, "reading %s", path)
		stripped := helmYAMLComment.ReplaceAllString(helmCommentBlock.ReplaceAllString(string(raw), ""), "")
		for _, match := range helmValuesRef.FindAllStringSubmatch(stripped, -1) {
			if _, ok := seen[match[1]]; !ok {
				seen[match[1]] = filepath.Base(path)
			}
		}
	}
	out := make([]valuesRef, 0, len(seen))
	for _, path := range sortedKeys(seen) {
		out = append(out, valuesRef{path: path, file: seen[path]})
	}
	return out
}

// schemaDeclares reports whether values.schema.json accounts for a dotted
// values path, and if not, where the walk gave up.
//
// "Accounts for" deliberately includes reaching a node that declares itself
// OPEN. Five objects in this schema are open on purpose -- ingress.
// annotations, nodeSelector, affinity and both resource shapes -- because
// each is a verbatim passthrough of a slice of the Kubernetes API. Helm
// accepts anything beneath them, so this test must too, or it would demand
// the partial restatement values.schema.json's own description explains why
// the chart does not want.
func schemaDeclares(t *testing.T, schema map[string]any, path string) (bool, string) {
	t.Helper()
	node := schema
	for i, part := range strings.Split(path, ".") {
		node = resolveSchemaRef(t, schema, node)
		properties, _ := node["properties"].(map[string]any)
		child, ok := properties[part].(map[string]any)
		if !ok {
			if additional, declared := node["additionalProperties"]; declared && additional != false {
				return true, ""
			}
			return false, strings.Join(strings.Split(path, ".")[:i+1], ".")
		}
		node = child
	}
	return true, ""
}

// resolveSchemaRef follows a local $ref one hop, which is all this schema
// needs: postgres.resources and resources both point at
// #/definitions/resources. Without it a future template reading into a
// $ref'd shape would fail this test for the wrong reason -- the schema does
// declare the key, the walk just could not see it.
func resolveSchemaRef(t *testing.T, root, node map[string]any) map[string]any {
	t.Helper()
	ref, ok := node["$ref"].(string)
	if !ok || !strings.HasPrefix(ref, "#/") {
		return node
	}
	target := any(root)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		asMap, ok := target.(map[string]any)
		if !ok {
			return node
		}
		target = asMap[part]
	}
	resolved, ok := target.(map[string]any)
	require.True(t, ok, "values.schema.json's %s does not resolve to an object", ref)
	return resolved
}

// TestHelmSchemaDeclaresEveryValuesKeyTheTemplatesRead is the guard loam-wu10
// needed, and the one whose absence let the reported failure through.
//
// Read that carefully, because the obvious story is wrong.
// TestHelmChartCanCarryEveryConfigVariable did NOT miss
// LOAM_OTEL_DB_ACQUIRE_THRESHOLD. It caught it, correctly, and sat red on
// main for a day. What it could not do was catch it on either branch that
// caused it: loam-9v9s added the variable to internal/config on a branch
// cut before helm_test.go existed, loam-uwus added helm_test.go on a branch
// where internal/config did not yet read that variable, and each was
// legitimately green in isolation. Nothing was weak; the two halves of the
// contradiction were never in the same tree until the merge. That is a fact
// about CI topology -- per-PR gates test branches, not merge results -- and
// no test in this package can fix it.
//
// What IS a gap, and is this test's subject, is the step AFTER the one that
// guard checks. TestHelmChartCanCarryEveryConfigVariable asks whether a
// template NAMES the variable. It stops there, and the chain does not:
//
//	internal/config reads LOAM_FOO
//	  -> templates/configmap.yaml emits it from .Values.config.foo
//	    -> values.schema.json declares config.foo   <- NOTHING CHECKED THIS
//	      -> values.yaml gives it a default
//
// Miss the third link and every test in this package stays green while the
// operator gets a hard render failure naming the key they just set:
//
//	at '/config': additional properties 'otelDbAcquireThreshold' not allowed
//
// which is verbatim the error loam-wu10 was filed with. Verified by
// mutation rather than argued: deleting ONLY the schema entry, leaving the
// template and values.yaml intact, left every other test in this package
// passing and `helm template --set config.otelDbAcquireThreshold=5ms`
// failing. (Deliberately not "all N of them". A test count written into a
// comment is the same construct as the key list this branch deleted --
// nothing regenerates it and nothing fails when it goes stale.) Since loam-uwus closed the schema, a forgotten declaration is no
// longer a silent no-op -- it is a chart the operator cannot render. Louder,
// but still discovered by the wrong person at the wrong moment.
//
// Both sides are discovered. The left is every `.Values.*` reference in the
// templates, the right is the schema's own property tree, so this covers
// the whole values surface rather than the config object: the same hole
// existed for terminationGracePeriodSeconds and would exist for the next
// top-level key somebody adds.
func TestHelmSchemaDeclaresEveryValuesKeyTheTemplatesRead(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(helmValuesSchemaPath)
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	read := helmValuesPathsRead(t)
	require.NotEmpty(t, read, "found no .Values references in the helm templates; the discovery is broken, not the schema")
	for _, ref := range read {
		declared, missing := schemaDeclares(t, schema, ref.path)
		assert.True(t, declared,
			"templates/%s reads .Values.%s but values.schema.json does not declare %s. The schema is CLOSED (loam-uwus), so an operator who sets it does not get a silently ignored key -- they get a hard render failure naming it, `at '/%s': additional properties not allowed`, which is exactly how loam-wu10 was reported. Declare it beside the key in values.yaml.",
			ref.file, ref.path, missing, strings.ReplaceAll(missing, ".", "/"))
	}
}

// TestHelmDefaultValuesDefineEveryKeyTheTemplatesRead is the fourth link in
// the chain above, and it fails for a different reason than the third, which
// is why it is a different test.
//
// A path declared in the schema but absent from values.yaml renders as an
// empty value rather than failing: `{{ .Values.config.foo | quote }}`
// becomes `""`, and under the `if ne (toString ...) ""` form the OTel keys
// use, it emits nothing at all. So the chart renders clean and the knob
// exists only for an operator who already knows to look in the schema for
// it. values.yaml is this chart's documentation -- every key in it carries
// a paragraph explaining the value -- and a key that never appears there is
// undocumented by construction.
func TestHelmDefaultValuesDefineEveryKeyTheTemplatesRead(t *testing.T) {
	t.Parallel()
	values := helmDefaultValues(t)
	for _, ref := range helmValuesPathsRead(t) {
		assert.True(t, valuesDefine(values, ref.path),
			"templates/%s reads .Values.%s but helm/loam/values.yaml never defines it, so the default render substitutes an empty value and the knob is documented nowhere an operator reads. Add it with the comment explaining what it is for.",
			ref.file, ref.path)
	}
}

// TestEveryHelmValueIsReadBySomeTemplate is the reverse, and it catches the
// mirror-image silent failure: a key that values.yaml documents, the schema
// accepts, and no template ever reads. Setting it renders clean, validates
// clean, changes nothing, and says nothing -- the same shape as loam-uwus's
// original zero-byte diff, arrived at from the other end. A rename on the
// template side leaves exactly this behind.
//
// Satisfied by any PREFIX of a values path being read, because several keys
// are consumed wholesale: templates/deployment.yaml does `toYaml
// .Values.resources`, so resources.requests.cpu is genuinely reached without
// any template naming it.
func TestEveryHelmValueIsReadBySomeTemplate(t *testing.T) {
	t.Parallel()
	read := map[string]struct{}{}
	for _, ref := range helmValuesPathsRead(t) {
		read[ref.path] = struct{}{}
	}
	leaves := valuesLeafPaths(helmDefaultValues(t), "")
	require.NotEmpty(t, leaves, "found no keys in helm/loam/values.yaml; the discovery is broken")
	for _, leaf := range leaves {
		assert.True(t, anyPrefixRead(read, leaf),
			"helm/loam/values.yaml defines %s and no template reads it or anything containing it: an operator can set it, the chart renders, the schema validates, and nothing happens. Either wire it up or delete it.", leaf)
	}
}

func helmDefaultValues(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(helmValuesPath)
	require.NoError(t, err)
	var values map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &values), "parsing %s", helmValuesPath)
	require.NotEmpty(t, values, "%s is empty", helmValuesPath)
	return values
}

func valuesDefine(values map[string]any, path string) bool {
	node := any(values)
	for _, part := range strings.Split(path, ".") {
		asMap, ok := node.(map[string]any)
		if !ok {
			return false
		}
		node, ok = asMap[part]
		if !ok {
			return false
		}
	}
	return true
}

// valuesLeafPaths flattens values.yaml to dotted paths. An EMPTY map is a
// leaf, not a branch to recurse into: nodeSelector: {} and affinity: {} are
// values in their own right (templates/deployment.yaml's `with` reads them),
// and treating them as branches would silently drop them from the reverse
// check.
func valuesLeafPaths(node any, prefix string) []string {
	asMap, ok := node.(map[string]any)
	if !ok || len(asMap) == 0 {
		if prefix == "" {
			return nil
		}
		return []string{prefix}
	}
	var out []string
	for _, key := range sortedKeys(asMap) {
		child := prefix + "." + key
		if prefix == "" {
			child = key
		}
		out = append(out, valuesLeafPaths(asMap[key], child)...)
	}
	return out
}

func anyPrefixRead(read map[string]struct{}, path string) bool {
	parts := strings.Split(path, ".")
	for i := range parts {
		if _, ok := read[strings.Join(parts[:i+1], ".")]; ok {
			return true
		}
	}
	return false
}
