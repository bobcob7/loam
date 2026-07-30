//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag. Run explicitly with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/handler/repoadmin/... -run TestRepoNameCheckConstraint -v
//
// (see internal/db/migrations/integration_test.go for why
// TESTCONTAINERS_RYUK_DISABLED is a podman-only workaround, not a CI
// setting).
package repoadmin

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoNameCases is the shared table this test drives both validRepoName
// and a real INSERT INTO repos through: exactly two non-empty segments,
// each starting with an alphanumeric and containing only
// alphanumerics/'.'/'_'/'-'. It mirrors internal/handler/git's own
// TestValidRepoName_Table fixture -- the same shapes that bead's review
// named -- since 0003_repos_name_check's CHECK constraint is meant to
// agree with this exact allowlist, not a looser or stricter one.
var repoNameCases = []struct {
	name string
	ok   bool
}{
	{"acme/widgets", true},
	{"acme/doc-server.wiki", true},
	{"acme.corp/wid_gets.v2", true},
	{"a/b", true},
	{"../../../tmp/evil", false},
	{"acme/..", false},
	{"acme/.", false},
	{"acme//evil", false},
	{"/acme", false},
	{"acme/", false},
	{"acme", false},
	{"acme/sub/widgets", false},
	{"", false},
	{".acme/widgets", false},
	{"acme/.widgets", false},
	{"-acme/widgets", false},
	{"acme/widgets ", false},
}

// expectedRepoNameCheckPattern is the regex 0003_repos_name_check.up.sql
// bakes into the database, restated here so this test can assert the live
// constraint's definition actually contains it -- catching a migration
// edited to some other pattern that this test's behavioral cases (below)
// might not happen to exercise.
const expectedRepoNameCheckPattern = `^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`

// TestRepoNameCheckConstraint_AgreesWithValidRepoNameInBothDirections is the
// drift guard between the two statements of the repo-name shape that
// cannot be expressed as one:
//
//   - validRepoName, which internal/handler/repoadmin.EnrollRepo validates
//     every enroll request's derived identifier against;
//   - repos_name_check in migration 0003_repos_name_check, which the
//     database enforces underneath it for every write path, including ones
//     that bypass EnrollRepo entirely.
//
// Both directions matter and both are checked, per name in repoNameCases:
//
//   - a name validRepoName accepts but the database rejects would make
//     EnrollRepo's own happy path fail at INSERT time -- the constraint
//     would be stricter than the validator;
//   - a name validRepoName rejects but the database accepts would leave the
//     loam-ofg.16-review traversal gap open for any write path that skips
//     validRepoName -- exactly what this constraint exists to close, so a
//     looser constraint defends nothing.
//
// This is the same treatment internal/handler/role's
// TestDatabaseCheckConstraintMatchesTheGoVocabulary gives the capability
// vocabulary: an inventory that cannot be checked WILL drift.
func TestRepoNameCheckConstraint_AgreesWithValidRepoNameInBothDirections(t *testing.T) {
	ctx := t.Context()
	pool := newTestPostgresPool(t)
	var definition string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'repos_name_check'`,
	).Scan(&definition), "the repos_name_check CHECK constraint must exist under that exact name")
	assert.Contains(t, definition, expectedRepoNameCheckPattern,
		"repos_name_check's live definition must contain the exact pattern 0003_repos_name_check.up.sql bakes in: %s", definition)
	for _, tc := range repoNameCases {
		t.Run(tc.name, func(t *testing.T) {
			gotValidRepoName := validRepoName(tc.name)
			assert.Equal(t, tc.ok, gotValidRepoName, "test fixture and validRepoName disagree on %q -- fix the fixture, not the assertion", tc.name)
			_, err := pool.Exec(ctx,
				`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, 'https://example.com/repo.git', 'example.com', 'main')`,
				uuid.New(), tc.name,
			)
			if tc.ok {
				assert.NoErrorf(t, err, "validRepoName accepts %q but repos_name_check rejected the INSERT -- the constraint is stricter than the Go validator", tc.name)
				return
			}
			require.Errorf(t, err, "validRepoName rejects %q but repos_name_check accepted the INSERT -- the constraint is looser than the Go validator and does not defend the write path", tc.name)
			assert.Contains(t, err.Error(), "repos_name_check", "the INSERT must fail specifically on repos_name_check, not some other constraint, for %q", tc.name)
		})
	}
}
