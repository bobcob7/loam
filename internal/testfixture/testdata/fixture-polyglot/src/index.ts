import { Validate } from "./validate";

/**
 * summarize renders a validation result for value, calling Validate.
 * Exercises cross-file reference resolution within the TypeScript fixture.
 */
export function summarize(value: string): string {
  return Validate(value) ? `ok: ${value}` : `invalid: ${value}`;
}
