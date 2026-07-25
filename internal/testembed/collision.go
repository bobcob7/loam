package testembed

import (
	"fmt"
	"sort"
	"strings"
)

// CollidingTokens tokenizes texts exactly as Embed does (see tokenize) and
// reports every group of two or more distinct tokens that hash into the same
// bucket at Dimension. It returns nil when the vocabulary is collision-free.
//
// Collisions at a fixed Dimension are unavoidable in general (pigeonhole:
// once a vocabulary exceeds Dimension distinct tokens, some pair must
// collide), and this is not a defect in the FNV-1a projection — see the
// package doc and docs/testing-spec.md, "Deterministic embedder". What is
// avoidable is a collision going unnoticed: two fixture tokens landing in the
// same bucket can invert the ranking property the acceptance suite depends
// on ("the auth chunk ranks first for an auth query") and surface as a
// mysterious flake instead of a clear failure. Fixture authors and the
// acceptance harness should call this (or RequireNoCollisions) once against
// their full vocabulary at setup time, so a colliding pair is reported by
// name instead of discovered by a failing cosine comparison three layers
// away.
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

// RequireNoCollisions is the one-line convenience form of CollidingTokens: it
// returns nil if texts' vocabulary is collision-free at Dimension, or an
// error naming every colliding token group otherwise. It deliberately takes
// no testing.TB — fixture setup, godog step definitions, and the acceptance
// harness all need to call this and none of them have a *testing.T in scope
// — so callers that do have one wrap it themselves:
//
//	require.NoError(t, testembed.RequireNoCollisions(fixtureTexts...))
func RequireNoCollisions(texts ...string) error {
	groups := CollidingTokens(texts...)
	if len(groups) == 0 {
		return nil
	}
	parts := make([]string, len(groups))
	for i, group := range groups {
		parts[i] = strings.Join(group, ", ")
	}
	return fmt.Errorf("testembed: colliding tokens at dimension %d: %s", Dimension, strings.Join(parts, "; "))
}
