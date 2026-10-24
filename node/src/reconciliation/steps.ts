// Movement steps, compared the same way for payments and withdrawals: the actual steps
// (array_agg(... ORDER BY reference_step)) against the expected list, both in alphabetical order.
export function sameSteps(actual: readonly string[], expected: readonly string[]): boolean {
  return actual.length === expected.length && actual.every((step, i) => step === expected[i]);
}
