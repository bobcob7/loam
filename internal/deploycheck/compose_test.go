package deploycheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bobcob7/loam/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	deployComposePath = "../../deploy/docker-compose.yml"
	e2eComposePath    = "../../deploy/docker-compose.e2e.yml"
	helmValuesPath    = "../../helm/loam/values.yaml"
	configPkgDir      = "../../internal/config"
)

// composeFile is the sliver of the compose schema these tests read. It is
// deliberately partial: yaml.v3 ignores keys with no field, so this stays
// valid as the real files grow, and a test that needs another key adds a
// field rather than a second parser.
type composeFile struct {
	Services map[string]struct {
		Image       string            `yaml:"image"`
		Environment map[string]string `yaml:"environment"`
	} `yaml:"services"`
}

func loadCompose(t *testing.T, path string) composeFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)
	var parsed composeFile
	require.NoError(t, yaml.Unmarshal(raw, &parsed), "parsing %s", path)
	require.NotEmpty(t, parsed.Services, "%s declares no services", path)
	return parsed
}

// interpolationDefault strips a compose ${VAR:-default} interpolation down
// to its default, and leaves anything else alone. deploy/docker-compose.yml
// writes its images as ${LOAM_IMAGE:-<pinned>} so an operator can override
// one without editing the file; what these tests care about is the pinned
// value that ships, which is the default.
var interpolationDefault = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*:-(.*)\}$`)

func pinnedImage(raw string) string {
	if m := interpolationDefault.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	return raw
}

// TestPostgresImageAgrees is the one fact deploy/docker-compose.yml,
// deploy/docker-compose.e2e.yml and helm/loam/values.yaml genuinely must
// share, and the one that rots silently.
//
// If the deployment stack and the e2e stack disagree about the pgvector
// version, the suite that is supposed to prove loam works is proving it
// against a database nobody runs -- and the failure surfaces as a migration
// or a vector-index error in production, long after the green build that
// missed it. helm/loam is in the comparison for the same reason one step
// further out: docs/deployment-spec.md records that its postgres.image and
// the e2e file's are pinned together deliberately, "no version drift in the
// extension between the two", and a claim that lives only in prose is a
// claim nothing checks.
//
// Note what this test does NOT do: it does not assert the tag is
// "pgvector/pgvector:pg16". Writing the expected value here would just move
// the hand-maintained copy from three files to four, and a legitimate
// upgrade would then be a two-place edit that still passes when you forget
// the third. It asserts only that the three agree, so an upgrade is a
// three-file change or a red test.
func TestPostgresImageAgrees(t *testing.T) {
	t.Parallel()
	deploy := loadCompose(t, deployComposePath)
	e2e := loadCompose(t, e2eComposePath)
	deployPG, ok := deploy.Services["postgres"]
	require.True(t, ok, "%s has no postgres service", deployComposePath)
	e2ePG, ok := e2e.Services["postgres"]
	require.True(t, ok, "%s has no postgres service", e2eComposePath)
	deployImage := pinnedImage(deployPG.Image)
	require.NotEmpty(t, deployImage)
	assert.Equal(t, deployImage, pinnedImage(e2ePG.Image),
		"the deployment stack and the e2e stack must run the SAME Postgres image: a skew means the e2e suite is exercising a pgvector version nobody deploys")
	raw, err := os.ReadFile(helmValuesPath)
	require.NoError(t, err)
	var helm struct {
		Postgres struct {
			Image string `yaml:"image"`
		} `yaml:"postgres"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &helm))
	assert.Equal(t, deployImage, helm.Postgres.Image,
		"helm/loam's postgres.image must match the compose stacks: docs/deployment-spec.md states they are pinned together, and nothing but this test checks it")
}

// TestPgvectorIsUsedNotPlainPostgres guards the one property the
// agreement test above cannot see: all three could agree on the WRONG
// image. loam's migrations create vector columns, so a stock postgres
// image fails on the very first boot of a fresh database with `type
// "vector" does not exist` -- a failure that reads like a loam bug.
func TestPgvectorIsUsedNotPlainPostgres(t *testing.T) {
	t.Parallel()
	deploy := loadCompose(t, deployComposePath)
	image := pinnedImage(deploy.Services["postgres"].Image)
	assert.Contains(t, image, "pgvector",
		"plain postgres cannot run loam's migrations; the image must carry the pgvector extension")
}

// TestE2EStackRunsNoLoamServer pins the deliberate difference between the
// two compose files, so that "reconciling" them later cannot quietly erase
// it. deploy/docker-compose.e2e.yml runs NO loam service on purpose: `task
// test:e2e` builds the real binaries and runs the server on the host
// against these containers, which is what makes the artifact under test
// byte-for-byte the one `task build:bin` produces. Someone tidying the two
// files toward each other would "fix" that omission first.
func TestE2EStackRunsNoLoamServer(t *testing.T) {
	t.Parallel()
	e2e := loadCompose(t, e2eComposePath)
	assert.NotContains(t, e2e.Services, "loam",
		"the e2e stack deliberately runs the server as a HOST binary, not a container -- see that file's header and docs/testing-spec.md Layer 3")
	assert.Contains(t, e2e.Services, "forgejo",
		"the e2e stack's seeded real Forgejo is the point of Layer 3; the deployment stack is where it does not belong")
	deploy := loadCompose(t, deployComposePath)
	assert.Contains(t, deploy.Services, "loam",
		"the deployment stack exists to run the server; if it stops doing that it is not a deployment")
	assert.NotContains(t, deploy.Services, "forgejo",
		"a deployment has no business shipping an upstream forge")
}

// TestComposeSetsOnlyRealConfigVariables catches the rename and the typo,
// which are the same bug wearing different hats and are both silent.
// internal/config reads its environment by exact string; a compose file
// that sets LOAM_EMBEDER_URL, or that keeps setting a variable after
// internal/config renamed it, does not fail -- the server starts and
// applies the default instead, and the operator's setting is ignored with
// no message anywhere.
//
// The expected set is DISCOVERED from internal/config's own source (every
// LOAM_* string literal in the package), not written down here. A literal
// list would be one more copy to forget.
func TestComposeSetsOnlyRealConfigVariables(t *testing.T) {
	t.Parallel()
	known := configEnvNames(t)
	require.NotEmpty(t, known, "found no LOAM_* names in internal/config; the discovery below is broken, not the compose file")
	deploy := loadCompose(t, deployComposePath)
	loam, ok := deploy.Services["loam"]
	require.True(t, ok)
	require.NotEmpty(t, loam.Environment)
	for _, name := range sortedKeys(loam.Environment) {
		if !strings.HasPrefix(name, "LOAM_") {
			continue
		}
		assert.Contains(t, known, name,
			"deploy/docker-compose.yml sets %s, which internal/config never reads: either it is a typo or internal/config renamed it, and both silently apply the default instead", name)
	}
}

// TestComposeEnvironmentSatisfiesConfigLoad is the direction that turns rot
// into a server that will not boot, and it does not hold an opinion about
// WHICH variables are required -- it asks internal/config, by running
// config.Load() against the environment this compose file actually
// produces.
//
// The earlier version of this test carried a hand-written list of the
// required names, which is the very pattern TestPostgresImageAgrees exists
// to avoid, and it had exactly the hole that pattern always has. Two
// mutations survived it, both compiling, both leaving the whole tree green
// while producing a server that exits during config load: adding a
// lookupRequired("LOAM_WEBHOOK_SECRET") to internal/config without adding it
// to the compose file, and deleting LOAM_DB_NAME from the compose file. A
// list cannot catch a requirement it was never told about.
//
// Running the loader catches both, and every future one, for free: it is
// not a model of internal/config's required set, it IS internal/config. It
// also subsumes the exclusive-or between LOAM_DATABASE_URL and the discrete
// LOAM_DB_* parts (resolveDatabaseURL rejects both together AND rejects
// neither) without this test having to know that rule exists.
//
// Not parallel: t.Setenv is process-global and the testing package forbids
// combining the two.
func TestComposeEnvironmentSatisfiesConfigLoad(t *testing.T) {
	deploy := loadCompose(t, deployComposePath)
	loam, ok := deploy.Services["loam"]
	require.True(t, ok)
	// Blank every name internal/config reads before setting anything, so an
	// ambient LOAM_* in the developer's or CI's shell can neither rescue a
	// compose file that stopped setting a variable nor break one that
	// didn't. Discovered from the same AST walk, so a newly-read variable
	// is blanked without anyone remembering to add it here.
	for _, name := range configEnvNames(t) {
		t.Setenv(name, "")
	}
	for name, raw := range loam.Environment {
		t.Setenv(name, resolveComposeValue(t, name, raw))
	}
	// The one deliberate substitution. The compose file points
	// LOAM_DATA_DIR at the container's /var/lib/loam, which this test's
	// host neither has nor should create; Load's final step probes it for
	// writability. Pointing it at a temp dir keeps that probe honest
	// without asserting anything about the host. Ownership of the real
	// path is the container's problem and is covered by internal/config's
	// own TestLoad_UnwritableDataDirErrorNamesUIDAndPath.
	t.Setenv("LOAM_DATA_DIR", t.TempDir())
	_, err := config.Load()
	require.NoError(t, err,
		"the environment deploy/docker-compose.yml gives the loam service does not satisfy internal/config: a server started from this file would exit during config load with exactly this error")
}

// composeMustSet reports the environment keys of one service whose value
// uses compose's ${VAR:?message} form -- the ones an operator has to supply
// before compose will render the file at all.
func composeMustSet(env map[string]string) map[string]struct{} {
	out := map[string]struct{}{}
	for name, raw := range env {
		if mustSetInterpolation.MatchString(raw) {
			out[name] = struct{}{}
		}
	}
	return out
}

var mustSetInterpolation = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*:\?`)

// operatorSuppliedValues stands in for the human filling out deploy/.env.
// This side of the contract genuinely cannot be discovered -- nothing in
// the repository knows what your admin password is -- so it is written
// down, and TestOperatorSuppliedValuesCoverEveryMustSetVariable below fails
// the moment the compose file grows a must-set variable this map does not
// answer for. That check is what keeps the map from going stale silently,
// which is the failure mode of every hand-written list in this package.
//
// LOAM_ENCRYPTION_KEY is base64 of exactly 32 bytes because internal/config
// validates the decoded length; the rest are shaped like what a real
// operator would paste.
var operatorSuppliedValues = map[string]string{
	"LOAM_ADMIN_PASSWORD": "deploycheck-admin-password",
	"LOAM_DB_PASSWORD":    "deploycheck-db-password",
	"LOAM_ENCRYPTION_KEY": "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
	"LOAM_EMBEDDER_URL":   "http://embedder.invalid:11434",
	"POSTGRES_PASSWORD":   "deploycheck-db-password",
}

// resolveComposeValue turns one compose environment value into the string a
// container would actually receive: a ${VAR:?...} becomes what the operator
// supplies, a ${VAR:-default} becomes its default (nothing here exports the
// outer variable), and anything else is already literal.
func resolveComposeValue(t *testing.T, name, raw string) string {
	t.Helper()
	if mustSetInterpolation.MatchString(raw) {
		value, ok := operatorSuppliedValues[name]
		require.True(t, ok, "%s is must-set in the compose file but operatorSuppliedValues has no value for it", name)
		return value
	}
	if m := interpolationDefault.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	require.NotContains(t, raw, "${", "%s uses an interpolation form this test does not model: %s", name, raw)
	return raw
}

// TestOperatorSuppliedValuesCoverEveryMustSetVariable keeps the one
// hand-written map in this package from rotting. Without it, adding a new
// ${VAR:?...} to the compose file would leave
// TestComposeEnvironmentSatisfiesConfigLoad unable to supply a value --
// and the point is that the failure names the missing variable rather than
// arriving as a confusing config error.
func TestOperatorSuppliedValuesCoverEveryMustSetVariable(t *testing.T) {
	t.Parallel()
	deploy := loadCompose(t, deployComposePath)
	for service, spec := range deploy.Services {
		for name := range composeMustSet(spec.Environment) {
			assert.Contains(t, operatorSuppliedValues, name,
				"%s.%s is must-set in deploy/docker-compose.yml but operatorSuppliedValues (compose_test.go) has no value for it", service, name)
		}
	}
}

// TestMustSetVariablesHaveNoWorkingDefault is the security decision from
// loam-lzxo.2, asserted rather than left to the reader of a comment. Each
// of these three uses compose's `${VAR:?message}` form, which makes compose
// refuse to render the file at all when the variable is unset. The failure
// mode this prevents is not a missing feature -- it is a stack that comes
// up SUCCESSFULLY with a password published in this repository, on a
// service whose agent surface has no authentication, and that looks
// identical from the outside to a correctly configured one.
//
// LOAM_EMBEDDER_URL rides along for a related reason: it is not a secret,
// but no value is CORRECT for everyone, and internal/config's own default
// resolves to the loam container itself. A working default there would
// mean silent ingest failure rather than a legible refusal to start.
func TestMustSetVariablesHaveNoWorkingDefault(t *testing.T) {
	t.Parallel()
	deploy := loadCompose(t, deployComposePath)
	loam := deploy.Services["loam"]
	postgres := deploy.Services["postgres"]
	cases := map[string]string{
		"loam.LOAM_ADMIN_PASSWORD":   loam.Environment["LOAM_ADMIN_PASSWORD"],
		"loam.LOAM_ENCRYPTION_KEY":   loam.Environment["LOAM_ENCRYPTION_KEY"],
		"loam.LOAM_DB_PASSWORD":      loam.Environment["LOAM_DB_PASSWORD"],
		"loam.LOAM_EMBEDDER_URL":     loam.Environment["LOAM_EMBEDDER_URL"],
		"postgres.POSTGRES_PASSWORD": postgres.Environment["POSTGRES_PASSWORD"],
	}
	for name, value := range cases {
		require.NotEmpty(t, value, "%s is not set at all", name)
		assert.Contains(t, value, ":?",
			"%s must use compose's ${VAR:?message} form so an unset value is a legible render-time error, never a working default", name)
	}
	// The list above is the POLICY -- these specific values must never
	// have a default -- and a policy is not discoverable from anywhere in
	// the repository, so it is written down. What IS discoverable is the
	// general shape of the mistake, and this second pass catches it for
	// variables nobody has thought of yet: any name that announces itself
	// as credential material must not carry a working default, whichever
	// service it belongs to and whenever it is added.
	credentialish := regexp.MustCompile(`(?i)(password|secret|token|_key$|encryption)`)
	for service, spec := range deploy.Services {
		for name, value := range spec.Environment {
			if !credentialish.MatchString(name) {
				continue
			}
			assert.Regexp(t, mustSetInterpolation, value,
				"%s.%s looks like credential material but has a working default; a stack that boots with a secret published in this repository is indistinguishable from a correctly configured one", service, name)
		}
	}
}

// configEnvNames returns every LOAM_* string literal appearing in
// internal/config's non-test sources. internal/config reads its
// environment exclusively through helpers taking a literal key
// (lookupRequired, lookupDefault, the parse*Env family, isEnvSet) plus the
// databaseURLPartKeys slice, so the literals ARE the surface -- there is no
// computed key to miss. Walking the AST rather than grepping means a name
// mentioned only in a comment does not count.
func configEnvNames(t *testing.T) []string {
	t.Helper()
	sources, err := filepath.Glob(filepath.Join(configPkgDir, "*.go"))
	require.NoError(t, err)
	fset := token.NewFileSet()
	seen := map[string]struct{}{}
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, "parsing %s", path)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if strings.HasPrefix(value, "LOAM_") {
				seen[value] = struct{}{}
			}
			return true
		})
	}
	return sortedKeys(seen)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
