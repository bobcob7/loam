package testembed

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The "validate"/"migration" pair below reproduces the exact collision from
// the loam-li0.2 incident this guard exists to catch (bd show loam-phm,
// DESCRIPTION): at dimension 32 those two tokens landed in the same
// FNV-1a bucket and inverted the monotonic-cosine ranking property. It was
// regenerated (not hand-picked) by brute-force scanning a ~50-word list of
// software/testing vocabulary (auth, token, session, validate, migration,
// commit, branch, chunk, ...) through fnv.New32a().Sum32() % dim for
// dim in {4, 8, 16, 32, 64}; "validate" and "migration" first collide at
// dim=32 (bucket 11) and keep colliding through dim=64. To regenerate:
// hash every candidate token, group by hash%dim, and print any group with
// more than one member.
func TestCollidingTokens_KnownPairAtSmallDimensionNamesBothTokens(t *testing.T) {
	t.Parallel()
	got := collidingTokensAt(32, "validate", "migration")
	assert.Equal(t, [][]string{{"migration", "validate"}}, got)
}

// "query" and "dimension" collide at the real, production Dimension=768
// (bucket 483) — found the same way as above, scanning the same candidate
// list at dim=768 instead of a small dimension. Notably, these two tokens
// already coexist in this package's own test corpus (TestEmbed_Deterministic
// uses "query", TestDimension_IsStableAndMatchesVectorWidth uses
// "dimension"), harmlessly, because those two texts are never compared to
// each other for ranking. That is precisely the failure mode this guard
// exists to make loud instead of silent.
func TestCollidingTokens_KnownPairAtProductionDimensionNamesBothTokens(t *testing.T) {
	t.Parallel()
	got := CollidingTokens("an auth query", "a stable dimension")
	assert.Equal(t, [][]string{{"dimension", "query"}}, got)
}

// This is the vocabulary TestEmbed_CosineIncreasesWithSharedTokenCount
// actually depends on for the ranking property docs/testing-spec.md:41-44
// describes ("the auth chunk ranks first for an auth query"): the query
// "auth token validate session" against the four docs of increasing overlap.
// It must stay collision-free at the real Dimension for that test to mean
// anything.
func TestCollidingTokens_CurrentRankingVocabularyIsCollisionFreeAt768(t *testing.T) {
	t.Parallel()
	got := CollidingTokens(
		"auth token validate session",
		"database migration schema export",
		"auth database migration schema",
		"auth token database migration",
		"auth token validate database",
	)
	assert.Empty(t, got)
}

func TestCollidingTokens_EmptyVocabularyReturnsNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, CollidingTokens())
	assert.Nil(t, CollidingTokens(""))
}

// The guard must tokenize exactly like Embed. TestEmbed_TextWithNoTokensYieldsZeroVector
// establishes that Embed treats "--- !!! ???" as contributing zero tokens
// (an all-punctuation string, no [a-z0-9]+ match). If the guard tokenized
// differently, adding that same string alongside a known-colliding pair
// would perturb the reported groups; it must not.
func TestCollidingTokens_TokenizationMatchesEmbed_NoTokenTextContributesNothing(t *testing.T) {
	t.Parallel()
	require.Empty(t, tokenize("--- !!! ???"), "precondition: Embed's own tokenizer must see no tokens here")
	without := collidingTokensAt(32, "validate", "migration")
	with := collidingTokensAt(32, "validate", "migration", "--- !!! ???")
	assert.Equal(t, without, with, "a string Embed tokenizes to nothing must not change the reported collision groups")
}

// Grouping goes through a map, so without sorting the reported order would
// depend on Go's randomized map iteration. This pins the fully sorted shape
// — both which group comes first and which token comes first within a
// group — across repeated calls in the same process.
func TestCollidingTokens_OutputOrderingIsDeterministicAcrossRepeatedCalls(t *testing.T) {
	t.Parallel()
	texts := []string{"an auth query", "a stable dimension", "validate", "migration"}
	first := collidingTokensAt(32, texts...)
	want := [][]string{{"dimension", "query"}, {"migration", "validate"}}
	require.Equal(t, want, first)
	for i := 0; i < 10; i++ {
		got := collidingTokensAt(32, texts...)
		assert.Equal(t, first, got, "call %d should reproduce the same group order and within-group order", i)
	}
}

func TestRequireNoCollisions_ReturnsNilForCollisionFreeVocabulary(t *testing.T) {
	t.Parallel()
	err := RequireNoCollisions("auth token validate session", "database migration schema export")
	assert.NoError(t, err)
}

func TestRequireNoCollisions_NamesBothCollidingTokens(t *testing.T) {
	t.Parallel()
	err := RequireNoCollisions("an auth query", "a stable dimension")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query")
	assert.Contains(t, err.Error(), "dimension")
}
