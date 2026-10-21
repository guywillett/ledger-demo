import { z } from 'zod';
import { fromTypeId, type IdPrefix, type Uuid } from './ids.ts';

// Event contracts, shared by producers and consumers. The language-neutral source is
// contracts/events/*.json; these Zod schemas must match them (tests check both).
// Events carry our IDs only (TypeIDs), never provider identifiers such as IBANs.

export const SUBJECTS = {
  paymentCreated: 'payment.created',
} as const;

// A TypeID on the wire, our UUID after parsing. The pattern matches the JSON contract;
// fromTypeId additionally checks that it holds a UUIDv7, which JSON Schema can't express, so
// on that one point the Zod schema is stricter than the contract.
const typeId = <P extends IdPrefix>(prefix: P): z.ZodType<Uuid<P>, string> =>
  z
    .string()
    .regex(new RegExp(`^${prefix}_[0-7][0-9a-hjkmnp-tv-z]{25}$`))
    .transform((value, ctx): Uuid<P> => {
      try {
        return fromTypeId(prefix, value);
      } catch {
        ctx.issues.push({ code: 'custom', message: `not a ${prefix} ID`, input: value });
        return z.NEVER;
      }
    });

// One schema, two types. z.input is the wire format a producer builds (strings, TypeIDs);
// z.output is what a consumer gets from parse() (our UUIDs, a BigInt amount, a Date).
//
// z.object (not strictObject): consumers ignore unknown fields, so a producer can add optional
// fields within a version.
export const paymentCreatedV1Schema = z.object({
  event_id: typeId('evt'),
  event_type: z.literal('payment.created'),
  schema_version: z.literal(1),
  payment_id: typeId('pay'),
  network: z.literal('SEPA'),
  network_transaction_id: z.string().min(1).max(255),
  account_id: typeId('acct'),
  amount_minor: z
    .string()
    .max(19)
    .regex(/^[1-9][0-9]*$/)
    .transform((value) => BigInt(value)),
  currency: z.literal('EUR'),
  occurred_at: z.iso.datetime().transform((value) => new Date(value)),
});

// What goes on the wire (producers build this).
export type PaymentCreatedV1Wire = z.input<typeof paymentCreatedV1Schema>;
// What a consumer works with after parsing.
export type PaymentCreatedV1 = z.output<typeof paymentCreatedV1Schema>;
