package testembed

import (
	"fmt"
	"sort"
	"strings"
)

// CollidingTokens tokenizes texts exactly as Embed does (see tokenize) and
// reports every group of two or more distinct tokens that hash into the same
// bucket at Dimension. It returns nil when none of the supplied texts share
// a bucket.
//
// Collisions at a fixed Dimension are unavoidable in general (pigeonhole:
// once a vocabulary exceeds Dimension distinct tokens, some pair must
// collide), and this is not a defect in the FNV-1a projection — see the
// package doc and docs/testing-spec.md, "Deterministic embedder". Most
// collisions are also harmless: a colliding pair only distorts a ranking if
// both tokens appear in texts that are actually compared to each other for
// that ranking. This is birthday-bound, not bad luck — even a vocabulary of
// ~50 distinct tokens already has roughly 80% odds of some collision at
// Dimension=768 — so calling this over an entire fixture's full vocabulary
// reports mostly noise, not signal. This package's own test corpus is a live
// example: "query" (used in TestEmbed_Deterministic) and "dimension" (used
// in TestDimension_IsStableAndMatchesVectorWidth) collide at bucket 483, and
// that is harmless, because those two texts are never ranked against each
// other.
//
// The scope that matters is co-ranked vocabulary: a query and the specific
// document texts it will actually be ranked against (e.g. the corpus behind
// TestEmbed_CosineIncreasesWithSharedTokenCount — see rankingQuery and
// rankingDocs in embedder_test.go). Call CollidingTokens (or
// CheckNoCollisions) with exactly that query plus those documents before
// asserting a ranking property, not with a fixture's entire vocabulary; a
// future co-ranked form (CollidingTokensAcross(queryTexts, docTexts) —
// tracked as a follow-up) will make this scoping the default instead of a
// convention callers must apply themselves.
//
// The returned groups are sorted for a stable, diffable failure message
// across runs and CI: tokens within a group are sorted, and groups are
// sorted by their first (smallest) token.
func CollidingTokens(texts ...string) [][]string {
	return collidingTokensAt(Dimension, texts...)
}

// collidingTokensAt is CollidingTokens parameterized by dimension. Production
// code only ever calls it with the fixed Dimension (via CollidingTokens);
// the parameter exists so tests can search for and pin known collisions at a
// small, easily-searched dimension without disturbing the real 768-wide
// projection every other test in this package relies on.
func collidingTokensAt(dimension int, texts ...string) [][]string {
	buckets := make(map[int]map[string]struct{})
	for _, text := range texts {
		for _, token := range tokenize(text) {
			bucket := tokenIndexAt(token, dimension)
			if buckets[bucket] == nil {
				buckets[bucket] = make(map[string]struct{})
			}
			buckets[bucket][token] = struct{}{}
		}
	}
	groups := make([][]string, 0, len(buckets))
	for _, tokens := range buckets {
		if len(tokens) < 2 {
			continue
		}
		group := make([]string, 0, len(tokens))
		for token := range tokens {
			group = append(group, token)
		}
		sort.Strings(group)
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	if len(groups) == 0 {
		return nil
	}
	return groups
}

// CheckNoCollisions is the one-line convenience form of CollidingTokens: nil
// if none of texts' tokens collide at Dimension, or an error naming every
// colliding token group (and its bucket, for quick debugging) otherwise. See
// CollidingTokens for scope: pass a query and the specific documents it is
// ranked against, not a fixture's whole vocabulary.
//
// It deliberately takes no testing.TB. Step definitions have none in scope,
// so they call this directly. Suite setup does have one — godog suites are
// launched from a plain `func TestFeatures(t *testing.T)` that calls
// godog.TestSuite{}.Run() (godog@v0.15.1 has no error-returning BeforeSuite
// hook) — so a one-time check belongs there, wrapped like any other test:
//
//	require.NoError(t, testembed.CheckNoCollisions(append([]string{query}, docs...)...))
func CheckNoCollisions(texts ...string) error {
	groups := CollidingTokens(texts...)
	if len(groups) == 0 {
		return nil
	}
	parts := make([]string, len(groups))
	for i, group := range groups {
		parts[i] = fmt.Sprintf("bucket %d: %s", tokenIndexAt(group[0], Dimension), strings.Join(group, ", "))
	}
	return fmt.Errorf("testembed: colliding tokens at dimension %d: %s", Dimension, strings.Join(parts, "; "))
}
