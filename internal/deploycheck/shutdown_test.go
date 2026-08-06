package deploycheck

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// shutdownMargin is the headroom the deployment artifacts must carry ABOVE
// the sum of cmd/server's bounded shutdown steps. It is a POLICY, in the
// same sense as TestMustSetVariablesHaveNoWorkingDefault's explicit list
// (see doc.go): the sum is discovered, but how much slack is "enough" is a
// judgement nothing in the repository can derive, so it is written here once
// rather than pasted into two YAML files and a JSON schema.
//
// Why the sum alone is not the bar, and why this is not fussiness: 30s drain
// + 5s flush = 35s is the FLOOR. Setting exactly 35 reproduces precisely the
// zero margin that made the Kubernetes default (30s against a 30s drain)
// wrong in the first place -- the SIGKILL would race the process exit.
//
// Ten seconds is sized for ONE step, not three. db.Close is serve's
// top-of-function defer, so it runs strictly AFTER the telemetry flush, and
// it is the only participant in the whole sequence with NO bound of its own:
// pgxpool.Close blocks until every in-flight connection is returned, for as
// long as that takes. The other two unbudgeted steps -- SIGTERM delivery and
// Go runtime signal handling, and process teardown after db.Close returns --
// are real and belong in the accounting, but they are milliseconds. Sizing
// the margin as though it were three comparable costs would misplace where
// the risk actually is, which matters the day someone asks whether 10s is
// still enough: the question is about the pool, not about signal delivery.
const shutdownMargin = 10 * time.Second

// shutdownBudgetName matches the constants in cmd/server that make up the
// shutdown budget. This is discovery, not a list: a THIRD bounded step added
// later -- "drain", "flush", "grace", "shutdown" all being the vocabulary
// this repository already uses for them -- is picked up automatically and
// widens the required grace period, rather than sitting unnoticed while two
// YAML files keep quoting an arithmetic that no longer adds up.
var shutdownBudgetName = regexp.MustCompile(`(?i)(grace|drain|flush|shutdown)`)

// knownShutdownBudgetConsts are the two the arithmetic in
// helm/loam/values.yaml and deploy/docker-compose.yml is written against.
// They are named here for one reason only: so that a rename or a deletion in
// cmd/server fails LOUDLY instead of quietly shrinking the discovered sum
// and letting an under-sized grace period pass. The tests never assume these
// are the only two -- shutdownBudgetName above finds whatever is there.
var knownShutdownBudgetConsts = []string{"defaultShutdownGrace", "telemetryFlushGrace"}

// shutdownBudget parses cmd/server's non-test sources and returns every
// duration constant that participates in the shutdown budget.
//
// It reads the Go source rather than importing cmd/server for the obvious
// reason -- both constants are unexported in package main and nothing can
// import them -- but also for a better one: the same AST-walking approach
// configEnvNames already uses is what lets this package stay a pure unit
// test with no build of the server, no Docker, and no cluster.
//
// Anything whose NAME says it is part of the budget and whose VALUE mentions
// the time package but does not parse is a hard failure, not a silent skip.
// A skip would be the same hole doc.go describes for hand-maintained lists,
// arriving by a different route: the sum would quietly get smaller and every
// assertion built on it would quietly get weaker.
func shutdownBudget(t *testing.T) map[string]time.Duration {
	t.Helper()
	sources, err := filepath.Glob(filepath.Join(serverPkgDir, "*.go"))
	require.NoError(t, err)
	fset := token.NewFileSet()
	out := map[string]time.Duration{}
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, "parsing %s", path)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
					continue
				}
				name := value.Names[0].Name
				if !shutdownBudgetName.MatchString(name) || !mentionsTimePackage(value.Values[0]) {
					continue
				}
				d, ok := durationLiteral(value.Values[0])
				require.True(t, ok,
					"cmd/server's %s looks like a shutdown-budget constant but this test cannot read its value; teach durationLiteral the new form rather than letting the budget silently shrink", name)
				out[name] = d
			}
		}
	}
	for _, name := range knownShutdownBudgetConsts {
		require.Contains(t, out, name,
			"cmd/server no longer declares %s: the shutdown arithmetic in helm/loam/values.yaml and deploy/docker-compose.yml is written against it, so a rename here must be a rename there", name)
	}
	return out
}

func mentionsTimePackage(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "time" {
			found = true
		}
		return true
	})
	return found
}

// durationLiteral evaluates the `<int> * time.<Unit>` form both of
// cmd/server's shutdown constants use. Anything else returns false and, at
// the one call site, fails the test by name.
func durationLiteral(expr ast.Expr) (time.Duration, bool) {
	bin, ok := expr.(*ast.BinaryExpr)
	if !ok || bin.Op != token.MUL {
		return 0, false
	}
	lit, ok := bin.X.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.ParseInt(lit.Value, 0, 64)
	if err != nil {
		return 0, false
	}
	sel, ok := bin.Y.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "time" {
		return 0, false
	}
	units := map[string]time.Duration{
		"Nanosecond":  time.Nanosecond,
		"Microsecond": time.Microsecond,
		"Millisecond": time.Millisecond,
		"Second":      time.Second,
		"Minute":      time.Minute,
		"Hour":        time.Hour,
	}
	unit, ok := units[sel.Sel.Name]
	if !ok {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

func totalShutdownBudget(t *testing.T) time.Duration {
	t.Helper()
	var total time.Duration
	for _, d := range shutdownBudget(t) {
		total += d
	}
	require.Positive(t, total, "discovered a zero shutdown budget; the discovery is broken")
	return total
}

// TestChartGracePeriodExceedsGoShutdownBudget is the guard the loam-uwus
// notes asked for by name: not a comment restating the current numbers, but
// a test that reads the Go constants and catches the NEXT change to them.
//
// The defect it protects against is live and was shipped, not hypothetical.
// Nothing in helm/ set terminationGracePeriodSeconds at all, so Kubernetes'
// 30s default applied against a shutdown sequence bounded at 30s of draining
// plus a further 5s of telemetry flush on its own context. The pod was
// SIGKILLed five seconds short -- in exactly the slow-shutdown case the
// flush budget exists to serve. Because the flush runs strictly AFTER the
// drain (an ordering pinned by mutation during loam-p56y's review), the
// overrun could only ever cost the telemetry; the drain always got its full
// 30s.
//
// Note what this test does NOT assert: that the value is 45. Writing 45 here
// would make this the fourth copy of a number, and doc.go is about what
// happens to copies. It asserts the RELATIONSHIP -- grace must clear the
// discovered sum with shutdownMargin to spare -- so tuning
// defaultShutdownGrace from 30s to 60s turns this red immediately instead of
// leaving a chart that quietly truncates the drain.
func TestChartGracePeriodExceedsGoShutdownBudget(t *testing.T) {
	t.Parallel()
	budget := totalShutdownBudget(t)
	raw, err := os.ReadFile(helmValuesPath)
	require.NoError(t, err)
	var values struct {
		TerminationGracePeriodSeconds *int `yaml:"terminationGracePeriodSeconds"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &values))
	require.NotNil(t, values.TerminationGracePeriodSeconds,
		"helm/loam/values.yaml sets no terminationGracePeriodSeconds, so Kubernetes' 30s default applies -- which is SHORTER than cmd/server's own %s shutdown budget and SIGKILLs the pod mid-sequence", budget)
	grace := time.Duration(*values.TerminationGracePeriodSeconds) * time.Second
	assert.GreaterOrEqual(t, grace, budget+shutdownMargin,
		"helm/loam's terminationGracePeriodSeconds is %s, but cmd/server's bounded shutdown steps sum to %s and need %s of margin above that for SIGTERM delivery, Go signal handling and the deferred pgxpool close. %s is the FLOOR, not a safe value -- setting it exactly reproduces the zero margin this test exists to remove.",
		grace, budget, shutdownMargin, budget)
}

// TestDeploymentTemplateConsumesTheGracePeriod is the test whose absence
// made every other test in this file prove the wrong thing, and it was found
// by mutation: deleting the terminationGracePeriodSeconds line from
// templates/deployment.yaml left all fourteen tests green while restoring
// the ORIGINAL loam-uwus defect exactly.
//
// The tests around this one read values.yaml, values.schema.json and the
// compose file. Every one of them proves the number is DECLARED and
// VALIDATED. Not one of them proves it reaches a pod -- and "a values key
// that exists and goes nowhere" is not an incidental gap here, it is
// verbatim the failure this bead exists to end. The old chart's problem was
// never that its values were wrong; it was that they were unconsumed.
//
// So this checks the consuming end: the Pod spec must set the field, and it
// must set it FROM the value rather than from a literal, since a hardcoded
// 45 would satisfy the first half while quietly ignoring every override a
// consumer's valuesObject makes.
func TestDeploymentTemplateConsumesTheGracePeriod(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(helmTemplatesDir, "deployment.yaml"))
	require.NoError(t, err)
	body := helmYAMLComment.ReplaceAllString(helmCommentBlock.ReplaceAllString(string(raw), ""), "")
	assert.Contains(t, body, "terminationGracePeriodSeconds:",
		"templates/deployment.yaml sets no terminationGracePeriodSeconds, so Kubernetes' 30s default applies and the pod is SIGKILLed mid-shutdown -- exactly the loam-uwus defect. values.yaml declaring the value changes nothing on its own; an unconsumed values key is what this whole bead is about")
	assert.Contains(t, body, ".Values.terminationGracePeriodSeconds",
		"templates/deployment.yaml sets terminationGracePeriodSeconds from something other than .Values.terminationGracePeriodSeconds; a literal here satisfies the check above while silently ignoring every consumer override, which is the same class of failure one level down")
}

// TestComposeGracePeriodExceedsGoShutdownBudget is the same property for the
// single-machine stack, and the exposure there is WORSE than Kubernetes',
// which is easy to miss when k8s is the deployment you are thinking about.
// Compose's stop_grace_period defaults to TEN seconds: `docker compose down`
// against an unset value SIGKILLs loam a third of the way into its own 30s
// drain, losing in-flight git smart-HTTP pushes and running ingest work, and
// never reaching the flush at all.
func TestComposeGracePeriodExceedsGoShutdownBudget(t *testing.T) {
	t.Parallel()
	budget := totalShutdownBudget(t)
	deploy := loadCompose(t, deployComposePath)
	loam, ok := deploy.Services["loam"]
	require.True(t, ok)
	require.NotEmpty(t, loam.StopGracePeriod,
		"deploy/docker-compose.yml's loam service sets no stop_grace_period, so compose's default of 10s applies -- a third of cmd/server's %s shutdown budget", budget)
	grace, err := time.ParseDuration(loam.StopGracePeriod)
	require.NoError(t, err, "stop_grace_period %q is not a duration compose will accept", loam.StopGracePeriod)
	assert.GreaterOrEqual(t, grace, budget+shutdownMargin,
		"deploy/docker-compose.yml's stop_grace_period is %s, but cmd/server's bounded shutdown steps sum to %s and need %s of margin above that",
		grace, budget, shutdownMargin)
}

// TestValuesSchemaGracePeriodFloorMatchesGoConstants closes the one gap the
// two tests above leave wide open, and it is not a small one.
//
// Both of them read helm/loam/values.yaml -- the chart's DEFAULT. But every
// real consumer of this chart overrides values (an ArgoCD Application's
// spec.source.helm.valuesObject, per values.yaml's own header), and an
// override reaches no Go test at all. The only thing standing between a
// consumer and `terminationGracePeriodSeconds: 20` is values.schema.json's
// `minimum`, which is enforced by Helm at render time in that consumer's
// pipeline, exactly where a Go test cannot go.
//
// That minimum is therefore load-bearing, and it is a hand-written number in
// a JSON file -- precisely the shape doc.go warns rots. So this test
// re-derives it from cmd/server's constants and asserts the schema still
// states it. Tuning a Go timeout without updating the schema is a red test
// here, not a bound that silently stopped meaning anything.
//
// It re-derives FLOOR PLUS MARGIN, not the bare floor, and the distinction
// is the whole point of the test rather than a detail of it. An earlier
// version asserted the sum alone, which set the schema minimum to 35 while
// values.yaml, the compose file and the two tests above all required 45 --
// so the single guard that a consumer's own valuesObject actually passes
// through was the one place admitting the value everything else in this
// change argues is unsafe. A schema floor below the required value is not a
// lenient floor; it is a floor that does not enforce the thing it exists for.
func TestValuesSchemaGracePeriodFloorMatchesGoConstants(t *testing.T) {
	t.Parallel()
	budget := totalShutdownBudget(t)
	raw, err := os.ReadFile(helmValuesSchemaPath)
	require.NoError(t, err)
	var schema struct {
		Properties struct {
			TerminationGracePeriodSeconds struct {
				Minimum *float64 `json:"minimum"`
			} `json:"terminationGracePeriodSeconds"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(raw, &schema))
	minimum := schema.Properties.TerminationGracePeriodSeconds.Minimum
	require.NotNil(t, minimum,
		"values.schema.json declares no minimum for terminationGracePeriodSeconds; a consumer's valuesObject could set it below cmd/server's %s shutdown budget and Helm would render it happily", budget)
	required := budget + shutdownMargin
	assert.Equal(t, required.Seconds(), *minimum,
		"values.schema.json's terminationGracePeriodSeconds minimum is %v, but cmd/server's bounded shutdown steps sum to %s and the required value is %s (sum plus %s of margin). The schema is the ONLY guard a consumer's own valuesObject passes through, so it must enforce what is actually required -- a minimum set to the bare sum admits precisely the zero-margin value everything else here refuses.",
		*minimum, budget, required, shutdownMargin)
}
