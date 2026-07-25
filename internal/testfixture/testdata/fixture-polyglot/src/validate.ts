/**
 * Validate reports whether the given value is a non-empty, trimmed string.
 *
 * This is the fixture's ambiguous symbol: a TypeScript export named Validate
 * that collides in name with pkg/validate/validate.go's Go export Validate,
 * proving that name-based edge resolution must not merge cross-language
 * hits.
 */
export function Validate(value: string): boolean {
  return value.trim().length > 0;
}
