// The separators a customer may put in an IBAN or around SEPA text (slice 3 decision 56): space,
// tab, CR, LF, no-break space (U+00A0) and narrow no-break space (U+202F), the last two because
// bank websites format IBANs with them. Defined explicitly, rather than as JavaScript's \s or
// trim(), so the Go implementation can implement exactly the same set from this sentence (the
// shared vectors, contracts/vectors/, pin it). Any other character, including every other
// Unicode space, is left in place, and the IBAN or text check then rejects it.

const SEPARATORS = new Set([' ', '\t', '\r', '\n', '\u00a0', '\u202f']);

function isSeparator(char: string): boolean {
  return SEPARATORS.has(char);
}

// Removes every separator (for IBANs). for…of walks the string by code point.
export function removeSeparators(value: string): string {
  let out = '';
  for (const char of value) {
    if (!isSeparator(char)) out += char;
  }
  return out;
}

// Removes separators from both ends only (for SEPA text). A scan from each end, not a regex like
// /[…]+$/, which backtracks quadratically on a long run of separators followed by anything else.
// Every separator is a single UTF-16 code unit, so indexing code units is safe.
export function trimSeparators(value: string): string {
  let start = 0;
  let end = value.length;
  while (start < end && isSeparator(value.charAt(start))) start++;
  while (end > start && isSeparator(value.charAt(end - 1))) end--;
  return value.slice(start, end);
}
