import { z } from 'zod';
import { parseIban, type Iban } from './iban.ts';
import { fromTypeId, type IdPrefix, type Uuid } from './ids.ts';

// Zod schemas for values that appear on the wire in more than one place (HTTP requests and
// responses, events), so every boundary validates them the same way. Messages never repeat the
// input: they can end up in 400 responses and logs.
//
// Two points where these are stricter than the JSON contracts (contracts/), because JSON Schema
// can't express them: a TypeID must hold a UUIDv7, and an amount must fit Postgres bigint
// (int8). The contracts accept such values; these schemas reject them.

const INT8_MAX = 9223372036854775807n;

// A TypeID such as acct_01m5…, parsed into our UUID. The pattern matches the contracts'.
export const typeIdSchema = <P extends IdPrefix>(prefix: P): z.ZodType<Uuid<P>, string> =>
  z
    .string()
    .regex(new RegExp(`^${prefix}_[0-7][0-9a-hjkmnp-tv-z]{25}$`), `must be a ${prefix}_ ID`)
    .transform((value, ctx): Uuid<P> => {
      try {
        return fromTypeId(prefix, value);
      } catch {
        ctx.issues.push({ code: 'custom', message: `must be a ${prefix}_ ID`, input: value });
        return z.NEVER;
      }
    });

// A positive amount of minor units as a string of digits, parsed into a BigInt. No leading zero
// (so "0125" is not a second spelling of 125), and at most the int8 maximum.
export const amountMinorSchema = z
  .string()
  .max(19)
  .regex(/^[1-9][0-9]*$/, 'must be a positive whole number of minor units, as a string')
  .transform((value) => BigInt(value))
  .refine((value) => value <= INT8_MAX, 'is larger than the maximum amount');

// A SEPA IBAN, normalised (whitespace removed, upper case) and checked by parseIban. The reason
// in the message is one of parseIban's fixed texts, never the input.
export const ibanSchema = z
  .string()
  .max(64)
  .transform((value, ctx): Iban => {
    const result = parseIban(value);
    if (!result.ok) {
      ctx.issues.push({ code: 'custom', message: `invalid IBAN: ${result.reason}`, input: value });
      return z.NEVER;
    }
    return result.iban;
  });

// The EPC's SEPA basic Latin character set: a-z A-Z 0-9 / - ? : ( ) . , ' + and space. Mirrors
// the CHECKs on withdrawals.creditor_name and remittance_info (db/migrations/…_create_withdrawals
// .sql), which stay as the backstop: this schema gives the client a 400 naming the field
// instead of a 500 from the database. Slice 2 decision 32: text outside the set is rejected,
// never transliterated.
const SEPA_BASIC_TEXT = /^[A-Za-z0-9/?:().,'+ -]+$/;

// Required SEPA text of at most maxLength characters. Leading and trailing whitespace is trimmed
// first (it is almost always a slip), and the trimmed value is what is stored and returned, so
// the client still sees exactly what is sent. Whitespace inside is kept as it is.
export const sepaTextSchema = (maxLength: number): z.ZodType<string, string> =>
  z
    .string()
    .trim()
    // abort: a blank value gets this one message, not also the character-set one.
    .min(1, { error: 'must not be blank', abort: true })
    .max(maxLength, `must be at most ${maxLength} characters`)
    .regex(SEPA_BASIC_TEXT, "may only contain letters, digits, spaces and / - ? : ( ) . , ' +");

// Optional SEPA text: absent, null, empty or only whitespace all mean "none", stored as null
// (the one spelling of none; the database refuses ''). Otherwise as sepaTextSchema.
export const optionalSepaTextSchema = (
  maxLength: number,
): z.ZodType<string | null, string | null | undefined> =>
  z
    .string()
    .nullish()
    .transform((value) => value?.trim() || null)
    .pipe(sepaTextSchema(maxLength).nullable());
