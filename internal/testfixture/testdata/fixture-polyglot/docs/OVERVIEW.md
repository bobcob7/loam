# Fixture Polyglot Overview

This fixture backs golden ingest tests: a known symbol graph across Go,
TypeScript, Python, and this document.

## Validation

Two languages each export a symbol named `Validate` -- `pkg/validate` in Go
and `src/validate.ts` in TypeScript -- deliberately colliding in name so
edge-resolution tests can prove approximate, name-based matching stays
intra-language and does not merge the two.

## Reporting

`pkg/report` (Go) and `src/index.ts` (TypeScript) each import their
language's `Validate` from a separate file, giving cross-file reference
resolution something concrete to find within a single language.

## Recursion Cycle

`scripts/parity.py` defines `is_even` and `is_odd`, two functions that call
each other. Resolving dependents from either one must terminate instead of
looping -- the cycle-safety case for the `dependents` recursive CTE.
