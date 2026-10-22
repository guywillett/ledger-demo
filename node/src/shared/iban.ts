// IBANs as provider notifications identify accounts. Parse, don't validate: parseIban is the
// only way to get an Iban, so any function taking one is guaranteed a normalised, valid IBAN.
export type Iban = string & { readonly __brand: 'Iban' };

export type IbanResult = { ok: true; iban: Iban } | { ok: false; reason: string };

// IBAN length per IBAN country code, for the countries in the SEPA schemes. Keyed by the IBAN
// country code, not the territory: French overseas departments use FR, Åland uses FI, and
// Jersey, Guernsey and the Isle of Man use GB.
//
// Sources (fetched 2026-10-20):
// - Lengths: https://www.iban.com/structure ("data updated on 8 May 2026"), a reproduction of
//   the SWIFT IBAN Registry; swift.com refused automated access. Every length was cross-checked
//   against that page's example IBAN (length and mod-97), see test/shared/iban.test.ts.
// - Membership: NOT yet verified against the EPC's "List of SEPA Scheme Countries" (EPC409-09;
//   europeanpaymentscouncil.eu refused automated access). In particular, check the recently
//   admitted or pending members AL, MD, ME, MK and RS before relying on this list.
const SEPA_IBAN_LENGTHS: ReadonlyMap<string, number> = new Map([
  ['AD', 24],
  ['AL', 28],
  ['AT', 20],
  ['BE', 16],
  ['BG', 22],
  ['CH', 21],
  ['CY', 28],
  ['CZ', 24],
  ['DE', 22],
  ['DK', 18],
  ['EE', 20],
  ['ES', 24],
  ['FI', 18],
  ['FR', 27],
  ['GB', 22],
  ['GI', 23],
  ['GR', 27],
  ['HR', 21],
  ['HU', 28],
  ['IE', 22],
  ['IS', 26],
  ['IT', 27],
  ['LI', 21],
  ['LT', 20],
  ['LU', 20],
  ['LV', 21],
  ['MC', 27],
  ['MD', 24],
  ['ME', 22],
  ['MK', 19],
  ['MT', 31],
  ['NL', 18],
  ['NO', 15],
  ['PL', 28],
  ['PT', 25],
  ['RO', 24],
  ['RS', 22],
  ['SE', 24],
  ['SI', 19],
  ['SK', 24],
  ['SM', 27],
  ['VA', 22],
]);

// Normalises (whitespace removed, upper case), then checks format, the country's length, the
// check digit range and the ISO 13616 mod-97 checksum. Reasons are fixed texts that repeat nothing from the input:
// they end up in 400 responses and logs, which never contain request values.
export function parseIban(input: string): IbanResult {
  const value = input.replace(/\s+/g, '').toUpperCase();
  if (!/^[A-Z]{2}[0-9]{2}[A-Z0-9]+$/.test(value)) {
    return {
      ok: false,
      reason: 'must be a country code, two check digits, then letters or digits',
    };
  }
  const country = value.slice(0, 2);
  const length = SEPA_IBAN_LENGTHS.get(country);
  if (length === undefined) {
    return { ok: false, reason: 'country is not in SEPA' };
  }
  if (value.length !== length) {
    return { ok: false, reason: 'wrong length for its country' };
  }
  // Issued check digits are always 02–98. 00, 01 and 99 can still pass mod-97 (they are
  // congruent to 97, 98 and 02), so reject them explicitly.
  const checkDigits = value.slice(2, 4);
  if (checkDigits === '00' || checkDigits === '01' || checkDigits === '99') {
    return { ok: false, reason: 'check digits are out of range' };
  }
  if (mod97(value) !== 1n) {
    return { ok: false, reason: 'checksum is invalid' };
  }
  return { ok: true, iban: value as Iban };
}

// For logs: country, check digits and the last four characters only.
export function maskIban(iban: Iban): string {
  return `${iban.slice(0, 4)}…${iban.slice(-4)}`;
}

// Move the first four characters to the end, replace letters by numbers (A = 10 … Z = 35)
// and take the remainder mod 97; a valid IBAN gives 1. The number has up to ~70 digits,
// hence BigInt.
function mod97(iban: string): bigint {
  const rearranged = iban.slice(4) + iban.slice(0, 4);
  const digits = Array.from(rearranged, (char) => parseInt(char, 36).toString()).join('');
  return BigInt(digits) % 97n;
}
