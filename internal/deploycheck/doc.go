// Package deploycheck holds no code. It exists so that the deployment
// artifacts that are not Go -- deploy/docker-compose.yml,
// deploy/docker-compose.e2e.yml, helm/loam/values.yaml -- can be guarded by
// ordinary `go test`, at unit-test speed, with no Docker daemon and no
// cluster (loam-lzxo.7).
//
// The rot these tests exist to catch is the quiet kind. Three files pin the
// Postgres image and one of them can be bumped alone; the compose file names
// a dozen LOAM_* variables and internal/config can rename one. Neither
// failure announces itself: the first means the e2e suite is exercising a
// pgvector nobody deploys, and the second means a variable is silently
// ignored and its default silently applied. A comment saying "keep these in
// sync" is documentation; this package is a guard.
//
// These tests DISCOVER the values they compare wherever discovery is
// possible -- parsing the YAML and the Go AST, and in one case running
// config.Load itself -- rather than restating what those files are supposed
// to contain. A hand-maintained list of expected values plus a test
// asserting the list is the same thing as the comment, only harder to read:
// it goes stale in exactly the situations it was written for. An earlier
// version of compose_test.go listed the required LOAM_* variables by hand
// and two mutations walked straight through it (a new lookupRequired in
// internal/config, and a deleted LOAM_DB_NAME in the compose file), which
// is why TestComposeEnvironmentSatisfiesConfigLoad now asks the loader
// instead of modelling it.
//
// Two things here are NOT discovered, both deliberately, and each is
// guarded by a test whose only job is to stop it going stale:
//
//   - operatorSuppliedValues, the stand-in for a human filling out
//     deploy/.env. Nothing in the repository knows what your admin password
//     is. TestOperatorSuppliedValuesCoverEveryMustSetVariable fails the
//     moment the compose file grows a must-set variable the map does not
//     answer for.
//   - the explicit list in TestMustSetVariablesHaveNoWorkingDefault, which
//     encodes a POLICY (these particular values must never have a working
//     default) rather than a fact. That test also carries a discovered pass
//     over every credential-shaped name in either compose file, so a secret
//     nobody has thought of yet is still caught.
//
// helm_test.go and shutdown_test.go (loam-uwus) extend the same idea to the
// chart, which until then had exactly one fact checked about it -- its
// postgres.image tag -- and to a relationship that had never been checked at
// all. Both were added because the failures they catch had already happened:
//
//   - THE CHART COULD NOT EXPRESS A VARIABLE AT ALL, AND SAID NOTHING.
//     helm/loam was byte-identical across three releases and named none of
//     internal/config's LOAM_OTEL_* variables; its ConfigMap emits a fixed
//     key list and its Deployment has no extraEnv or second envFrom, so
//     there was no route from a values file to those variables. Setting them
//     rendered clean, synced green in ArgoCD, and exported nothing.
//     TestHelmChartCanCarryEveryConfigVariable states the property both
//     sides discovered: every name internal/config reads must appear
//     somewhere in the templates.
//
//     Note what makes this a DIFFERENT test rather than a duplicate of
//     TestComposeEnvironmentSatisfiesConfigLoad. Running config.Load only
//     complains about REQUIRED variables; every optional one -- which is all
//     three OTel variables -- is invisible to it. For compose, the reverse
//     direction (TestComposeSetsOnlyRealConfigVariables) covers the optional
//     ones by name, but only for variables the compose file actually sets,
//     which is why the OTel variables are set there to an empty default
//     rather than left out or commented: a name present in the file is a
//     name that test checks. helm_test.go covers both directions for the
//     chart.
//
//     One corner of that gap survived all of the above and was found by
//     mutation rather than by reading: DELETING an optional variable from
//     deploy/docker-compose.yml. The name-checking test only sees names the
//     file still sets, and the config.Load test only sees required ones, so
//     the stack would quietly stop passing a setting through.
//     TestComposeOffersTheSameKnobsAsTheChart closes it with a property
//     rather than a list -- the two deployment stacks should expose the same
//     configuration surface -- comparing the compose environment against the
//     chart's ConfigMap, both discovered.
//
//   - A NUMBER IN YAML DERIVED FROM CONSTANTS IN GO, WITH NOTHING JOINING
//     THEM. cmd/server drains for 30s and then flushes telemetry for a
//     further 5s on its own context; nothing in helm/ set
//     terminationGracePeriodSeconds, so Kubernetes' 30s default SIGKILLed
//     the pod mid-sequence, and compose's stop_grace_period default of 10s
//     was worse. The fix is a number in two YAML files and a JSON schema,
//     which is three copies of an arithmetic living in Go. So
//     shutdown_test.go parses cmd/server's constants out of the AST and
//     asserts the RELATIONSHIP rather than the number -- tuning a timeout in
//     Go turns those tests red instead of leaving three stale copies. The
//     one thing it does write down is the required MARGIN above the sum,
//     which is a policy in the same sense as the list in
//     TestMustSetVariablesHaveNoWorkingDefault: not derivable from anything.
//
// It lives in internal/ rather than under deploy/ so that deploy/ stays what
// it looks like -- a directory of YAML -- and so `go build ./...` has a
// normal package to build rather than a test-only directory.
//
// WHAT THIS PACKAGE STILL CANNOT SEE, so that nobody reads a green run as
// more than it is: it never renders the chart. `helm template` is what
// proves an unknown values key is REJECTED rather than ignored, that the
// OTel variables reach the pod spec when set, and that a release setting
// none of them renders unchanged -- all three were run by hand on loam-uwus
// and none of them is re-run here, because requiring a helm binary would
// cost this package the property that makes it useful: it is an ordinary
// unit test.
package deploycheck
