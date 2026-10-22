import { describe, expect, it } from 'vitest';
import { maskIban, parseIban, type Iban } from '../../src/shared/iban.ts';

function reasonFor(input: string): string {
  const result = parseIban(input);
  if (result.ok) throw new Error(`expected ${input} to be rejected`);
  return result.reason;
}

describe('parseIban', () => {
  it.each([
    'FR7612345000010000000000193',
    'FR7612345000010000000000290',
    'FR7612345000010000000000387',
  ])('accepts seed IBAN %s', (iban) => {
    expect(parseIban(iban)).toEqual({ ok: true, iban });
  });

  it('normalises spaces and case', () => {
    expect(parseIban(' fr76 1234 5000 0100 0000 0000 193 ')).toEqual({
      ok: true,
      iban: 'FR7612345000010000000000193',
    });
  });

  // Length and example from the IBAN registry (via https://www.iban.com/structure, see the
  // source note in src/shared/iban.ts). A sample including non-EU SEPA countries and the
  // shortest and longest lengths.
  it.each([
    ['GB', 22, 'GB33BUKB20201555555555'],
    ['CH', 21, 'CH5604835012345678009'],
    ['NO', 15, 'NO8330001234567'],
    ['IS', 26, 'IS750001121234563108962099'],
    ['LI', 21, 'LI7408806123456789012'],
    ['MC', 27, 'MC5810096180790123456789085'],
    ['SM', 27, 'SM76P0854009812123456789123'],
    ['VA', 22, 'VA59001123000012345678'],
    ['AD', 24, 'AD1400080001001234567890'],
    ['GI', 23, 'GI56XAPO000001234567890'],
    ['MT', 31, 'MT31MALT01100000000000000000123'],
    ['BE', 16, 'BE71096123456769'],
    ['DE', 22, 'DE75512108001245126199'],
    ['IT', 27, 'IT60X0542811101000000123456'],
  ])('accepts the %s registry example (%i characters)', (country, length, example) => {
    expect(example.slice(0, 2)).toBe(country);
    expect(example).toHaveLength(length);
    expect(parseIban(example)).toEqual({ ok: true, iban: example });
    expect(reasonFor(example.slice(0, -1))).toBe('wrong length for its country');
  });

  it('rejects a wrong checksum', () => {
    expect(reasonFor('FR7612345000010000000000194')).toBe('checksum is invalid');
    expect(reasonFor('FR7712345000010000000000193')).toBe('checksum is invalid');
  });

  // Each pair has the same BBAN: the issued form (02, 97, 98) and the never-issued form (99,
  // 00, 01) that still passes mod-97.
  it.each([
    ['DE02123456780000000077', 'DE99123456780000000077'],
    ['DE97123456780000000016', 'DE00123456780000000016'],
    ['DE98123456780000000095', 'DE01123456780000000095'],
  ])('accepts %s but rejects its out-of-range twin %s', (issued, twin) => {
    expect(parseIban(issued)).toEqual({ ok: true, iban: issued });
    expect(reasonFor(twin)).toBe('check digits are out of range');
  });

  it('rejects countries outside SEPA', () => {
    expect(reasonFor('US64SVBKUS6S3300958879')).toBe('country is not in SEPA');
    expect(reasonFor('BR1800360305000010009795493C1')).toBe('country is not in SEPA');
  });

  it('rejects malformed input', () => {
    expect(reasonFor('')).toMatch(/country code/);
    expect(reasonFor('FR76-1234-5000')).toMatch(/country code/);
    expect(reasonFor('7612345000010000000000193')).toMatch(/country code/);
  });
});

describe('maskIban', () => {
  it('keeps the country, check digits and last four characters', () => {
    expect(maskIban('FR7612345000010000000000193' as Iban)).toBe('FR76…0193');
  });
});
