import { z } from 'zod';
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
