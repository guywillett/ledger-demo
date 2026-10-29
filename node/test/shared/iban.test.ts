import { describe, expect, it } from 'vitest';
import { maskIban, parseIban, type Iban } from '../../src/shared/iban.ts';

// Which IBANs are accepted, and how they are normalised, is pinned by the shared vectors
// (contracts/vectors/iban.json, run in test/vectors.test.ts), including one registry example per
// SEPA country. What the vectors don't assert is the reason given for a rejection: a fixed text,
// because it ends up in 400 responses and logs.

describe('parseIban', () => {
  it.each([
    ['FR76-1234-5000', 'must be a country code, two check digits, then letters or digits'],
    ['US64SVBKUS6S3300958879', 'country is not in SEPA'],
    ['DE7551210800124512619', 'wrong length for its country'],
    ['DE99123456780000000077', 'check digits are out of range'],
    ['DE76512108001245126199', 'checksum is invalid'],
  ])('rejects %s with a fixed reason', (input, reason) => {
    expect(parseIban(input)).toEqual({ ok: false, reason });
  });

  it('never repeats the input in its reason', () => {
    const input = 'FR7612345000010000000000194';
    const result = parseIban(input);
    expect(result.ok).toBe(false);
    expect(JSON.stringify(result)).not.toContain('12345');
  });
});

describe('maskIban', () => {
  it('keeps the country, check digits and last four characters', () => {
    expect(maskIban('FR7612345000010000000000193' as Iban)).toBe('FR76…0193');
  });
});
